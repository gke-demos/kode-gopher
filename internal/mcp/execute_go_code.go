/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/creds"
	"github.com/gke-demos/kode-gopher/internal/executor"
	"github.com/gke-demos/kode-gopher/internal/normalize"
	"github.com/gke-demos/kode-gopher/internal/prompts"
	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

// ExecuteGoCodeArgs is the input schema for execute_go_code. The
// `jsonschema:` tag values become the per-field descriptions the LLM
// sees when picking arguments.
//
// Callers set EXACTLY ONE of Code (single-file snippet) or Files
// (multi-file snippet with optional helper subpackages).
type ExecuteGoCodeArgs struct {
	Code         string            `json:"code,omitempty" jsonschema:"Single-file Go source to ship into the sandbox. EITHER a snippet declaring 'func run(ctx context.Context) (any, error)' (the value run returns is JSON-marshaled and surfaced as result.value; the wrapper provides func main()), OR a complete 'package main' program (your code owns stdout/stderr; if you want a structured result write it to /app/.kode-gopher/result.json yourself). Mutually exclusive with 'files'."`
	Files        map[string]string `json:"files,omitempty" jsonschema:"Multi-file Go source. Keys are paths relative to /app (e.g. 'main.go', 'util.go', 'helper/mypkg.go'). Root files (no '/' in key) must share a package name; wrapped mode requires exactly one root file with 'func run(ctx context.Context) (any, error)'. Subdirectory files pass through unchanged as separate packages. If a key equals 'go.mod' the executor uses your version pins instead of the prewarm lockfile — you own version selection (and any recompile cost) in that case. Mutually exclusive with 'code'."`
	ExtraImports []string          `json:"extra_imports,omitempty" jsonschema:"Optional list of import paths to add via a generated blank-import companion file. Use to nudge go mod tidy when you know your snippet needs a package the prewarmed image has cached but you haven't declared the import in source yet."`
}

// ExecuteGoCodeOutput is the structured response. Clients that can
// consume MCP structured content get this typed; clients that only
// read text get the human-readable rendering in CallToolResult.Content.
type ExecuteGoCodeOutput struct {
	Phase      string   `json:"phase" jsonschema:"Where execution stopped: 'build' (the build failed: compiler errors, a go mod tidy failure such as an unknown module, or the build was killed by a timeout or out of memory; stderr says which) or 'run' (it built and ran)."`
	Mode       string   `json:"mode" jsonschema:"'wrapped' (a snippet with func run, wrapped by kode-gopher) or 'verbatim' (a full package main, run as is)."`
	ExitCode   int      `json:"exit_code" jsonschema:"Exit code of the phase that ran last: the build step's for phase=build, the program's for phase=run."`
	DurationMS int64    `json:"duration_ms" jsonschema:"Build plus run time in the sandbox, in milliseconds."`
	BuildMS    int64    `json:"build_ms" jsonschema:"Build time alone, in milliseconds."`
	Tidied     bool     `json:"tidied" jsonschema:"True when the build ran go mod tidy because the code imports a package outside the prewarmed lockfile (slower)."`
	Warnings   []string `json:"warnings,omitempty" jsonschema:"Notes for the caller: go mod tidy moved modules off the prewarmed versions (slow recompiles), or why the result is missing (too large, not valid JSON, or it couldn't be fetched)."`
	Stdout     string   `json:"stdout,omitempty" jsonschema:"Standard output of the phase that ran last. Long output keeps its first and last 8 KiB, with the middle elided."`
	Stderr     string   `json:"stderr,omitempty" jsonschema:"Standard error of the phase that ran last. Long output keeps its first and last 8 KiB, with the middle elided."`
	Result     *Result  `json:"result,omitempty" jsonschema:"The structured result, when the program wrote one: a snippet's wrapper writes it when run returns or panics; a full program only if it wrote /app/.kode-gopher/result.json. Absent if the process exited or was killed first (os.Exit, log.Fatal, a timeout), or if the result was dropped (see warnings)."`
}

// Result is the wire-form discriminated payload sent on
// ExecuteGoCodeOutput. Value is declared as `any` (rather than the
// executor's `json.RawMessage`) so the SDK's schema generator doesn't
// describe it as an array of bytes — the actual value can be any
// JSON-shaped thing the user returned from run().
type Result struct {
	Kind    string `json:"kind" jsonschema:"'ok' (run returned a value), 'error' (run returned an error), 'panic' (run panicked), or 'marshal_error' (the returned value couldn't be encoded as JSON)."`
	Value   any    `json:"value,omitempty" jsonschema:"For kind=ok: the value run returned, as JSON."`
	Message string `json:"message,omitempty" jsonschema:"For kind=error, panic or marshal_error: the error or panic message."`
	Stack   string `json:"stack,omitempty" jsonschema:"For kind=panic: the goroutine stack."`
	Type    string `json:"type,omitempty" jsonschema:"For kind=marshal_error: the Go type of the value that failed to encode."`
}

// toWireResult converts the executor's raw-bytes-preserving Result to
// the MCP wire form. Decodes Value from its JSON bytes so the SDK can
// serialize it as a normal JSON value (not a byte array).
func toWireResult(r *executor.Result) *Result {
	if r == nil {
		return nil
	}
	out := &Result{
		Kind:    r.Kind,
		Message: r.Message,
		Stack:   r.Stack,
		Type:    r.Type,
	}
	if len(r.Value) > 0 {
		var v any
		if err := json.Unmarshal(r.Value, &v); err != nil {
			// Preserve the raw bytes as a string so the LLM at
			// least sees something. Shouldn't happen — the wrapper
			// only writes well-formed JSON — but defensive.
			out.Value = string(r.Value)
		} else {
			out.Value = v
		}
	}
	return out
}

func (s *Server) handleExecuteGoCode(ctx context.Context, req *sdk.CallToolRequest, args ExecuteGoCodeArgs) (*sdk.CallToolResult, *ExecuteGoCodeOutput, error) {
	start := time.Now()

	// Exactly one of code/files must be set.
	hasCode := strings.TrimSpace(args.Code) != ""
	hasFiles := len(args.Files) > 0
	if hasCode && hasFiles {
		return toolError("provide either `code` (single-file) or `files` (multi-file), not both"), nil, nil
	}
	if !hasCode && !hasFiles {
		return toolError("either `code` or `files` is required"), nil, nil
	}

	inputFiles := map[string][]byte{}
	if hasCode {
		inputFiles["main.go"] = []byte(args.Code)
	} else {
		for k, v := range args.Files {
			inputFiles[k] = []byte(v)
		}
	}

	norm, err := normalize.Normalize(inputFiles, normalize.Options{
		ExtraImports: args.ExtraImports,
	})
	if err != nil {
		return toolError(fmt.Sprintf("normalize: %v", err)), nil, nil
	}
	log.Printf("execute_go_code: mode=%s files=%v extra_imports=%d", norm.Mode, fileKeys(norm.Files), len(args.ExtraImports))

	// Build the full file set: normalize output + forwarded credentials.
	// go.mod is bootstrapped from the sandbox image's prewarm lockfile
	// by internal/executor at tidy time — not synthesized here.
	files := map[string][]byte{}
	for k, v := range norm.Files {
		files[k] = v
	}
	envs := map[string]string{}
	var runCreds *sandbox.Credentials
	if src := s.credentialsFor(req); src != nil {
		credFiles, credEnv, cErr := src.Materialize(ctx)
		if cErr != nil {
			return toolError(fmt.Sprintf("materialize credentials: %v", cErr)), nil, nil
		}
		for k, v := range credFiles {
			files[k] = v
		}
		for k, v := range credEnv {
			envs[k] = v
		}
		if m, ok := src.(creds.TokenMinter); ok {
			tok, tErr := m.AccessToken(ctx)
			if tErr != nil {
				return toolError(fmt.Sprintf("credentials: %v", tErr)), nil, nil
			}
			runCreds = sandboxCredentials(tok)
		}
	}

	sl := s.slotFor(req)
	sess, err := s.ensureSession(ctx, sl)
	if err != nil {
		return toolError(err.Error()), nil, nil
	}

	// Serialize tool invocations: one Execute at a time on this
	// session, even if the SDK is dispatching us concurrently.
	sl.execMu.Lock()
	defer sl.execMu.Unlock()

	// runOnce runs Reset + Build/Run/Fetch on the given session. Split
	// out so the retry-on-dead-session path below can reuse it against
	// a fresh session without duplicating logic.
	runOnce := func(sess *sandbox.Session) (*executor.Outcome, error) {
		if rErr := sess.Reset(ctx); rErr != nil {
			return nil, fmt.Errorf("reset sandbox: %w", rErr)
		}
		return executor.Run(ctx, sess, executor.Request{
			Files:       files,
			Env:         envs,
			Credentials: runCreds,
			Timeout:     s.cfg.ExecTimeout,
		})
	}

	outcome, err := runOnce(sess)
	if err != nil && errors.Is(err, sandbox.ErrSessionDead) && ctx.Err() == nil {
		// Session went away (pod evicted, port-forward dropped, ...).
		// Close what we have, drop it from the MCP session's slot,
		// open a fresh one, and try the whole (reset + build + run)
		// dance once more. On retry failure return the ORIGINAL error
		// so diagnostics point at the real cause, not the retry symptom.
		origErr := err
		log.Printf("execute_go_code: session dead, closing and retrying once: %v", err)
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 30*time.Second)
		s.dropSession(closeCtx, sl, sess)
		cancelClose()
		if sess2, e := s.ensureSession(ctx, sl); e == nil {
			if o2, e2 := runOnce(sess2); e2 == nil {
				outcome, err = o2, nil
			} else {
				log.Printf("execute_go_code: retry also failed: %v", e2)
				err = origErr
			}
		}
	}
	if err != nil {
		return toolError(fmt.Sprintf("execute: %v", err)), nil, nil
	}

	out := &ExecuteGoCodeOutput{
		Phase:      string(outcome.Phase),
		Mode:       norm.Mode.String(),
		ExitCode:   outcome.ExitCode,
		DurationMS: outcome.Duration.Milliseconds(),
		BuildMS:    outcome.BuildDuration.Milliseconds(),
		Tidied:     outcome.Tidied,
		Warnings:   outcome.Warnings,
		Stdout:     outcome.Stdout,
		Stderr:     outcome.Stderr,
		Result:     toWireResult(outcome.Result),
	}
	log.Printf("execute_go_code: done phase=%s exit=%d tidied=%v duration=%s (handler total=%s)",
		out.Phase, out.ExitCode, out.Tidied, outcome.Duration.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// IsError if the program crashed (non-zero exit) or the wrapper
	// reported a non-ok result. The LLM uses IsError to decide
	// whether to react.
	isErr := outcome.ExitCode != 0 || outcome.ResultError != "" || (outcome.Result != nil && outcome.Result.Kind != "ok")

	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: renderText(out)}},
		IsError: isErr,
	}, out, nil
}

// renderText produces a compact human-readable rendering suitable for
// MCP clients that ignore structured content.
func renderText(o *ExecuteGoCodeOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "phase=%s  mode=%s  exit=%d  tidied=%v  (%dms, build %dms)\n", o.Phase, o.Mode, o.ExitCode, o.Tidied, o.DurationMS, o.BuildMS)
	for _, w := range o.Warnings {
		b.WriteString("\nwarning: " + ensureNewline(w))
	}
	if o.Stdout != "" {
		b.WriteString("\n── stdout ──\n")
		b.WriteString(ensureNewline(o.Stdout))
	}
	if o.Stderr != "" {
		b.WriteString("\n── stderr ──\n")
		b.WriteString(ensureNewline(o.Stderr))
	}
	if o.Result != nil {
		b.WriteString("\n── result ──\n")
		enc := json.NewEncoder(&b)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(o.Result)
	}
	return b.String()
}

func ensureNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func fileKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func toolError(msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: msg}},
		IsError: true,
	}
}

// executeGoCodeDescription now lives in internal/prompts, generated
// from internal/curated.Packages via `make prompts`. Aliased here so
// server.go's tool registration doesn't need to import prompts.
var executeGoCodeDescription = prompts.ExecuteGoCodeDescription

// sandboxCredentials converts a minted token to sandbox-server's
// credentials field.
func sandboxCredentials(t creds.AccessToken) *sandbox.Credentials {
	return &sandbox.Credentials{
		AccessToken:  t.Token,
		Expiry:       t.Expiry,
		Email:        t.Email,
		Project:      t.Project,
		QuotaProject: t.QuotaProject,
	}
}

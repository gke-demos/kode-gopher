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
	Files        map[string]string `json:"files,omitempty" jsonschema:"Multi-file Go source. Keys are paths relative to /app (e.g. 'main.go', 'util.go', 'helper/mypkg.go'). Root files (no '/' in key) must share a package name; wrapped mode requires exactly one root file with 'func run(ctx context.Context) (any, error)'. Subdirectory files pass through unchanged as separate packages. Paths under 'persist' and '.kode-gopher' are reserved (read/write /app/persist from Go code at runtime). If a key equals 'go.mod' the executor uses your version pins instead of the prewarm lockfile — you own version selection (and any recompile cost) in that case. Mutually exclusive with 'code'."`
	ExtraImports []string          `json:"extra_imports,omitempty" jsonschema:"Optional list of import paths to add via a generated blank-import companion file. Use to nudge go mod tidy when you know your snippet needs a package the prewarmed image has cached but you haven't declared the import in source yet."`
	ReadOnly     bool              `json:"read_only,omitempty" jsonschema:"Optional: restrict this run's Google Cloud credentials to read-only OAuth scopes (cloud-platform.read-only, devstorage.read_only, bigquery.readonly, logging.read, monitoring.read, trace.readonly). Supported for --credentials=service, service-account or workload-identity-federation --credentials=access-token, and --read-only OAuth deployments; fails closed rather than forwarding full-scope credentials when the configured credential mode cannot restrict scopes."`
}

// ExecuteGoCodeOutput is the structured response. Clients that can
// consume MCP structured content get this typed; clients that only
// read text get the human-readable rendering in CallToolResult.Content.
type ExecuteGoCodeOutput struct {
	Phase       string           `json:"phase" jsonschema:"Where execution stopped: 'build' (the build failed: compiler errors, a go mod tidy failure such as an unknown module, or the build was killed by a timeout or out of memory; stderr says which) or 'run' (it built and ran)."`
	Mode        string           `json:"mode" jsonschema:"'wrapped' (a snippet with func run, wrapped by kode-gopher) or 'verbatim' (a full package main, run as is)."`
	ExitCode    int              `json:"exit_code" jsonschema:"Exit code of the phase that ran last: the build step's for phase=build, the program's for phase=run."`
	DurationMS  int64            `json:"duration_ms" jsonschema:"Build plus run time in the sandbox, in milliseconds."`
	BuildMS     int64            `json:"build_ms" jsonschema:"Build time alone, in milliseconds."`
	Tidied      bool             `json:"tidied" jsonschema:"True when the build ran go mod tidy because the code imports a package outside the prewarmed lockfile (slower)."`
	Warnings    []string         `json:"warnings,omitempty" jsonschema:"Notes for the caller: go mod tidy moved modules off the prewarmed versions (slow recompiles), why the result is missing (too large, not valid JSON, or it couldn't be fetched), or why an entry in /app/.kode-gopher/attachments/ was skipped."`
	Stdout      string           `json:"stdout,omitempty" jsonschema:"Standard output of the phase that ran last. Long output keeps its first and last 8 KiB, with the middle elided."`
	Stderr      string           `json:"stderr,omitempty" jsonschema:"Standard error of the phase that ran last. Long output keeps its first and last 8 KiB, with the middle elided."`
	Result      *Result          `json:"result,omitempty" jsonschema:"The structured result, when the program wrote one: a snippet's wrapper writes it when run returns or panics; a full program only if it wrote /app/.kode-gopher/result.json. Absent if the process exited or was killed first (os.Exit, log.Fatal, a timeout), or if the result was dropped (see warnings)."`
	Attachments []AttachmentMeta `json:"attachments,omitempty" jsonschema:"Metadata for validated image files written under /app/.kode-gopher/attachments/ (PNG, JPEG, or WebP; up to 8 images and 2 MiB total). Raw image bytes are returned as MCP ImageContent blocks alongside this structured output."`
}

// AttachmentMeta describes an image attachment returned as an MCP
// ImageContent block in CallToolResult.Content.
type AttachmentMeta struct {
	Name     string `json:"name" jsonschema:"File name under /app/.kode-gopher/attachments/."`
	MIMEType string `json:"mime_type" jsonschema:"Verified image MIME type: image/png, image/jpeg, or image/webp."`
	Bytes    int    `json:"bytes" jsonschema:"Raw image size in bytes."`
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

	inputFiles, errMsg := prepareInputFiles(args)
	if errMsg != "" {
		log.Printf("execute_go_code: failed stage=validate reason=invalid_args (handler total=%s)", time.Since(start).Round(time.Millisecond))
		s.cfg.Metrics.RecordExec(ctx, time.Since(start), "validate", "error", "unknown", false)
		return toolError(errMsg), nil, nil
	}

	norm, err := normalize.Normalize(inputFiles, normalize.Options{
		ExtraImports: args.ExtraImports,
	})
	if err != nil {
		return s.failExec(ctx, start, "normalize", "unknown", args, inputFiles, err, fmt.Sprintf("normalize: %v", err)), nil, nil
	}
	mode := norm.Mode.String()
	log.Printf("execute_go_code: mode=%s files=%v extra_imports=%d", norm.Mode, fileKeys(norm.Files), len(args.ExtraImports))

	execReq, cErr := s.buildExecRequest(ctx, req, args, norm)
	if cErr != nil {
		return s.failExec(ctx, start, "credentials", mode, args, inputFiles, cErr, fmt.Sprintf("credentials: %v", cErr)), nil, nil
	}

	outcome, stage, err := s.executeRequest(ctx, req, execReq)
	if err != nil {
		msg := err.Error()
		if stage == "execute" {
			msg = fmt.Sprintf("execute: %v", err)
		}
		return s.failExec(ctx, start, stage, mode, args, inputFiles, err, msg), nil, nil
	}

	res, out := s.finishExec(ctx, start, args, inputFiles, mode, outcome)
	return res, out, nil
}

func prepareInputFiles(args ExecuteGoCodeArgs) (map[string][]byte, string) {
	hasCode := strings.TrimSpace(args.Code) != ""
	hasFiles := len(args.Files) > 0
	if hasCode && hasFiles {
		return nil, "provide either `code` (single-file) or `files` (multi-file), not both"
	}
	if !hasCode && !hasFiles {
		return nil, "either `code` or `files` is required"
	}
	inputFiles := make(map[string][]byte, max(1, len(args.Files)))
	if hasCode {
		inputFiles["main.go"] = []byte(args.Code)
	} else {
		for k, v := range args.Files {
			inputFiles[k] = []byte(v)
		}
	}
	return inputFiles, ""
}

func (s *Server) buildExecRequest(ctx context.Context, req *sdk.CallToolRequest, args ExecuteGoCodeArgs, norm *normalize.Result) (executor.Request, error) {
	// Build the full file set: normalize output + forwarded credentials.
	// go.mod is bootstrapped from the sandbox image's prewarm lockfile
	// by internal/executor at tidy time — not synthesized here.
	files := make(map[string][]byte, len(norm.Files))
	for k, v := range norm.Files {
		files[k] = v
	}
	credFiles, envs, tok, err := creds.ForRun(ctx, s.credentialsFor(req), s.cfg.ReadOnly || args.ReadOnly)
	if err != nil {
		return executor.Request{}, err
	}
	for k, v := range credFiles {
		files[k] = v
	}
	var runCreds *sandbox.Credentials
	if tok != nil {
		runCreds = sandboxCredentials(*tok)
	}
	return executor.Request{
		Reset:       true,
		Files:       files,
		Env:         envs,
		Credentials: runCreds,
		Timeout:     s.cfg.ExecTimeout,
	}, nil
}

func (s *Server) executeRequest(ctx context.Context, req *sdk.CallToolRequest, execReq executor.Request) (*executor.Outcome, string, error) {
	if s.testRun != nil {
		outcome, err := s.testRun(ctx, execReq)
		return outcome, "execute", err
	}
	// Serialize tool invocations: one Execute at a time on this
	// session, even if the SDK is dispatching us concurrently.
	sl := s.slotFor(req)
	sl.execMu.Lock()
	defer sl.execMu.Unlock()

	sess, err := s.ensureSession(ctx, sl)
	if err != nil {
		return nil, "open_sandbox", err
	}
	outcome, err := s.runWithRetry(ctx, sl, sess, execReq)
	return outcome, "execute", err
}

func (s *Server) runWithRetry(ctx context.Context, sl *slot, sess *sandbox.Session, execReq executor.Request) (*executor.Outcome, error) {
	outcome, err := executor.Run(ctx, sess, execReq)
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
			if o2, e2 := executor.Run(ctx, sess2, execReq); e2 == nil {
				return o2, nil
			} else {
				log.Printf("execute_go_code: retry also failed: %v", e2)
				return nil, origErr
			}
		}
	}
	return outcome, err
}

func (s *Server) failExec(ctx context.Context, start time.Time, stage, mode string, args ExecuteGoCodeArgs, inputFiles map[string][]byte, err error, toolMsg string) *sdk.CallToolResult {
	elapsed := time.Since(start)
	reason := classifyExecErr(ctx, err)
	log.Printf("execute_go_code: failed stage=%s mode=%s reason=%s (handler total=%s)",
		stage, mode, reason, elapsed.Round(time.Millisecond))
	s.capturer.recordFailure(stage, mode, reason, elapsed, inputFiles, args.ExtraImports, err)
	s.cfg.Metrics.RecordExec(ctx, elapsed, stage, classifyFailureStatus(stage, reason), mode, false)
	return toolError(toolMsg)
}

func (s *Server) finishExec(ctx context.Context, start time.Time, args ExecuteGoCodeArgs, inputFiles map[string][]byte, mode string, outcome *executor.Outcome) (*sdk.CallToolResult, *ExecuteGoCodeOutput) {
	elapsed := time.Since(start)
	res, out := outcomeToToolResult(outcome, mode)
	resultKind := sanitizeResultKind(out.Result)
	log.Printf("execute_go_code: done phase=%s exit=%d result=%s tidied=%v build=%s attachments=%d duration=%s (handler total=%s)",
		out.Phase, out.ExitCode, resultKind, out.Tidied,
		outcome.BuildDuration.Round(time.Millisecond),
		len(out.Attachments),
		outcome.Duration.Round(time.Millisecond),
		elapsed.Round(time.Millisecond))
	s.capturer.recordOutcome(inputFiles, args.ExtraImports, out)

	execStatus := "ok"
	buildStatus := "ok"
	if out.Phase == "build" {
		execStatus = "build_error"
		buildStatus = "error"
	} else if res.IsError {
		execStatus = "run_error"
	}
	s.cfg.Metrics.RecordBuild(ctx, outcome.BuildDuration, buildStatus, out.Tidied)
	s.cfg.Metrics.RecordExec(ctx, elapsed, out.Phase, execStatus, mode, out.Tidied)
	return res, out
}

func sanitizeResultKind(r *Result) string {
	if r == nil || r.Kind == "" {
		return "none"
	}
	switch r.Kind {
	case "ok", "error", "panic", "marshal_error":
		return r.Kind
	default:
		return "custom"
	}
}

func classifyExecErr(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return "context_canceled"
	case errors.Is(err, sandbox.ErrSessionDead):
		return "session_dead"
	case err != nil && (strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout exceeded")):
		return "deadline_exceeded"
	default:
		return "error"
	}
}

func outcomeToToolResult(outcome *executor.Outcome, mode string) (*sdk.CallToolResult, *ExecuteGoCodeOutput) {
	var metas []AttachmentMeta
	if len(outcome.Attachments) > 0 {
		metas = make([]AttachmentMeta, len(outcome.Attachments))
		for i, a := range outcome.Attachments {
			metas[i] = AttachmentMeta{Name: a.Name, MIMEType: a.MIMEType, Bytes: len(a.Data)}
		}
	}
	out := &ExecuteGoCodeOutput{
		Phase:       string(outcome.Phase),
		Mode:        mode,
		ExitCode:    outcome.ExitCode,
		DurationMS:  outcome.Duration.Milliseconds(),
		BuildMS:     outcome.BuildDuration.Milliseconds(),
		Tidied:      outcome.Tidied,
		Warnings:    outcome.Warnings,
		Stdout:      outcome.Stdout,
		Stderr:      outcome.Stderr,
		Result:      toWireResult(outcome.Result),
		Attachments: metas,
	}

	// IsError if the program crashed (non-zero exit) or the wrapper
	// reported a non-ok result. The LLM uses IsError to decide
	// whether to react.
	isErr := outcome.ExitCode != 0 || outcome.ResultError != "" || (outcome.Result != nil && outcome.Result.Kind != "ok")

	content := make([]sdk.Content, 0, 1+len(outcome.Attachments))
	content = append(content, &sdk.TextContent{Text: renderText(out)})
	for _, a := range outcome.Attachments {
		content = append(content, &sdk.ImageContent{
			Data:     a.Data,
			MIMEType: a.MIMEType,
		})
	}
	return &sdk.CallToolResult{
		Content: content,
		IsError: isErr,
	}, out
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
		enc.SetEscapeHTML(false)
		_ = enc.Encode(o.Result)
	}
	if len(o.Attachments) > 0 {
		b.WriteString("\n── attachments ──\n")
		for _, a := range o.Attachments {
			fmt.Fprintf(&b, "%s (%s, %d bytes)\n", a.Name, a.MIMEType, a.Bytes)
		}
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
		Scopes:       t.Scopes,
	}
}

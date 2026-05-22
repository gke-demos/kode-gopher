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
	"fmt"
	"log"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/executor"
	"github.com/gke-demos/kode-gopher/internal/normalize"
)

// ExecuteGoCodeArgs is the input schema for execute_go_code. The
// `jsonschema:` tag values become the per-field descriptions the LLM
// sees when picking arguments.
type ExecuteGoCodeArgs struct {
	Code         string   `json:"code" jsonschema:"The Go source to ship into the sandbox. EITHER a snippet declaring 'func run(ctx context.Context) (any, error)' (the value run returns is JSON-marshaled and surfaced as result.value; the wrapper provides func main()), OR a complete 'package main' program (your code owns stdout/stderr; if you want a structured result write it to /app/.kode-gopher/result.json yourself)."`
	ExtraImports []string `json:"extra_imports,omitempty" jsonschema:"Optional list of import paths to add via a generated blank-import companion file. Use to nudge go mod tidy when you know your snippet needs a package the prewarmed image has cached but you haven't declared the import in source yet. Prewarmed set: cloud.google.com/go/{storage,bigquery,compute/apiv1,container/apiv1,secretmanager/apiv1} + google.golang.org/api/option."`
}

// ExecuteGoCodeOutput is the structured response. Clients that can
// consume MCP structured content get this typed; clients that only
// read text get the human-readable rendering in CallToolResult.Content.
type ExecuteGoCodeOutput struct {
	Phase      string  `json:"phase"`                  // "build" or "run"
	Mode       string  `json:"mode"`                   // "verbatim" or "wrapped"
	ExitCode   int     `json:"exit_code"`
	DurationMS int64   `json:"duration_ms"`
	Stdout     string  `json:"stdout,omitempty"`
	Stderr     string  `json:"stderr,omitempty"`
	Result     *Result `json:"result,omitempty"` // {kind: ok|error|panic|marshal_error, value?, message?, stack?, type?}
}

// Result is the wire-form discriminated payload sent on
// ExecuteGoCodeOutput. Value is declared as `any` (rather than the
// executor's `json.RawMessage`) so the SDK's schema generator doesn't
// describe it as an array of bytes — the actual value can be any
// JSON-shaped thing the user returned from run().
type Result struct {
	Kind    string `json:"kind"`
	Value   any    `json:"value,omitempty"`
	Message string `json:"message,omitempty"`
	Stack   string `json:"stack,omitempty"`
	Type    string `json:"type,omitempty"`
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

func (s *Server) handleExecuteGoCode(ctx context.Context, _ *sdk.CallToolRequest, args ExecuteGoCodeArgs) (*sdk.CallToolResult, *ExecuteGoCodeOutput, error) {
	start := time.Now()
	if strings.TrimSpace(args.Code) == "" {
		return toolError("`code` field is required and must not be empty"), nil, nil
	}

	norm, err := normalize.Normalize([]byte(args.Code), normalize.Options{
		ExtraImports: args.ExtraImports,
	})
	if err != nil {
		return toolError(fmt.Sprintf("normalize: %v", err)), nil, nil
	}
	log.Printf("execute_go_code: mode=%s files=%v extra_imports=%d", norm.Mode, fileKeys(norm.Files), len(args.ExtraImports))

	// Build the full file set: normalize output + synthesized go.mod
	// + forwarded credentials.
	files := map[string][]byte{
		"go.mod": []byte("module kode_gopher_user\n\ngo 1.26\n"),
	}
	for k, v := range norm.Files {
		files[k] = v
	}
	envs := map[string]string{}
	if s.cfg.Credentials != nil {
		credFiles, credEnv := s.cfg.Credentials()
		for k, v := range credFiles {
			files[k] = v
		}
		for k, v := range credEnv {
			envs[k] = v
		}
	}

	sess, err := s.ensureSession(ctx)
	if err != nil {
		return toolError(err.Error()), nil, nil
	}

	// Serialize tool invocations: one Execute at a time on this
	// session, even if the SDK is dispatching us concurrently.
	s.execMu.Lock()
	defer s.execMu.Unlock()

	// Reset /app between calls so one tool invocation's filesystem
	// state can't leak into the next. $GOCACHE / $GOMODCACHE survive.
	if rErr := sess.Reset(ctx); rErr != nil {
		return toolError(fmt.Sprintf("reset sandbox: %v", rErr)), nil, nil
	}

	outcome, err := executor.Run(ctx, sess, executor.Request{
		Files:   files,
		Env:     envs,
		Timeout: s.cfg.ExecTimeout,
	})
	if err != nil {
		return toolError(fmt.Sprintf("execute: %v", err)), nil, nil
	}

	out := &ExecuteGoCodeOutput{
		Phase:      string(outcome.Phase),
		Mode:       norm.Mode.String(),
		ExitCode:   outcome.ExitCode,
		DurationMS: outcome.Duration.Milliseconds(),
		Stdout:     outcome.Stdout,
		Stderr:     outcome.Stderr,
		Result:     toWireResult(outcome.Result),
	}
	log.Printf("execute_go_code: done phase=%s exit=%d duration=%s (handler total=%s)",
		out.Phase, out.ExitCode, outcome.Duration.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// IsError if the program crashed (non-zero exit) or the wrapper
	// reported a non-ok result. The LLM uses IsError to decide
	// whether to react.
	isErr := outcome.ExitCode != 0 || (outcome.Result != nil && outcome.Result.Kind != "ok")

	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: renderText(out)}},
		IsError: isErr,
	}, out, nil
}

// renderText produces a compact human-readable rendering suitable for
// MCP clients that ignore structured content.
func renderText(o *ExecuteGoCodeOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "phase=%s  mode=%s  exit=%d  (%dms)\n", o.Phase, o.Mode, o.ExitCode, o.DurationMS)
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

const executeGoCodeDescription = `Build and run Go code in a sandboxed Kubernetes pod (gVisor-isolated on GKE, runsc-free on local kind). The pod has the Go toolchain plus a prewarmed cache of the curated Google Cloud SDK packages:

  cloud.google.com/go/storage
  cloud.google.com/go/bigquery
  cloud.google.com/go/compute/apiv1
  cloud.google.com/go/container/apiv1
  cloud.google.com/go/secretmanager/apiv1
  google.golang.org/api/option

Anything else is fetched on demand via go mod tidy (slower; may hit the agent-sandbox ~60s per-call HTTP cap).

Two input modes for the 'code' field:

1) SNIPPET (recommended): a Go file declaring ANY package other than "main", containing a function with EXACTLY this signature:
     func run(ctx context.Context) (any, error)
   Your return value is JSON-marshaled and surfaced as result.value. Errors and panics are captured into result.kind=error/panic. The kode-gopher wrapper provides func main() — do not write one.

2) FULL PROGRAM: a complete "package main" file. Your code owns stdout/stderr. If you want a structured result, write JSON to /app/.kode-gopher/result.json before exiting.

The host forwards ambient Google Cloud credentials (gcloud Application Default Credentials, plus GOOGLE_CLOUD_PROJECT if set) into the sandbox, so cloud.google.com/go/* calls work without additional setup. ADC lands at /app/.kode-gopher/creds/adc.json and GOOGLE_APPLICATION_CREDENTIALS is set for the run phase.

State semantics: /app is RESET between tool calls (so one program's files can't leak into the next). $GOCACHE and $GOMODCACHE PERSIST (so repeated builds against the same imports are near-instant).

Response shape: {phase, mode, exit_code, duration_ms, stdout?, stderr?, result?}. phase=build means the program never ran (tidy or compile failed). phase=run means the program executed; exit_code tells you whether it succeeded, and result (when present) is the structured payload your snippet returned.

Snippet example:

  package kode_gopher_snippet

  import (
      "context"
      "errors"
      "os"

      "cloud.google.com/go/storage"
      "google.golang.org/api/iterator"
  )

  func run(ctx context.Context) (any, error) {
      project := os.Getenv("GOOGLE_CLOUD_PROJECT")
      c, err := storage.NewClient(ctx)
      if err != nil { return nil, err }
      defer c.Close()
      var names []string
      it := c.Buckets(ctx, project)
      for {
          attrs, err := it.Next()
          if errors.Is(err, iterator.Done) { break }
          if err != nil { return nil, err }
          names = append(names, attrs.Name)
      }
      return names, nil
  }
`

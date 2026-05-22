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

// Package executor drives the host side of the build/run/fetch loop
// inside a sandbox.Session: ship Files, compile, execute the binary
// with the right env, and read back /app/.kode-gopher/result.json.
//
// All sandbox commands are kept under ~60s because the upstream
// agent-sandbox HTTP layer caps a single /execute call at that mark
// (see docs/decisions.md). Splitting tidy / build / run from each
// other also gives the caller unambiguous error attribution via the
// Outcome.Phase field.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

// Phase identifies the stage an Outcome reflects. PhaseBuild means we
// stopped before running the user program (compile / tidy failure).
// PhaseRun means we reached the user binary; its exit code is in
// Outcome.ExitCode and any structured result is in Outcome.Result.
type Phase string

const (
	PhaseBuild Phase = "build"
	PhaseRun   Phase = "run"
)

// Result is the discriminated payload the wrapper (or user code in
// verbatim mode) writes to /app/.kode-gopher/result.json. Kind takes
// one of: "ok", "error", "panic", "marshal_error".
type Result struct {
	Kind    string          `json:"kind"`
	Value   json.RawMessage `json:"value,omitempty"`
	Message string          `json:"message,omitempty"`
	Stack   string          `json:"stack,omitempty"`
	Type    string          `json:"type,omitempty"`
}

// Outcome is what one Run call produced.
type Outcome struct {
	Phase    Phase
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration // sum of all phases that ran
	Result   *Result       // nil if result.json was absent or unparseable
}

// Request is the input to Run.
type Request struct {
	// Files is the file set to materialize under /app before building.
	// At minimum should include main.go and go.mod.
	Files map[string][]byte
	// Env is a set of environment variables to apply to the user
	// binary at run time. Not applied to tidy or build (they have no
	// business seeing GCP creds).
	Env map[string]string
	// Timeout bounds each individual sandbox /execute call. Zero means
	// 90s. The agent-sandbox in-pod HTTP server caps at ~60s
	// regardless, so values larger than that only affect our own
	// internal accounting.
	Timeout time.Duration
}

const (
	binDir     = ".kode-gopher/bin"
	binName    = "run"
	binPath    = binDir + "/" + binName
	resultPath = ".kode-gopher/result.json"
)

// Run drives Build → Run → Fetch on sess. The session is reset to a
// clean state by the caller (or not — Run is happy to reuse cached
// build artifacts across invocations).
func Run(ctx context.Context, sess *sandbox.Session, req Request) (*Outcome, error) {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}

	// Phase 1a: tidy (downloads any missing modules).
	tidy, err := sess.Execute(ctx, sandbox.Request{
		Files:   req.Files,
		Command: "go mod tidy",
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("execute (tidy): %w", err)
	}
	if tidy.ExitCode != 0 {
		return &Outcome{
			Phase:    PhaseBuild,
			ExitCode: tidy.ExitCode,
			Stdout:   tidy.Stdout,
			Stderr:   tidy.Stderr,
			Duration: tidy.Duration,
		}, nil
	}

	// Phase 1b: build into a stable path.
	buildCmd := "mkdir -p " + binDir + " && go build -o " + binPath + " ."
	build, err := sess.Execute(ctx, sandbox.Request{
		Command: buildCmd,
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("execute (build): %w", err)
	}
	if build.ExitCode != 0 {
		return &Outcome{
			Phase:    PhaseBuild,
			ExitCode: build.ExitCode,
			Stdout:   build.Stdout,
			Stderr:   build.Stderr,
			Duration: tidy.Duration + build.Duration,
		}, nil
	}

	// Phase 2: run the binary with the requested env.
	runCmd := envPrefix(req.Env) + "./" + binPath
	run, err := sess.Execute(ctx, sandbox.Request{
		Command: runCmd,
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("execute (run): %w", err)
	}

	// Phase 3: fetch the structured result, if any. Cheap: cat one
	// small file. We discard fetch errors from the protocol — the
	// caller can tell "no result.json" from "non-nil Result" by the
	// Outcome.Result pointer.
	fetch, fetchErr := sess.Execute(ctx, sandbox.Request{
		Command: "cat " + resultPath + " 2>/dev/null || true",
		Timeout: 30 * time.Second,
	})
	var result *Result
	if fetchErr == nil {
		if body := strings.TrimSpace(fetch.Stdout); body != "" {
			var parsed Result
			if jErr := json.Unmarshal([]byte(body), &parsed); jErr == nil {
				result = &parsed
			}
		}
	}

	return &Outcome{
		Phase:    PhaseRun,
		ExitCode: run.ExitCode,
		Stdout:   run.Stdout,
		Stderr:   run.Stderr,
		Duration: tidy.Duration + build.Duration + run.Duration,
		Result:   result,
	}, nil
}

// envPrefix renders an env map as a deterministic shell prefix, e.g.
// `FOO='a' BAR='b' `. Returns "" if envs is empty.
func envPrefix(envs map[string]string) string {
	if len(envs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(shellQuote(envs[k]))
		b.WriteByte(' ')
	}
	return b.String()
}

// shellQuote wraps s for single-quoted use in a POSIX shell line.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

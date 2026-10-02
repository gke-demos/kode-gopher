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
// Keeping build (including any go mod tidy) and run in separate
// sandbox calls gives the caller unambiguous error attribution via the
// Outcome.Phase field. The upstream agent-sandbox HTTP layer's
// per-call cap is bounded by internal/sandbox.Options.PerAttemptTimeout
// (default 3min).
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
	Result   *Result       // nil if result.json was absent or unusable (see ResultError)
	// ResultError says why a result.json that exists couldn't be used:
	// too large, or not JSON. It's also among Warnings.
	ResultError string
	// Tidied reports whether the build had to run `go mod tidy`: the
	// snippet imported a package the lockfile didn't cover.
	Tidied bool
	// Warnings are notes for the caller (and the model) about how the
	// build went, e.g. tidy moving lockfile-pinned versions.
	Warnings []string
	// BuildDuration is the build phase alone (including any tidy).
	BuildDuration time.Duration
}

// Request is the input to Run.
type Request struct {
	// Files is the file set to materialize under /app before building.
	// At minimum should include main.go. go.mod / go.sum are optional:
	// if absent, the executor bootstraps them from the sandbox image's
	// preserved prewarm lockfile (baseGoModPath / baseGoSumPath) so
	// the snippet inherits the prewarm's pinned versions and hits the
	// $GOCACHE / $GOMODCACHE consistently.
	Files map[string][]byte
	// Env is a set of environment variables to apply to the user
	// binary at run time. Not applied to the build (it has no
	// business seeing GCP creds).
	Env map[string]string
	// Credentials, if set, is the access token the run phase executes
	// as (never the build or fetch). Needs an in-cluster session.
	Credentials *sandbox.Credentials
	// Timeout bounds each individual sandbox /execute call. Zero means
	// 90s. The upstream agent-sandbox HTTP cap is governed by
	// internal/sandbox.Options.PerAttemptTimeout (default 3min).
	Timeout time.Duration
}

const (
	binDir     = ".kode-gopher/bin"
	binName    = "run"
	binPath    = binDir + "/" + binName
	resultPath = ".kode-gopher/result.json"

	// maxResultBytes caps the structured result. Past this it would
	// swamp the model's context anyway; the program should return
	// less.
	maxResultBytes = 256 << 10

	// baseGoModPath / baseGoSumPath are where sandbox/Dockerfile
	// preserves the prewarm module's tidied lockfile. The executor
	// copies them into /app/{go.mod,go.sum} at build time (unless the
	// caller shipped their own go.mod, in which case theirs wins).
	baseGoModPath = "/opt/kode-gopher-base/go.mod"
	baseGoSumPath = "/opt/kode-gopher-base/go.sum"
)

// buildCmd is the whole build phase, in one sandbox round trip:
//
//  1. Unless the caller shipped a go.mod, bootstrap go.mod / go.sum
//     from the prewarm lockfile baked into the sandbox image (module
//     line rewritten to kode_gopher_user). Snippets then inherit the
//     exact versions the image's $GOCACHE was built from, so only the
//     snippet's own package compiles.
//  2. go build straight away. The default -mod=readonly means this
//     either builds with exactly the lockfile's versions or fails fast
//     because an import isn't covered. -s -w skips the symbol table
//     and DWARF, which saves ~1 s of linking under gVisor; panics still
//     get file:line stack traces.
//  3. Only on a missing-module failure: go mod tidy, report (with
//     markerMoved lines) any lockfile-pinned module whose version tidy
//     moved, and build again. A moved version means everything built
//     against it compiles from source: slow, and for client-go-scale
//     graphs more memory than the sandbox has.
//
// A sandbox image without the lockfile predates it and would cache-miss
// every build (~60 s+ on GKE), so name that instead of a bare cp error.
const buildCmd = `set -u
mkdir -p ` + binDir + `
bootstrapped=0
if [ ! -f go.mod ]; then
  if [ ! -f ` + baseGoModPath + ` ]; then
    echo "kode-gopher: sandbox image has no ` + baseGoModPath + `; it is older than this kode-gopher. Use the image pinned in manifests/overlays/gke (make sandbox-pin)." >&2
    exit 1
  fi
  cp ` + baseGoModPath + ` go.mod
  sed -i 's|^module .*|module kode_gopher_user|' go.mod
  cp ` + baseGoSumPath + ` go.sum
  chmod u+w go.mod go.sum
  bootstrapped=1
fi
build() { go build -ldflags='-s -w' -o ` + binPath + ` . ; }
if build 2>` + buildErrPath + `; then exit 0; fi
if ! grep -qE '` + missingModuleRE + `' ` + buildErrPath + `; then
  cat ` + buildErrPath + ` >&2
  exit 1
fi
echo "` + markerTidy + `"
go mod tidy || exit $?
if [ "$bootstrapped" = 1 ]; then
  awk -v m="` + markerMoved + `" '
    FNR == 1 { n++ }
    /^require \(/ { r = 1; next }
    r && /^\)/ { r = 0; next }
    /^require [^(]/ { c($2, $3); next }
    r && NF >= 2 { c($1, $2) }
    function c(p, v) { if (n == 1) b[p] = v; else if ((p in b) && b[p] != v) print m, p, b[p], v }
  ' ` + baseGoModPath + ` go.mod
fi
build
`

const (
	buildErrPath = ".kode-gopher/build.err"
	markerTidy   = "::kode-gopher:tidy::"
	markerMoved  = "::kode-gopher:moved::"
	// missingModuleRE matches go build's complaints about imports the
	// go.mod / go.sum don't cover, the cases tidy can fix.
	missingModuleRE = `no required module provides package|missing go\.sum entry|updates to go\.(mod|sum) needed|cannot find module providing package`
)

// Run drives Build → Run → Fetch on sess. The session is reset to a
// clean state by the caller (or not — Run is happy to reuse cached
// build artifacts across invocations).
func Run(ctx context.Context, sess *sandbox.Session, req Request) (*Outcome, error) {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}

	// Phase 1: build (with tidy only if the lockfile falls short).
	build, err := sess.Execute(ctx, sandbox.Request{
		Files:   req.Files,
		Command: buildCmd,
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("execute (build): %w", err)
	}
	stdout, tidied, moved := parseBuildStdout(build.Stdout)
	var warnings []string
	if len(moved) > 0 {
		warnings = append(warnings, movedWarning(moved))
	}
	if build.ExitCode != 0 {
		return &Outcome{
			Phase:    PhaseBuild,
			ExitCode: build.ExitCode,
			Stdout:   stdout,
			Stderr:   build.Stderr,
			Duration: build.Duration,
			Tidied:   tidied,
			Warnings: warnings,

			BuildDuration: build.Duration,
		}, nil
	}

	// Phase 2: run the binary with the requested env.
	runCmd := envPrefix(req.Env) + "./" + binPath
	run, err := sess.Execute(ctx, sandbox.Request{
		Command:     runCmd,
		Timeout:     timeout,
		Credentials: req.Credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("execute (run): %w", err)
	}

	// Phase 3: fetch the structured result, if any. Cheap: cat one
	// file, untruncated (Raw), since it's parsed rather than read.
	fetch, fetchErr := sess.Execute(ctx, sandbox.Request{
		Command: "cat " + resultPath + " 2>/dev/null || true",
		Timeout: 30 * time.Second,
		Raw:     true,
	})
	var result *Result
	var resultErr string
	if fetchErr != nil {
		resultErr = fmt.Sprintf("couldn't fetch the result: %v", fetchErr)
	} else {
		result, resultErr = parseResult(fetch.Stdout)
	}
	if resultErr != "" {
		warnings = append(warnings, resultErr)
	}

	return &Outcome{
		Phase:    PhaseRun,
		ExitCode: run.ExitCode,
		Stdout:   run.Stdout,
		Stderr:   run.Stderr,
		Duration: build.Duration + run.Duration,
		Result:   result,
		Tidied:   tidied,
		Warnings: warnings,

		ResultError:   resultErr,
		BuildDuration: build.Duration,
	}, nil
}

// parseResult decodes result.json's contents. An empty file means no
// result (a full program that didn't write one). Otherwise a result
// that can't be used comes back as a reason, never silently dropped.
func parseResult(body string) (*Result, string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, ""
	}
	if len(body) > maxResultBytes {
		return nil, fmt.Sprintf("the result is %d KiB, over the %d KiB limit, so it was dropped: return less data (filter, summarize or page through it in the program)",
			len(body)>>10, maxResultBytes>>10)
	}
	var r Result
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return nil, fmt.Sprintf("%s isn't valid JSON (%v); a full program must write {\"kind\": \"ok\", \"value\": ...}", resultPath, err)
	}
	return &r, ""
}

// parseBuildStdout strips buildCmd's marker lines from the build
// phase's stdout, reporting whether tidy ran and which modules it
// moved ("path old new").
func parseBuildStdout(s string) (stdout string, tidied bool, moved []string) {
	var keep []string
	for _, line := range strings.SplitAfter(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == markerTidy:
			tidied = true
		case strings.HasPrefix(trimmed, markerMoved+" "):
			moved = append(moved, strings.TrimPrefix(trimmed, markerMoved+" "))
		default:
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, ""), tidied, moved
}

// movedWarning explains a tidy that moved lockfile-pinned versions.
func movedWarning(moved []string) string {
	var b strings.Builder
	b.WriteString("go mod tidy moved modules off the sandbox's prewarmed versions:")
	for _, m := range moved {
		f := strings.Fields(m)
		if len(f) == 3 {
			fmt.Fprintf(&b, " %s %s -> %s;", f[0], f[1], f[2])
		} else {
			fmt.Fprintf(&b, " %s;", m)
		}
	}
	b.WriteString(" packages built against them compiled from source, which is slow and memory-hungry" +
		" (a client-go-sized rebuild can exceed the sandbox's memory)." +
		" Prefer curated packages, or drop the extra import that pulled these forward.")
	return b.String()
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

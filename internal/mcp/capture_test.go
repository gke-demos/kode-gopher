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
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gke-demos/kode-gopher/internal/creds"
	"github.com/gke-demos/kode-gopher/internal/executor"
	"github.com/gke-demos/kode-gopher/internal/telemetry"
)

type fakeCredSource struct{}

func (fakeCredSource) Materialize(context.Context) (map[string][]byte, map[string]string, error) {
	return map[string][]byte{
		".kode-gopher/creds/adc.json": []byte(`{"client_secret":"top-secret-adc"}`),
	}, map[string]string{"GOOGLE_CLOUD_PROJECT": "demo-proj"}, nil
}

func (fakeCredSource) Identity(context.Context) (creds.Identity, error) {
	return creds.Identity{Mode: "forwarded"}, nil
}

var _ creds.Source = fakeCredSource{}

func TestValidateCaptureConfig(t *testing.T) {
	if err := ValidateCaptureConfig("", ""); err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if err := ValidateCaptureConfig("", "failures"); err == nil {
		t.Fatal("expected error when --capture-mode is set without --capture-dir")
	}
	if err := ValidateCaptureConfig("/tmp/cap", "invalid"); err == nil {
		t.Fatal("expected error for invalid --capture-mode")
	}
	for _, m := range []string{"", CaptureModeFailures, CaptureModeAll} {
		if err := ValidateCaptureConfig("/tmp/cap", m); err != nil {
			t.Fatalf("ValidateCaptureConfig(/tmp/cap, %q): %v", m, err)
		}
	}
}

func TestExecuteGoCodeLoggingCaptureAndMetrics(t *testing.T) {
	ctx := context.Background()
	capDir := t.TempDir()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })
	rec := telemetry.NewRecorder(mp.Meter("test"))

	var nextOutcome *executor.Outcome
	var nextErr error
	srv := New(Config{
		CaptureDir:      capDir,
		CaptureMode:     CaptureModeFailures,
		CaptureMaxFiles: 10,
		Credentials:     fakeCredSource{},
		Metrics:         rec,
	})
	srv.testRun = func(_ context.Context, req executor.Request) (*executor.Outcome, error) {
		if _, ok := req.Files[".kode-gopher/creds/adc.json"]; !ok {
			t.Error("expected credential file in executor.Request.Files")
		}
		return nextOutcome, nextErr
	}

	validSnippet := "package snippet\nimport \"context\"\nfunc run(ctx context.Context) (any, error) { return \"sensitive-value\", nil }\n"

	// 1. Successful untidied run: not captured in failures mode.
	nextOutcome = &executor.Outcome{
		Phase:         executor.PhaseRun,
		ExitCode:      0,
		Duration:      120 * time.Millisecond,
		BuildDuration: 80 * time.Millisecond,
		Result:        &executor.Result{Kind: "ok", Value: json.RawMessage(`"sensitive-value"`)},
	}
	nextErr = nil
	res, _, err := srv.handleExecuteGoCode(ctx, nil, ExecuteGoCodeArgs{Code: validSnippet})
	if err != nil || res.IsError {
		t.Fatalf("step 1 unexpected error: err=%v isError=%v", err, res.IsError)
	}
	if got := readCaptures(t, capDir); len(got) != 0 {
		t.Fatalf("expected 0 captures after clean ok run, got %d", len(got))
	}

	// 2. Build failure: captured with status=build_error, no cred files.
	nextOutcome = &executor.Outcome{
		Phase:         executor.PhaseBuild,
		ExitCode:      1,
		Duration:      95 * time.Millisecond,
		BuildDuration: 95 * time.Millisecond,
		Stderr:        "./main.go:4:2: undefined: missingSymbol\n",
	}
	res, _, err = srv.handleExecuteGoCode(ctx, nil, ExecuteGoCodeArgs{
		Code:         validSnippet,
		ExtraImports: []string{"cloud.google.com/go/storage"},
	})
	if err != nil || !res.IsError {
		t.Fatalf("step 2 expected tool error result, err=%v isError=%v", err, res.IsError)
	}
	caps := readCaptures(t, capDir)
	if len(caps) != 1 {
		t.Fatalf("expected 1 capture after build failure, got %d", len(caps))
	}
	if caps[0].Status != "build_error" || caps[0].Phase != "build" || caps[0].ExitCode != 1 {
		t.Errorf("unexpected build_error capture: %+v", caps[0])
	}
	if !strings.Contains(caps[0].Stderr, "undefined: missingSymbol") {
		t.Errorf("capture missing compiler stderr: %q", caps[0].Stderr)
	}
	for k, v := range caps[0].Files {
		if strings.Contains(k, ".kode-gopher") || strings.Contains(v, "top-secret-adc") {
			t.Fatalf("credential leaked into capture file %q", k)
		}
	}

	// 3. Tidied success: captured in failures mode with status=tidied, omitting result.value.
	nextOutcome = &executor.Outcome{
		Phase:         executor.PhaseRun,
		ExitCode:      0,
		Tidied:        true,
		Duration:      4200 * time.Millisecond,
		BuildDuration: 3900 * time.Millisecond,
		Result:        &executor.Result{Kind: "ok", Value: json.RawMessage(`{"secret":"do-not-capture"}`)},
	}
	res, _, err = srv.handleExecuteGoCode(ctx, nil, ExecuteGoCodeArgs{Code: validSnippet})
	if err != nil || res.IsError {
		t.Fatalf("step 3 unexpected error: err=%v isError=%v", err, res.IsError)
	}
	caps = readCaptures(t, capDir)
	if len(caps) != 2 || caps[1].Status != "tidied" {
		t.Fatalf("expected second capture with status=tidied, got %+v", caps)
	}
	rawBytes, err := os.ReadFile(filepath.Join(capDir, listCaptureNames(t, capDir)[1]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawBytes, []byte("do-not-capture")) {
		t.Fatalf("result.value leaked into capture JSON:\n%s", string(rawBytes))
	}

	// 4. Timeout during execute: captured with status=timeout, stage=execute.
	nextOutcome = nil
	nextErr = context.DeadlineExceeded
	res, _, err = srv.handleExecuteGoCode(ctx, nil, ExecuteGoCodeArgs{Code: validSnippet})
	if err != nil || !res.IsError {
		t.Fatalf("step 4 expected tool error on timeout")
	}
	caps = readCaptures(t, capDir)
	if len(caps) != 3 || caps[2].Status != "timeout" || caps[2].Stage != "execute" {
		t.Fatalf("expected third capture with status=timeout stage=execute, got %+v", caps[len(caps)-1])
	}

	// 5. Normalize error: captured with status=normalize_error, stage=normalize.
	res, _, err = srv.handleExecuteGoCode(ctx, nil, ExecuteGoCodeArgs{Code: "not valid go code"})
	if err != nil || !res.IsError {
		t.Fatalf("step 5 expected tool error on normalize failure")
	}
	caps = readCaptures(t, capDir)
	if len(caps) != 4 || caps[3].Status != "normalize_error" || caps[3].Stage != "normalize" {
		t.Fatalf("expected fourth capture with status=normalize_error, got %+v", caps[len(caps)-1])
	}

	// 6. Forged Result.Kind in verbatim mode is sanitized to "custom".
	if got := sanitizeResultKind(&Result{Kind: "ok\nFORGED LOG LINE"}); got != "custom" {
		t.Errorf("sanitizeResultKind(forged) = %q, want %q", got, "custom")
	}

	// Verify OTel metrics were recorded.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
		}
	}
	if !seen["kode_gopher.exec.duration"] || !seen["kode_gopher.exec.build.duration"] {
		t.Errorf("expected exec and build duration metrics, got %v", seen)
	}
}

func TestCapturerPruningAndUTF8Truncation(t *testing.T) {
	dir := t.TempDir()
	// Unrelated file must never be deleted by pruneLocked.
	unrelated := filepath.Join(dir, "keep-me.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newCapturer(dir, CaptureModeAll, 3)
	for i := 0; i < 5; i++ {
		c.recordOutcome(map[string][]byte{"main.go": []byte("package main")}, nil, &ExecuteGoCodeOutput{
			Phase:    "run",
			Mode:     "verbatim",
			ExitCode: 0,
		})
	}
	names := listCaptureNames(t, dir)
	if len(names) != 3 {
		t.Fatalf("expected 3 retained capture files, got %d: %v", len(names), names)
	}
	if !strings.Contains(names[0], "-000003-") || !strings.Contains(names[2], "-000005-") {
		t.Errorf("expected captures 3..5 retained in order, got %v", names)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file was removed: %v", err)
	}

	// Verify UTF-8 boundary safety when truncating multibyte runes.
	s := strings.Repeat("a", 10) + "──" + strings.Repeat("b", 10)
	// "─" is 3 bytes in UTF-8; cutting at 11 or 12 lands inside the first rune.
	got := truncateCapturedString(s, 11)
	if !utf8.ValidString(got) {
		t.Errorf("truncateCapturedString produced invalid UTF-8: %q", got)
	}
}

func listCaptureNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), captureFilePrefix) && strings.HasSuffix(e.Name(), captureFileSuffix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func readCaptures(t *testing.T, dir string) []CaptureRecord {
	t.Helper()
	names := listCaptureNames(t, dir)
	out := make([]CaptureRecord, 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		var rec CaptureRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatalf("unmarshal %s: %v", n, err)
		}
		out = append(out, rec)
	}
	return out
}

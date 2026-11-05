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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postRunGoTest(t *testing.T, ctx context.Context, ts *httptest.Server, req runGoRequest) (int, runGoResponse, string) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/run-go", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	var out runGoResponse
	_ = json.Unmarshal(raw.Bytes(), &out)
	return resp.StatusCode, out, raw.String()
}

func TestRunGoEndToEndIsolatesBuildFromCredsAndEnv(t *testing.T) {
	ts, dir := newTestServer(t)

	// Seed stale artifacts, a 0000 directory, and a persist file that must survive reset.
	if err := os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked", "inner")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f.txt"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(locked, 0o000)
	_ = os.Chmod(filepath.Join(dir, "locked"), 0o000)
	if err := os.MkdirAll(filepath.Join(dir, "persist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "persist", "keep.txt"), []byte("saved"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o500)
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(filepath.Join(dir, "locked"), 0o755)
		_ = os.Chmod(locked, 0o755)
	})

	status, got, raw := postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        true,
		PreserveDirs: []string{"persist"},
		Files: map[string][]byte{
			"main.txt":        []byte("root-file"),
			"helper/util.txt": []byte("sub-file"),
		},
		// Build checks that neither USER_SECRET nor GCE_METADATA_HOST leaks into build.
		BuildCommand: `if [ -n "${USER_SECRET:-}" ] || [ -n "${GCE_METADATA_HOST:-}" ]; then echo "leaked to build" >&2; exit 9; fi; echo "::kode-gopher:tidy::"`,
		RunCommand: `mkdir -p .kode-gopher
printf 'meta=%s adc=%s secret=%s sub=%s kept=%s\n' "${GCE_METADATA_HOST:-}" "${GOOGLE_APPLICATION_CREDENTIALS:-}" "${USER_SECRET:-}" "$(cat helper/util.txt)" "$(cat persist/keep.txt)"
printf '{"kind":"ok","value":42}' > .kode-gopher/result.json`,
		Env: map[string]string{
			"USER_SECRET":                    "s3cr3t",
			"GCE_METADATA_HOST":              "evil.example:80", // must be overridden by Credentials emulator
			"GOOGLE_APPLICATION_CREDENTIALS": "/tmp/spoof.json", // must be stripped when Credentials != nil
		},
		Credentials: &credentials{
			AccessToken: "test-access-token",
			Expiry:      time.Now().Add(time.Hour),
			Email:       "u@example.com",
			Project:     "demo-proj",
		},
		ResultPath:     ".kode-gopher/result.json",
		MaxResultBytes: 256 << 10,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d raw=%s", status, raw)
	}
	if got.Phase != "run" || got.ExitCode != 0 {
		t.Fatalf("got phase=%q exit=%d stderr=%q", got.Phase, got.ExitCode, got.Stderr)
	}
	if !strings.Contains(got.BuildStdout, "::kode-gopher:tidy::") {
		t.Errorf("BuildStdout = %q, want tidy marker", got.BuildStdout)
	}
	if strings.Contains(got.Stdout, "evil.example") || !strings.Contains(got.Stdout, "meta=127.0.0.1:") {
		t.Errorf("Stdout = %q, want local metadata emulator overriding user GCE_METADATA_HOST", got.Stdout)
	}
	if !strings.Contains(got.Stdout, "adc= secret=s3cr3t sub=sub-file kept=saved") {
		t.Errorf("Stdout = %q", got.Stdout)
	}
	if got.Result != `{"kind":"ok","value":42}` {
		t.Errorf("Result = %q", got.Result)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt survived reset: %v", err)
	}

	// Reset=false must clear any pre-existing ResultPath before running so a
	// verbatim program that writes no result never inherits a stale result.json.
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:          false,
		BuildCommand:   "true",
		RunCommand:     "true",
		ResultPath:     ".kode-gopher/result.json",
		MaxResultBytes: 256 << 10,
	})
	if status != http.StatusOK || got.Result != "" {
		t.Fatalf("expected stale result.json cleared on Reset=false, got status=%d Result=%q raw=%s", status, got.Result, raw)
	}

	// PreserveDirs recovers when persist was chmod 0000'd or replaced by a symlink.
	_ = os.Chmod(filepath.Join(dir, "persist"), 0o000)
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        true,
		PreserveDirs: []string{"persist"},
		BuildCommand: "true",
		RunCommand:   "cat persist/keep.txt",
	})
	if status != http.StatusOK || strings.TrimSpace(got.Stdout) != "saved" {
		t.Fatalf("0000 persist recovery failed: status=%d stdout=%q raw=%s", status, got.Stdout, raw)
	}
	_ = os.RemoveAll(filepath.Join(dir, "persist"))
	outside := t.TempDir()
	_ = os.Symlink(outside, filepath.Join(dir, "persist"))
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        true,
		PreserveDirs: []string{"persist"},
		BuildCommand: "true",
		RunCommand:   "echo fresh > persist/new.txt",
	})
	if status != http.StatusOK || got.ExitCode != 0 {
		t.Fatalf("symlink persist replacement failed: status=%d got=%+v raw=%s", status, got, raw)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("persist symlink was followed outside workdir!")
	}
}

func TestRunGoBuildFailureSkipsRunPhase(t *testing.T) {
	ts, dir := newTestServer(t)
	status, got, raw := postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        true,
		Files:        map[string][]byte{"main.go": []byte("broken")},
		BuildCommand: `echo "syntax error" >&2; exit 2`,
		RunCommand:   `touch ran-anyway`,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d raw=%s", status, raw)
	}
	if got.Phase != "build" || got.ExitCode != 2 || !strings.Contains(got.Stderr, "syntax error") {
		t.Fatalf("got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran-anyway")); !os.IsNotExist(err) {
		t.Fatalf("run command executed despite build failure")
	}
}

func TestRunGoOversizeAndNonRegularResult(t *testing.T) {
	ts, _ := newTestServer(t)

	// 1. Oversize result omits body bytes while reporting ResultSize.
	status, got, raw := postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:          true,
		BuildCommand:   "true",
		RunCommand:     "mkdir -p .kode-gopher && head -c 5000 /dev/zero > .kode-gopher/result.json",
		ResultPath:     ".kode-gopher/result.json",
		MaxResultBytes: 1024,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d raw=%s", status, raw)
	}
	if got.Result != "" || got.ResultSize != 5000 {
		t.Fatalf("oversize: got Result len=%d ResultSize=%d, want empty Result and size 5000", len(got.Result), got.ResultSize)
	}

	// 2. FIFO at ResultPath must not hang sandbox-server.
	fifoPath := ".kode-gopher/result.json"
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:          true,
		BuildCommand:   "true",
		RunCommand:     "mkdir -p .kode-gopher && mkfifo " + fifoPath,
		ResultPath:     fifoPath,
		MaxResultBytes: 1024,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d raw=%s", status, raw)
	}
	if !strings.Contains(got.ResultReadErr, "not a regular file") {
		t.Fatalf("FIFO result: got ResultReadErr=%q, want 'not a regular file'", got.ResultReadErr)
	}
}

func TestRunGoRefusesSymlinkEscape(t *testing.T) {
	ts, dir := newTestServer(t)
	outside := t.TempDir()
	outsideSecret := filepath.Join(outside, "secret.json")
	if err := os.WriteFile(outsideSecret, []byte(`{"kind":"ok","value":"leaked"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-plant a symlink inside workdir pointing outside workdir.
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	// Writing through a symlink escaping workdir must fail (Reset=false).
	status, _, _ := postRunGoTest(t, context.Background(), ts, runGoRequest{
		BuildCommand: "true",
		RunCommand:   "true",
		Files:        map[string][]byte{"escape/pwn.txt": []byte("nope")},
	})
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500 when writing through escaping symlink, got %d", status)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatalf("escaped write landed outside workdir!")
	}

	// Reading ResultPath through a symlink (escaping or intra-workdir) must be refused.
	status, got, raw := postRunGoTest(t, context.Background(), ts, runGoRequest{
		BuildCommand:   "true",
		RunCommand:     "ln -sf " + outsideSecret + " .result-escape.json",
		ResultPath:     ".result-escape.json",
		MaxResultBytes: 1024,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d raw=%s", status, raw)
	}
	if got.Result != "" || !strings.Contains(got.ResultReadErr, "is a symlink") {
		t.Fatalf("expected escaping result symlink to fail, got Result=%q ResultReadErr=%q", got.Result, got.ResultReadErr)
	}

	if err := os.WriteFile(filepath.Join(dir, "inside.json"), []byte(`{"kind":"ok","value":"inside"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		BuildCommand:   "true",
		RunCommand:     "mkdir -p .kode-gopher && ln -sf ../inside.json .kode-gopher/result.json",
		ResultPath:     ".kode-gopher/result.json",
		MaxResultBytes: 1024,
	})
	if status != http.StatusOK || got.Result != "" || !strings.Contains(got.ResultReadErr, "is a symlink") {
		t.Fatalf("expected intra-workdir result symlink to fail, got status=%d Result=%q ResultReadErr=%q raw=%s",
			status, got.Result, got.ResultReadErr, raw)
	}
}

func TestRunGoHasAttachmentsDetectionAndCleanup(t *testing.T) {
	ts, dir := newTestServer(t)

	// 1. Empty attachments directory does not set HasAttachments.
	status, got, raw := postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        true,
		BuildCommand: "true",
		RunCommand:   "mkdir -p .kode-gopher/attachments",
	})
	if status != http.StatusOK || got.HasAttachments {
		t.Fatalf("empty attachments dir: status=%d HasAttachments=%v raw=%s", status, got.HasAttachments, raw)
	}

	// 2. Non-empty attachments directory sets HasAttachments=true.
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        false,
		BuildCommand: "true",
		RunCommand:   "mkdir -p .kode-gopher/attachments && printf 'img' > .kode-gopher/attachments/chart.png",
	})
	if status != http.StatusOK || !got.HasAttachments {
		t.Fatalf("populated attachments dir: status=%d HasAttachments=%v raw=%s", status, got.HasAttachments, raw)
	}

	// 3. Subsequent Reset=false run clears stale attachments even if chmod 0000'd.
	_ = os.Chmod(filepath.Join(dir, ".kode-gopher", "attachments"), 0o000)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, ".kode-gopher", "attachments"), 0o755) })
	status, got, raw = postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:        false,
		BuildCommand: "true",
		RunCommand:   "true",
	})
	if status != http.StatusOK || got.HasAttachments {
		t.Fatalf("stale attachments not cleared on Reset=false: status=%d HasAttachments=%v raw=%s", status, got.HasAttachments, raw)
	}
}

func TestRunGoPhaseTimeoutAndValidation(t *testing.T) {
	ts, dir := newTestServer(t)

	// Phase timeout kills child process group and returns TimedOut=true.
	status, got, raw := postRunGoTest(t, context.Background(), ts, runGoRequest{
		Reset:          true,
		BuildCommand:   "true",
		RunCommand:     "sleep 30",
		PhaseTimeoutMS: 200,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d raw=%s", status, raw)
	}
	if got.Phase != "run" || !got.TimedOut {
		t.Fatalf("timeout: got %+v", got)
	}

	// Validation failure returns 400 without resetting workdir.
	marker := filepath.Join(dir, "must-survive.txt")
	if err := os.WriteFile(marker, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []runGoRequest{
		{Reset: true, RunCommand: "true"},
		{Reset: true, BuildCommand: "true"},
		{Reset: true, BuildCommand: "true", RunCommand: "true", Files: map[string][]byte{"../escape.go": []byte("x")}},
		{Reset: true, BuildCommand: "true", RunCommand: "true", PreserveDirs: []string{"sub/dir"}},
		{Reset: true, BuildCommand: "true", RunCommand: "true", Env: map[string]string{"BAD-KEY": "1"}},
		{Reset: true, BuildCommand: "true", RunCommand: "true", Credentials: &credentials{AccessToken: "tok", Expiry: time.Now().Add(-time.Minute)}},
	} {
		code, _, body := postRunGoTest(t, context.Background(), ts, bad)
		if code != http.StatusBadRequest {
			t.Errorf("bad request %+v: got %d (%s), want 400", bad, code, body)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("invalid request mutated workdir: %v", err)
	}
}

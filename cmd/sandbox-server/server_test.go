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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := newServer(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts, s.workdir
}

// percentEncode is the agent-sandbox Go client's path encoding
// (clients/go/sandbox/files.go): everything outside RFC 3986's
// unreserved set is escaped, including "/".
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func get(t *testing.T, ts *httptest.Server, endpoint, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/" + endpoint + "/" + percentEncode(path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func execute(t *testing.T, ctx context.Context, ts *httptest.Server, command string) (executeResponse, error) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"command": command})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/execute", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return executeResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("execute %q: status %d: %s", command, resp.StatusCode, body)
	}
	var out executeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func upload(t *testing.T, ts *httptest.Server, field, filename string, content []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	w.Close()
	resp, err := http.Post(ts.URL+"/upload", w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestHealth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestExecute(t *testing.T) {
	ts, dir := newTestServer(t)
	ctx := context.Background()
	cases := []struct {
		command        string
		stdout, stderr string
		exitCode       int
	}{
		{command: "pwd", stdout: dir + "\n"},
		{command: "echo out; echo err >&2; exit 3", stdout: "out\n", stderr: "err\n", exitCode: 3},
		{command: "kill -TERM $$", exitCode: 128 + int(syscall.SIGTERM)},
		{command: "printf 'a\nb' | wc -l", stdout: "1\n"},
	}
	for _, c := range cases {
		got, err := execute(t, ctx, ts, c.command)
		if err != nil {
			t.Fatal(err)
		}
		want := executeResponse{Stdout: c.stdout, Stderr: c.stderr, ExitCode: c.exitCode}
		if strings.TrimSpace(got.Stdout) != strings.TrimSpace(want.Stdout) || got.Stderr != want.Stderr || got.ExitCode != want.ExitCode {
			t.Errorf("%q: got %+v, want %+v", c.command, got, want)
		}
	}
}

func TestExecuteBadRequests(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, body := range []string{`not json`, `{"command": ""}`, `{}`} {
		resp, err := http.Post(ts.URL+"/execute", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestExecuteCapsOutput(t *testing.T) {
	ts, _ := newTestServer(t)
	got, err := execute(t, context.Background(), ts, fmt.Sprintf("head -c %d /dev/zero", maxStreamBytes+1000))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.Stdout, "[truncated by sandbox-server]") || len(got.Stdout) > maxStreamBytes+100 {
		t.Fatalf("stdout len %d, suffix %q", len(got.Stdout), got.Stdout[max(0, len(got.Stdout)-40):])
	}
}

// A backgrounded child that inherits stdout must not hold the request
// open until it exits.
func TestExecuteBackgroundChildDoesNotHang(t *testing.T) {
	ts, _ := newTestServer(t)
	start := time.Now()
	got, err := execute(t, context.Background(), ts, "sleep 30 & echo started")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "started\n" || got.ExitCode != 0 {
		t.Fatalf("got %+v", got)
	}
	if d := time.Since(start); d > waitDelay+3*time.Second {
		t.Fatalf("took %v", d)
	}
}

// Cancelling the request kills the whole process group, not just sh.
func TestExecuteCancelKillsProcessGroup(t *testing.T) {
	ts, dir := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := execute(t, ctx, ts, "sleep 30 & echo $! > child.pid; wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still running after cancel", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// alive reports whether pid is running (not gone, not a zombie).
func alive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// Field 3, after the parenthesised command name, is the state.
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestUpload(t *testing.T) {
	ts, dir := newTestServer(t)
	for _, content := range []string{"first", "second, longer"} {
		status, body := upload(t, ts, "file", "main.go", []byte(content))
		if status != 200 {
			t.Fatalf("status %d: %s", status, body)
		}
		var resp struct {
			Filename string `json:"filename"`
			Size     int64  `json:"size"`
		}
		json.Unmarshal(body, &resp)
		if resp.Filename != "main.go" || resp.Size != int64(len(content)) {
			t.Errorf("response %s", body)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
		if string(got) != content {
			t.Errorf("file = %q, want %q", got, content)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("workdir has %d entries, want only main.go (temp file left behind?)", len(entries))
	}
}

func TestUploadRejects(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, c := range []struct{ field, name string }{
		{"file", ".."},
		{"file", ""},
		{"other", "main.go"},
	} {
		if status, body := upload(t, ts, c.field, c.name, []byte("x")); status != http.StatusBadRequest {
			t.Errorf("%s=%q: status %d, want 400: %s", c.field, c.name, status, body)
		}
	}
}

// multipart.Part.FileName reduces the client-supplied name to its
// base, so directory parts can never place a file outside the workdir.
func TestUploadStripsDirectories(t *testing.T) {
	ts, dir := newTestServer(t)
	for _, name := range []string{"sub/main.go", "../main.go", "/etc/main.go"} {
		if status, body := upload(t, ts, "file", name, []byte(name)); status != 200 {
			t.Fatalf("%q: status %d: %s", name, status, body)
		}
		got, err := os.ReadFile(filepath.Join(dir, "main.go"))
		if err != nil || string(got) != name {
			t.Errorf("%q: workdir/main.go = %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "main.go")); err == nil {
		t.Error("upload escaped the workdir")
	}
}

func TestDownload(t *testing.T) {
	ts, dir := newTestServer(t)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "f.txt"), []byte("nested"), 0o644)

	if status, body := get(t, ts, "download", "a/b/f.txt"); status != 200 || string(body) != "nested" {
		t.Errorf("nested: %d %q", status, body)
	}
	if status, _ := get(t, ts, "download", "/a/b/f.txt"); status != 200 {
		t.Errorf("leading slash: status %d", status)
	}
	if status, _ := get(t, ts, "download", "missing"); status != 404 {
		t.Errorf("missing: status %d", status)
	}
	if status, _ := get(t, ts, "download", "a"); status != 400 {
		t.Errorf("directory: status %d", status)
	}
}

func TestPathsStayInWorkdir(t *testing.T) {
	ts, dir := newTestServer(t)
	secret := filepath.Join(filepath.Dir(dir), "secret")
	os.WriteFile(secret, []byte("s3cret"), 0o644)
	t.Cleanup(func() { os.Remove(secret) })
	os.Symlink(secret, filepath.Join(dir, "link"))
	os.Symlink(filepath.Dir(dir), filepath.Join(dir, "linkdir"))

	for _, endpoint := range []string{"download", "list", "exists"} {
		for _, path := range []string{"../secret", "a/../../secret", "link", "linkdir/secret", "linkdir"} {
			status, body := get(t, ts, endpoint, path)
			if status != http.StatusForbidden {
				t.Errorf("%s %q: status %d, want 403: %s", endpoint, path, status, body)
			}
			if strings.Contains(string(body), "s3cret") {
				t.Errorf("%s %q leaked the file", endpoint, path)
			}
		}
	}
}

func TestList(t *testing.T) {
	ts, dir := newTestServer(t)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("12345"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "g.txt"), nil, 0o644)

	status, body := get(t, ts, "list", ".")
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var entries []listEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	got := map[string]listEntry{}
	for _, e := range entries {
		got[e.Name] = e
	}
	if len(got) != 2 || got["sub"].Type != "directory" || got["f.txt"].Type != "file" || got["f.txt"].Size != 5 || got["f.txt"].ModTime == 0 {
		t.Errorf("entries %s", body)
	}

	if status, body := get(t, ts, "list", "sub"); status != 200 || !strings.Contains(string(body), `"g.txt"`) {
		t.Errorf("sub: %d %s", status, body)
	}
	if status, _ := get(t, ts, "list", "f.txt"); status != 404 {
		t.Errorf("file: status %d, want 404", status)
	}
	if status, _ := get(t, ts, "list", "missing"); status != 404 {
		t.Errorf("missing: status %d, want 404", status)
	}
}

func TestExists(t *testing.T) {
	ts, dir := newTestServer(t)
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "f"), nil, 0o644)
	for path, want := range map[string]bool{"a": true, "a/f": true, "a/g": false, "nope/x": false} {
		status, body := get(t, ts, "exists", path)
		var resp struct {
			Exists bool `json:"exists"`
		}
		json.Unmarshal(body, &resp)
		if status != 200 || resp.Exists != want {
			t.Errorf("%q: %d %s, want exists=%v", path, status, body, want)
		}
	}
}

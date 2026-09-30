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
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// maxCommandBytes bounds the /execute request body.
	maxCommandBytes = 1 << 20
	// maxUploadBytes matches the agent-sandbox client's default
	// MaxUploadSize, plus headroom for the multipart framing.
	maxUploadBytes = 256<<20 + 1<<20
	// maxStreamBytes caps each of stdout and stderr. The client rejects
	// /execute responses over 16 MiB, so two capped streams (plus JSON
	// escaping) must stay under that. kode-gopher truncates far lower
	// (internal/sandbox); this is only the wire-level backstop.
	maxStreamBytes = 4 << 20
	// waitDelay is how long /execute waits for output pipes to close
	// after the shell exits, so a backgrounded child that keeps stdout
	// open can't hang the request.
	waitDelay = 2 * time.Second
)

type server struct {
	workdir string
	root    *os.Root
	log     *slog.Logger
}

func newServer(workdir string, log *slog.Logger) (*server, error) {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	// Probe writability so a misconfigured pod fails readiness loudly
	// instead of failing every upload.
	probe, err := os.CreateTemp(abs, ".write-probe-*")
	if err != nil {
		return nil, fmt.Errorf("workdir %s not writable: %w", abs, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &server{workdir: abs, root: root, log: log}, nil
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("POST /upload", s.handleUpload)
	mux.HandleFunc("GET /download/", s.handleDownload)
	mux.HandleFunc("GET /list/", s.handleList)
	mux.HandleFunc("GET /exists/", s.handleExists)
	return s.logRequests(mux)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type executeResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func (s *server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command     string       `json:"command"`
		Credentials *credentials `json:"credentials,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCommandBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command must not be empty")
		return
	}
	var env []string
	if req.Credentials != nil {
		if err := req.Credentials.validate(time.Now()); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		md, err := startMetadataEmulator(req.Credentials)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "starting metadata emulator: "+err.Error())
			return
		}
		defer md.Close()
		env = md.env()
	}
	start := time.Now()
	resp := runShell(r.Context(), s.workdir, req.Command, env)
	s.log.Info("exec",
		"command", preview(req.Command),
		"credentials", req.Credentials != nil,
		"exit", resp.ExitCode,
		"stdout_bytes", len(resp.Stdout),
		"stderr_bytes", len(resp.Stderr),
		"duration_ms", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, resp)
}

// runShell runs command via sh -c in dir, with env added to this
// process's environment. The shell gets its own process group so that
// cancelling ctx (the client went away) kills everything it started,
// e.g. a go build's compiler children, not just the shell. A shell
// killed by a signal reports 128+signal, as a shell would.
func runShell(ctx context.Context, dir, command string, env []string) executeResponse {
	var stdout, stderr cappedBuffer
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	code := 0
	switch {
	case cmd.ProcessState != nil:
		code = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	case err != nil:
		// sh never started.
		code = -1
		_, _ = stderr.Write([]byte(err.Error()))
	}
	return executeResponse{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// handleUpload streams the multipart "file" part to <workdir>/<name>.
// The clients send plain file names; multipart.Part.FileName reduces
// anything else to its base, so the file always lands directly in the
// workdir. It is written to a temp file and renamed into place, so a
// failed upload never leaves a truncated file behind.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart/form-data body: "+err.Error())
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, `missing "file" form field`)
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "reading multipart body: "+err.Error())
			return
		}
		if part.FormName() != "file" {
			continue
		}
		name := part.FileName()
		if name == "" || name == "." || name == ".." {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("filename %q must be a plain file name", name))
			return
		}
		n, err := s.writeFile(name, part)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, http.StatusRequestEntityTooLarge, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "writing "+name+": "+err.Error())
			return
		}
		s.log.Debug("upload", "filename", name, "size", n)
		writeJSON(w, http.StatusOK, map[string]any{"filename": name, "size": n})
		return
	}
}

func (s *server) writeFile(name string, src io.Reader) (int64, error) {
	tmp := fmt.Sprintf(".upload-%d-%s", time.Now().UnixNano(), name)
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, name)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
		return 0, err
	}
	return n, nil
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name, ok := s.pathParam(w, r, "/download/")
	if !ok {
		return
	}
	f, err := s.root.Open(name)
	if err != nil {
		writePathError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writePathError(w, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

type listEntry struct {
	Name    string  `json:"name"`
	Size    int64   `json:"size"`
	Type    string  `json:"type"`
	ModTime float64 `json:"mod_time"`
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	name, ok := s.pathParam(w, r, "/list/")
	if !ok {
		return
	}
	f, err := s.root.Open(name)
	if err != nil {
		writePathError(w, err)
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.IsDir() {
		writeError(w, http.StatusNotFound, "directory not found")
		return
	}
	dirents, err := f.ReadDir(-1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]listEntry, 0, len(dirents))
	for _, d := range dirents {
		info, err := d.Info()
		if err != nil {
			continue // removed since ReadDir
		}
		typ := "file"
		if info.IsDir() {
			typ = "directory"
		}
		out = append(out, listEntry{
			Name:    info.Name(),
			Size:    info.Size(),
			Type:    typ,
			ModTime: float64(info.ModTime().UnixNano()) / 1e9,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleExists(w http.ResponseWriter, r *http.Request) {
	name, ok := s.pathParam(w, r, "/exists/")
	if !ok {
		return
	}
	_, err := s.root.Stat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		writePathError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": name, "exists": err == nil})
}

// pathParam decodes the percent-encoded path after prefix into a name
// relative to the workdir. The clients encode "/" as %2F, so this reads
// the escaped path rather than r.URL.Path. A leading "/" is dropped and
// an empty path means the workdir itself. Names that lexically leave
// the workdir are refused here; s.root refuses symlinks that do.
func (s *server) pathParam(w http.ResponseWriter, r *http.Request, prefix string) (string, bool) {
	name, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), prefix))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid path encoding: "+err.Error())
		return "", false
	}
	name = strings.TrimLeft(name, "/")
	if name == "" {
		name = "."
	}
	if !filepath.IsLocal(name) {
		writeError(w, http.StatusForbidden, "access denied: path must be within "+s.workdir)
		return "", false
	}
	return name, true
}

// writePathError maps an os.Root error: missing is 404; anything else
// (a symlink out of the workdir, permissions) is 403.
func writePathError(w http.ResponseWriter, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusForbidden, "access denied: "+err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// preview is a one-line, length-capped command for log lines.
func preview(cmd string) string {
	s := strings.ReplaceAll(cmd, "\n", "⏎")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// cappedBuffer keeps the first maxStreamBytes written and discards the
// rest, marking the output as truncated.
type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := maxStreamBytes - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n... [truncated by sandbox-server]"
	}
	return c.buf.String()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if r.URL.Path == "/" {
			level = slog.LevelDebug // readiness probes
		}
		s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

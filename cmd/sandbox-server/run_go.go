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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// maxRunGoBytes bounds the JSON request body for POST /run-go.
	maxRunGoBytes  = 32 << 20
	attachmentsDir = ".kode-gopher/attachments"
)

var runGoTmpSeq atomic.Uint64

type runGoRequest struct {
	Reset          bool              `json:"reset,omitempty"`
	PreserveDirs   []string          `json:"preserve_dirs,omitempty"`
	Files          map[string][]byte `json:"files,omitempty"`
	BuildCommand   string            `json:"build_command"`
	RunCommand     string            `json:"run_command"`
	Env            map[string]string `json:"env,omitempty"`
	Credentials    *credentials      `json:"credentials,omitempty"`
	ResultPath     string            `json:"result_path,omitempty"`
	MaxResultBytes int64             `json:"max_result_bytes,omitempty"`
	PhaseTimeoutMS int64             `json:"phase_timeout_ms,omitempty"`
}

type runGoResponse struct {
	Phase          string `json:"phase"`
	ExitCode       int    `json:"exit_code"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	BuildStdout    string `json:"build_stdout,omitempty"`
	BuildMS        int64  `json:"build_ms"`
	RunMS          int64  `json:"run_ms,omitempty"`
	TimedOut       bool   `json:"timed_out,omitempty"`
	Result         string `json:"result,omitempty"`
	ResultSize     int64  `json:"result_size,omitempty"`
	ResultReadErr  string `json:"result_read_err,omitempty"`
	HasAttachments bool   `json:"has_attachments,omitempty"`
}

func (s *server) handleRunGo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRunGoBytes)
	var req runGoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := validateRunGoRequest(&req, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Reset {
		if err := s.resetWorkdir(req.PreserveDirs); err != nil {
			writeError(w, http.StatusInternalServerError, "resetting workdir: "+err.Error())
			return
		}
	}
	if len(req.Files) > 0 {
		if err := s.writeFiles(req.Files); err != nil {
			writeError(w, http.StatusInternalServerError, "writing files: "+err.Error())
			return
		}
	}

	resp, err := s.executeRunGoPhases(r.Context(), &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("run-go",
		"reset", req.Reset,
		"files", len(req.Files),
		"credentials", req.Credentials != nil,
		"phase", resp.Phase,
		"exit", resp.ExitCode,
		"timed_out", resp.TimedOut,
		"build_ms", resp.BuildMS,
		"run_ms", resp.RunMS,
		"result_size", resp.ResultSize)
	writeJSON(w, http.StatusOK, resp)
}

// maxBuildMarkerBytes caps build_stdout returned alongside a run-phase
// response. On a successful build, buildCmd emits only tidy/moved marker
// lines on stdout.
const maxBuildMarkerBytes = 64 << 10

func (s *server) executeRunGoPhases(ctx context.Context, req *runGoRequest) (*runGoResponse, error) {
	buildResp, buildMS, buildTimedOut := runPhase(ctx, req.PhaseTimeoutMS, s.workdir, req.BuildCommand, nil)
	if ctx.Err() != nil {
		return &runGoResponse{
			Phase:    "build",
			ExitCode: buildResp.ExitCode,
			Stdout:   buildResp.Stdout,
			Stderr:   buildResp.Stderr,
			BuildMS:  buildMS,
		}, nil
	}
	if buildTimedOut || buildResp.ExitCode != 0 {
		return &runGoResponse{
			Phase:    "build",
			ExitCode: buildResp.ExitCode,
			Stdout:   buildResp.Stdout,
			Stderr:   buildResp.Stderr,
			BuildMS:  buildMS,
			TimedOut: buildTimedOut,
		}, nil
	}

	s.clearRunOutputs(req.ResultPath)
	runEnv := buildRunEnv(req.Env, req.Credentials != nil)
	if req.Credentials != nil {
		md, err := startMetadataEmulator(req.Credentials)
		if err != nil {
			return nil, fmt.Errorf("starting metadata emulator: %w", err)
		}
		defer func() { _ = md.Close() }()
		runEnv = append(runEnv, md.env()...)
	}

	buildMarkers := buildResp.Stdout
	if len(buildMarkers) > maxBuildMarkerBytes {
		buildMarkers = buildMarkers[:maxBuildMarkerBytes]
	}
	runResp, runMS, runTimedOut := runPhase(ctx, req.PhaseTimeoutMS, s.workdir, req.RunCommand, runEnv)
	out := &runGoResponse{
		Phase:       "run",
		ExitCode:    runResp.ExitCode,
		Stdout:      runResp.Stdout,
		Stderr:      runResp.Stderr,
		BuildStdout: buildMarkers,
		BuildMS:     buildMS,
		RunMS:       runMS,
		TimedOut:    runTimedOut,
	}
	if !runTimedOut && ctx.Err() == nil {
		out.HasAttachments = s.hasAttachmentsEntry()
		if req.ResultPath != "" {
			out.Result, out.ResultSize, out.ResultReadErr = s.readResult(req.ResultPath, req.MaxResultBytes)
		}
	}
	return out, nil
}

func runPhase(parent context.Context, timeoutMS int64, dir, command string, env []string) (executeResponse, int64, bool) {
	ctx := parent
	var cancel context.CancelFunc
	if timeoutMS > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
		defer cancel()
	}
	start := time.Now()
	resp := runShell(ctx, dir, command, env)
	ms := time.Since(start).Milliseconds()
	timedOut := timeoutMS > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil
	return resp, ms, timedOut
}

func validateRunGoRequest(req *runGoRequest, now time.Time) error {
	if req.BuildCommand == "" {
		return errors.New("build_command must not be empty")
	}
	if req.RunCommand == "" {
		return errors.New("run_command must not be empty")
	}
	for _, d := range req.PreserveDirs {
		if !isSingleDirName(d) {
			return fmt.Errorf("preserve_dirs entry %q must be a single local directory name", d)
		}
	}
	for k := range req.Files {
		if !isValidRelPath(k) {
			return fmt.Errorf("file path %q must be a clean relative path within workdir", k)
		}
	}
	if req.ResultPath != "" && !isValidRelPath(req.ResultPath) {
		return fmt.Errorf("result_path %q must be a clean relative path within workdir", req.ResultPath)
	}
	for k, v := range req.Env {
		if !isValidEnvKey(k) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid env variable %q", k)
		}
	}
	if req.Credentials != nil {
		if err := req.Credentials.validate(now); err != nil {
			return err
		}
	}
	return nil
}

func isSingleDirName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00") && filepath.IsLocal(s)
}

func isValidRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || !filepath.IsLocal(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func buildRunEnv(user map[string]string, hasCreds bool) []string {
	if len(user) == 0 {
		return nil
	}
	keys := make([]string, 0, len(user))
	for k := range user {
		if hasCreds && isReservedCredEnv(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+user[k])
	}
	return out
}

func isReservedCredEnv(k string) bool {
	switch k {
	case "GCE_METADATA_HOST", "GCE_METADATA_IP", "GCE_METADATA_ROOT", "NO_GCE_CHECK", "GOOGLE_APPLICATION_CREDENTIALS":
		return true
	default:
		return false
	}
}

func (s *server) resetWorkdir(preserve []string) error {
	if err := os.Chmod(s.workdir, 0o755); err != nil {
		return err
	}
	keep := make(map[string]bool, len(preserve))
	for _, p := range preserve {
		keep[p] = true
	}
	dirFile, err := s.root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if readErr != nil {
		return readErr
	}
	for _, e := range entries {
		name := e.Name()
		if keep[name] {
			info, err := s.root.Lstat(name)
			if err == nil && !info.IsDir() {
				if err := s.root.RemoveAll(name); err != nil {
					return err
				}
			}
			continue
		}
		unlockDirsInRoot(s.root, name)
		if err := s.root.RemoveAll(name); err != nil {
			return err
		}
	}
	for _, p := range preserve {
		if err := s.root.MkdirAll(p, 0o755); err != nil {
			return err
		}
		if err := s.root.Chmod(p, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func unlockDirsInRoot(root *os.Root, rel string) {
	info, err := root.Lstat(rel)
	if err != nil || !info.IsDir() {
		return
	}
	if info.Mode().Perm()&0o700 != 0o700 {
		_ = root.Chmod(rel, 0o700)
	}
	f, err := root.Open(rel)
	if err != nil {
		return
	}
	children, _ := f.ReadDir(-1)
	_ = f.Close()
	for _, c := range children {
		unlockDirsInRoot(root, rel+"/"+c.Name())
	}
}

func (s *server) writeFiles(files map[string][]byte) error {
	if err := os.Chmod(s.workdir, 0o755); err != nil {
		return err
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		dir := path.Dir(rel)
		if dir != "." {
			if err := s.root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}
		if err := s.writeAtomicRel(rel, files[rel]); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

func (s *server) writeAtomicRel(rel string, data []byte) error {
	tmp := fmt.Sprintf(".run-go-tmp-%d-%d", time.Now().UnixNano(), runGoTmpSeq.Add(1))
	if dir := path.Dir(rel); dir != "." {
		tmp = dir + "/" + tmp
	}
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, rel)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
		return err
	}
	return nil
}

func (s *server) clearRunOutputs(resultPath string) {
	if resultPath != "" {
		_ = s.root.RemoveAll(resultPath)
	}
	unlockDirsInRoot(s.root, attachmentsDir)
	_ = s.root.RemoveAll(attachmentsDir)
}

func (s *server) hasAttachmentsEntry() bool {
	if symlinkComponent(s.root, ".kode-gopher") != "" {
		return false
	}
	info, err := s.root.Lstat(attachmentsDir)
	if err != nil {
		return false
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return info.Mode()&fs.ModeSymlink != 0
	}
	f, err := s.root.Open(attachmentsDir)
	if err != nil {
		return true
	}
	entries, err := f.ReadDir(1)
	_ = f.Close()
	return (err != nil && !errors.Is(err, io.EOF)) || len(entries) > 0
}

func symlinkComponent(root *os.Root, relPath string) string {
	cur := ""
	for _, p := range strings.Split(relPath, "/") {
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		info, err := root.Lstat(cur)
		if err != nil {
			return ""
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return cur
		}
	}
	return ""
}

func (s *server) readResult(relPath string, maxBytes int64) (string, int64, string) {
	if sym := symlinkComponent(s.root, relPath); sym != "" {
		return "", 0, fmt.Sprintf("%s is a symlink and was ignored", sym)
	}
	capBytes := maxBytes
	if capBytes <= 0 || capBytes > maxStreamBytes {
		capBytes = maxStreamBytes
	}
	f, err := s.root.OpenFile(relPath, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", 0, ""
	}
	if err != nil {
		return "", 0, err.Error()
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", 0, err.Error()
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Sprintf("%s is not a regular file", relPath)
	}
	data, err := io.ReadAll(io.LimitReader(f, capBytes+1))
	if err != nil {
		return "", 0, err.Error()
	}
	total := max(info.Size(), int64(len(data)))
	if int64(len(data)) > capBytes || info.Size() > capBytes {
		return "", total, ""
	}
	return string(data), int64(len(data)), ""
}

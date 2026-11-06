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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Capture modes for Config.CaptureMode.
const (
	CaptureModeFailures = "failures"
	CaptureModeAll      = "all"

	defaultCaptureMaxFiles = 200
	maxCapturedFileBytes   = 64 * 1024
	captureFilePrefix      = "kg-capture-"
	captureFileSuffix      = ".json"
)

// ValidateCaptureConfig checks --capture-dir and --capture-mode.
func ValidateCaptureConfig(dir, mode string) error {
	dir = strings.TrimSpace(dir)
	mode = strings.TrimSpace(mode)
	if dir == "" {
		if mode != "" {
			return fmt.Errorf("--capture-mode requires --capture-dir")
		}
		return nil
	}
	if mode != "" && mode != CaptureModeFailures && mode != CaptureModeAll {
		return fmt.Errorf("--capture-mode must be %q or %q, got %q", CaptureModeFailures, CaptureModeAll, mode)
	}
	return nil
}

// CaptureRecord is one execute_go_code invocation saved to CaptureDir
// for post-mortem debugging. It deliberately excludes credential files,
// environment variables, access tokens, and result.value.
type CaptureRecord struct {
	Timestamp    string            `json:"timestamp"`
	Status       string            `json:"status"`
	Stage        string            `json:"stage,omitempty"`
	Phase        string            `json:"phase,omitempty"`
	Mode         string            `json:"mode,omitempty"`
	ExitCode     int               `json:"exit_code"`
	Tidied       bool              `json:"tidied"`
	BuildMS      int64             `json:"build_ms,omitempty"`
	DurationMS   int64             `json:"duration_ms"`
	ExtraImports []string          `json:"extra_imports,omitempty"`
	Files        map[string]string `json:"files,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
	Stdout       string            `json:"stdout,omitempty"`
	Stderr       string            `json:"stderr,omitempty"`
	Error        string            `json:"error,omitempty"`
	Result       *CapturedResult   `json:"result,omitempty"`
}

// CapturedResult holds the non-value fields of Result so error/panic
// diagnostics are preserved without writing user data (Result.Value) to
// disk.
type CapturedResult struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	Stack   string `json:"stack,omitempty"`
	Type    string `json:"type,omitempty"`
}

type capturer struct {
	dir      string
	mode     string
	maxFiles int

	mu  sync.Mutex
	seq uint64
}

func newCapturer(dir, mode string, maxFiles int) *capturer {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = CaptureModeFailures
	}
	if maxFiles <= 0 {
		maxFiles = defaultCaptureMaxFiles
	}
	return &capturer{
		dir:      dir,
		mode:     mode,
		maxFiles: maxFiles,
	}
}

func (c *capturer) shouldRecord(status string) bool {
	if c == nil {
		return false
	}
	if c.mode == CaptureModeAll {
		return true
	}
	return status != "ok"
}

func (c *capturer) recordOutcome(inputFiles map[string][]byte, extraImports []string, out *ExecuteGoCodeOutput) {
	status := classifyOutcomeStatus(out)
	if !c.shouldRecord(status) {
		return
	}
	rec := CaptureRecord{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Status:       status,
		Stage:        "done",
		Phase:        out.Phase,
		Mode:         out.Mode,
		ExitCode:     out.ExitCode,
		Tidied:       out.Tidied,
		BuildMS:      out.BuildMS,
		DurationMS:   out.DurationMS,
		ExtraImports: slices.Clone(extraImports),
		Files:        sanitizeCapturedFiles(inputFiles),
		Warnings:     slices.Clone(out.Warnings),
		Stdout:       truncateCapturedString(out.Stdout, maxCapturedFileBytes),
		Stderr:       truncateCapturedString(out.Stderr, maxCapturedFileBytes),
	}
	if out.Result != nil {
		rec.Result = &CapturedResult{
			Kind:    sanitizeResultKind(out.Result),
			Message: truncateCapturedString(out.Result.Message, maxCapturedFileBytes),
			Stack:   truncateCapturedString(out.Result.Stack, maxCapturedFileBytes),
			Type:    truncateCapturedString(out.Result.Type, 1024),
		}
	}
	c.write(rec)
}

func (c *capturer) recordFailure(stage, mode, reason string, d time.Duration, inputFiles map[string][]byte, extraImports []string, err error) {
	status := classifyFailureStatus(stage, reason)
	if !c.shouldRecord(status) {
		return
	}
	var errMsg string
	if err != nil {
		errMsg = truncateCapturedString(err.Error(), maxCapturedFileBytes)
	}
	rec := CaptureRecord{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Status:       status,
		Stage:        stage,
		Mode:         mode,
		ExitCode:     -1,
		DurationMS:   d.Milliseconds(),
		ExtraImports: slices.Clone(extraImports),
		Files:        sanitizeCapturedFiles(inputFiles),
		Error:        errMsg,
	}
	c.write(rec)
}

func classifyOutcomeStatus(out *ExecuteGoCodeOutput) string {
	if out == nil {
		return "exec_error"
	}
	if out.Phase == "build" {
		return "build_error"
	}
	if out.ExitCode != 0 || (out.Result != nil && out.Result.Kind != "ok") {
		return "run_error"
	}
	if out.Tidied {
		return "tidied"
	}
	return "ok"
}

func classifyFailureStatus(stage, reason string) string {
	switch {
	case stage == "normalize":
		return "normalize_error"
	case reason == "deadline_exceeded":
		return "timeout"
	case reason == "context_canceled":
		return "canceled"
	default:
		return "exec_error"
	}
}

func sanitizeCapturedFiles(inputFiles map[string][]byte) map[string]string {
	if len(inputFiles) == 0 {
		return nil
	}
	out := make(map[string]string, len(inputFiles))
	for k, v := range inputFiles {
		// Defensive guard: never capture anything under .kode-gopher even if
		// a caller passed it in inputFiles.
		clean := filepath.ToSlash(filepath.Clean(k))
		if clean == ".kode-gopher" || strings.HasPrefix(clean, ".kode-gopher/") {
			continue
		}
		out[k] = truncateCapturedString(string(v), maxCapturedFileBytes)
	}
	return out
}

func truncateCapturedString(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-cut)
}

func (c *capturer) write(rec CaptureRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		log.Printf("execute_go_code: capture mkdir failed: %v", err)
		return
	}
	// Evict oldest records before creating the new file so a full ring
	// buffer frees space even when the volume is near capacity.
	c.pruneLocked(c.maxFiles - 1)

	c.seq++
	ts := time.Now().UTC().Format("20060102T150405.000Z")
	name := fmt.Sprintf("%s%s-%06d-%s%s", captureFilePrefix, ts, c.seq, rec.Status, captureFileSuffix)

	tmp, err := os.CreateTemp(c.dir, ".kg-capture-*.tmp")
	if err != nil {
		log.Printf("execute_go_code: capture create temp failed: %v", err)
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Chmod(0o600)

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	encErr := enc.Encode(rec)
	closeErr := tmp.Close()
	if encErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		log.Printf("execute_go_code: capture write failed")
		return
	}
	if err := os.Rename(tmpPath, filepath.Join(c.dir, name)); err != nil {
		_ = os.Remove(tmpPath)
		log.Printf("execute_go_code: capture rename failed: %v", err)
		return
	}
}

func (c *capturer) pruneLocked(keep int) {
	if keep < 0 {
		keep = 0
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, captureFilePrefix) && strings.HasSuffix(n, captureFileSuffix) {
			names = append(names, n)
		}
	}
	if len(names) <= keep {
		return
	}
	slices.Sort(names)
	for _, old := range names[:len(names)-keep] {
		_ = os.Remove(filepath.Join(c.dir, old))
	}
}

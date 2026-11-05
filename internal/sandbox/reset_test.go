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

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runResetInDir(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", resetCmd)
	cmd.Dir = dir
	// Enforce strict POSIX option parsing (matching BSD/macOS getopt behavior)
	// so GNU-only flag ordering regressions fail on Linux CI as well.
	cmd.Env = append(os.Environ(), "POSIXLY_CORRECT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resetCmd failed: %v\noutput:\n%s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("resetCmd wrote unexpected output:\n%s", out)
	}
}

func TestResetCmdPreservesPersistAndWipesWorkdir(t *testing.T) {
	app := t.TempDir()

	// Populate per-run artifacts, dotfiles (including ..*), leading-dash
	// names, forwarded credential files, and cross-turn persist files.
	mustWrite := func(rel, body string) {
		t.Helper()
		full := filepath.Join(app, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("main.go", "package main\n")
	mustWrite("go.mod", "module kode_gopher_user\n")
	mustWrite("helper/util.go", "package helper\n")
	mustWrite(".kode-gopher/result.json", `{"kind":"ok","value":1}`)
	mustWrite(".kode-gopher/creds/adc.json", `{"type":"authorized_user"}`)
	mustWrite(".hidden", "dot")
	mustWrite("..double_dot", "doubledot")
	mustWrite("-dash-file", "dash")
	mustWrite("persist/clusters.json", `["prod-1","prod-2"]`)
	mustWrite("persist/sub/notes.txt", "nested")

	runResetInDir(t, app)

	entries, err := os.ReadDir(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != PersistDir || !entries[0].IsDir() {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only %q directory in /app after reset, got %v", PersistDir, names)
	}

	got, err := os.ReadFile(filepath.Join(app, "persist/clusters.json"))
	if err != nil || string(got) != `["prod-1","prod-2"]` {
		t.Fatalf("persist/clusters.json = %q, err=%v", got, err)
	}
	gotNested, err := os.ReadFile(filepath.Join(app, "persist/sub/notes.txt"))
	if err != nil || string(gotNested) != "nested" {
		t.Fatalf("persist/sub/notes.txt = %q, err=%v", gotNested, err)
	}
}

func TestResetCmdCreatesPersistInEmptyWorkdir(t *testing.T) {
	app := t.TempDir()
	runResetInDir(t, app)

	info, err := os.Lstat(filepath.Join(app, PersistDir))
	if err != nil {
		t.Fatalf("stat persist: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected persist to be a real directory, got mode %v", info.Mode())
	}
	if err := os.WriteFile(filepath.Join(app, PersistDir, "probe.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("persist not writable after reset: %v", err)
	}
}

func TestResetCmdRepairsReadOnlyDirsAndSymlinkTraps(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(outsideFile, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0o755) })

	// 1. Read-only subdirectory and read-only persist directory + symlink to outside.
	roDir := filepath.Join(app, "readonly")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "locked.txt"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(roDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}

	persistPath := filepath.Join(app, PersistDir)
	if err := os.MkdirAll(persistPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persistPath, "saved.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(persistPath, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(app, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(app, 0o755)
		_ = os.Chmod(persistPath, 0o755)
		_ = os.Chmod(roDir, 0o755)
	})

	runResetInDir(t, app)

	if _, err := os.Stat(roDir); !os.IsNotExist(err) {
		t.Fatalf("expected readonly dir to be removed, stat err=%v", err)
	}
	// Symlink target outside /app must remain untouched and keep its mode.
	outInfo, err := os.Stat(outside)
	if err != nil || outInfo.Mode().Perm() != 0o500 {
		t.Fatalf("outside dir altered: info=%v err=%v", outInfo, err)
	}
	if b, err := os.ReadFile(outsideFile); err != nil || string(b) != "untouched" {
		t.Fatalf("outside file altered: %q err=%v", b, err)
	}
	// Persist contents survive and persist itself is writable again.
	if b, err := os.ReadFile(filepath.Join(persistPath, "saved.txt")); err != nil || string(b) != "kept" {
		t.Fatalf("persist/saved.txt = %q err=%v", b, err)
	}
	if err := os.WriteFile(filepath.Join(persistPath, "next.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("persist not writable after repairing 0500 mode: %v", err)
	}

	// 2. Replacing persist with a symlink or regular file must be cleaned and recreated as a real dir.
	if err := os.RemoveAll(persistPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, persistPath); err != nil {
		t.Fatal(err)
	}
	runResetInDir(t, app)
	pInfo, err := os.Lstat(persistPath)
	if err != nil || !pInfo.IsDir() {
		t.Fatalf("expected symlink at persist to be replaced by real directory, got info=%v err=%v", pInfo, err)
	}
	if b, err := os.ReadFile(outsideFile); err != nil || string(b) != "untouched" {
		t.Fatalf("outside file deleted through persist symlink: %q err=%v", b, err)
	}

	if err := os.RemoveAll(persistPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistPath, []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	runResetInDir(t, app)
	pInfo, err = os.Lstat(persistPath)
	if err != nil || !pInfo.IsDir() {
		t.Fatalf("expected file at persist to be replaced by real directory, got info=%v err=%v", pInfo, err)
	}
}

func TestResetCmdHandlesDashDirsZeroPermsAndDoubleDashSymlink(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(app, "-dashdir", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, PersistDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "-dashdir", "inner", "f.txt"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, PersistDir, "saved.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink literally named "--" pointing outside /app: non-POSIX `chmod mode -- {}`
	// would treat "--" as a path operand and follow the symlink.
	if err := os.Symlink(outside, filepath.Join(app, "--")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(app, "-dashdir", "inner"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(app, "-dashdir"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(app, PersistDir), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(app, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(outside, 0o755)
		_ = os.Chmod(app, 0o755)
		_ = os.Chmod(filepath.Join(app, PersistDir), 0o755)
		_ = os.Chmod(filepath.Join(app, "-dashdir"), 0o755)
		_ = os.Chmod(filepath.Join(app, "-dashdir", "inner"), 0o755)
	})

	runResetInDir(t, app)

	outInfo, err := os.Stat(outside)
	if err != nil || outInfo.Mode().Perm() != 0o500 {
		t.Fatalf("outside target mode mutated via '--' symlink: info=%v err=%v", outInfo, err)
	}
	if _, err := os.Lstat(filepath.Join(app, "--")); !os.IsNotExist(err) {
		t.Fatalf("expected '--' symlink to be removed, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(app, "-dashdir")); !os.IsNotExist(err) {
		t.Fatalf("expected '-dashdir' to be removed, err=%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(app, PersistDir, "saved.txt")); err != nil || string(b) != "kept" {
		t.Fatalf("persist/saved.txt = %q err=%v", b, err)
	}
}

func TestResetCmdPropagatesWipeError(t *testing.T) {
	app := t.TempDir()
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "stale.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Shadow rm with a failing stub so line 4 of resetCmd fails while
	// line 5 (mkdir -p persist && chmod) would otherwise succeed.
	rmStub := filepath.Join(fakeBin, "rm")
	if err := os.WriteFile(rmStub, []byte("#!/bin/sh\necho 'simulated rm failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", resetCmd)
	cmd.Dir = app
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "POSIXLY_CORRECT=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected resetCmd to fail when rm fails, got exit 0 (output: %s)", out)
	}
}

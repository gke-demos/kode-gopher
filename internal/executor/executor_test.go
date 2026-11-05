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

package executor

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

func TestParseBuildStdout(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		stdout string
		tidied bool
		moved  []string
	}{
		{name: "no tidy", in: "", stdout: ""},
		{name: "tidy, nothing moved", in: markerTidy + "\n", tidied: true},
		{
			name:   "tidy moved versions, other output kept",
			in:     "hello\n" + markerTidy + "\n" + markerMoved + " k8s.io/api v0.34.1 v0.35.0\n" + markerMoved + " golang.org/x/net v0.40.0 v0.41.0\nbye",
			stdout: "hello\nbye",
			tidied: true,
			moved:  []string{"k8s.io/api v0.34.1 v0.35.0", "golang.org/x/net v0.40.0 v0.41.0"},
		},
	}
	for _, c := range cases {
		stdout, tidied, moved := parseBuildStdout(c.in)
		if stdout != c.stdout || tidied != c.tidied || !reflect.DeepEqual(moved, c.moved) {
			t.Errorf("%s: got (%q, %v, %q), want (%q, %v, %q)", c.name, stdout, tidied, moved, c.stdout, c.tidied, c.moved)
		}
	}
}

func TestMovedWarning(t *testing.T) {
	w := movedWarning([]string{"k8s.io/api v0.34.1 v0.35.0"})
	for _, want := range []string{"k8s.io/api v0.34.1 -> v0.35.0", "memory"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q lacks %q", w, want)
		}
	}
}

func TestParseResult(t *testing.T) {
	// 40 KiB of value: over the old 16 KiB truncation that made large
	// results vanish, under maxResultBytes.
	big := `{"kind":"ok","value":"` + strings.Repeat("x", 40<<10) + `"}`
	for _, tc := range []struct {
		name     string
		body     string
		wantKind string
		wantErr  string
	}{
		{"empty", "", "", ""},
		{"whitespace", " \n", "", ""},
		{"ok", `{"kind":"ok","value":[1,2]}`, "ok", ""},
		{"panic", `{"kind":"panic","message":"boom","stack":"..."}`, "panic", ""},
		{"large but allowed", big, "ok", ""},
		{"too large", `{"kind":"ok","value":"` + strings.Repeat("x", maxResultBytes) + `"}`, "", "over the 256 KiB limit"},
		{"truncated JSON", big[:len(big)/2], "", "isn't valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, msg := parseResult(tc.body)
			if tc.wantErr != "" {
				if r != nil || !strings.Contains(msg, tc.wantErr) {
					t.Fatalf("got result %v, message %q; want nil and %q", r, msg, tc.wantErr)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected message %q", msg)
			}
			if tc.wantKind == "" {
				if r != nil {
					t.Fatalf("got %+v, want no result", r)
				}
				return
			}
			if r == nil || r.Kind != tc.wantKind {
				t.Fatalf("got %+v, want kind %q", r, tc.wantKind)
			}
		})
	}
}

func TestBuildCmdCreatesAndIgnoresPersistDir(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	proxyMod := filepath.Join(root, "proxy", "example.local", "dep", "@v")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proxyMod, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProxyModule(t, proxyMod, "example.local/dep", "v0.1.0")

	goMod := "module kode_gopher_user\n\ngo 1.26.10\n"
	mainSrc := "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ok\") }\n"
	if err := os.WriteFile(filepath.Join(app, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run 1: persist/ does not exist yet; buildCmd creates it.
	cmd1 := exec.Command("sh", "-c", buildCmd)
	cmd1.Dir = app
	if out, err := cmd1.CombinedOutput(); err != nil {
		t.Fatalf("run 1 buildCmd failed: %v\n%s", err, out)
	}
	if info, err := os.Stat(filepath.Join(app, "persist")); err != nil || !info.IsDir() {
		t.Fatalf("expected buildCmd to create persist/, got info=%v err=%v", info, err)
	}

	// Run 2: place a syntax-broken .go file in persist/ AND import an unrequired
	// module served from a local file:// GOPROXY so buildCmd exercises
	// `go mod edit -ignore=persist` + `go mod tidy` with zero network access.
	if err := os.WriteFile(filepath.Join(app, "persist", "scratch.go"), []byte("package broken\nimport \"example.invalid/missing\"\nfunc {"), 0o644); err != nil {
		t.Fatal(err)
	}
	tidyMain := "package main\nimport _ \"example.local/dep\"\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(app, "main.go"), []byte(tidyMain), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd2 := exec.Command("sh", "-c", buildCmd)
	cmd2.Dir = app
	cmd2.Env = append(os.Environ(),
		"GOPROXY=file://"+filepath.ToSlash(filepath.Join(root, "proxy")),
		"GONOSUMDB=*",
		"GOMODCACHE="+filepath.Join(root, "modcache"),
		"GOFLAGS=-modcacherw",
	)
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("run 2 buildCmd (tidy path with broken persist/scratch.go) failed: %v\n%s", err, out2)
	}
	_, tidied, _ := parseBuildStdout(string(out2))
	if !tidied {
		t.Fatalf("expected tidy marker in stdout, got %q", string(out2))
	}
}

func writeProxyModule(t *testing.T, vDir, modPath, ver string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(vDir, "list"), []byte(ver+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vDir, ver+".info"), []byte(`{"Version":"`+ver+`","Time":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vDir, ver+".mod"), []byte("module "+modPath+"\n\ngo 1.26.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zf, err := os.Create(filepath.Join(vDir, ver+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	prefix := modPath + "@" + ver + "/"
	w1, err := zw.Create(prefix + "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w1.Write([]byte("module " + modPath + "\n\ngo 1.26.10\n"))
	w2, err := zw.Create(prefix + "dep.go")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w2.Write([]byte("package dep\nconst V = 1\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

type fakeSession struct {
	runGoFn   func(context.Context, sandbox.RunGoRequest) (*sandbox.RunGoResult, error)
	resetFn   func(context.Context) error
	executeFn func(context.Context, sandbox.Request) (*sandbox.Result, error)

	runGoCalls   int
	resetCalls   int
	executeCalls []sandbox.Request
}

func (f *fakeSession) RunGo(ctx context.Context, req sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
	f.runGoCalls++
	if f.runGoFn == nil {
		return nil, sandbox.ErrRunGoUnsupported
	}
	return f.runGoFn(ctx, req)
}

func (f *fakeSession) Reset(ctx context.Context) error {
	f.resetCalls++
	if f.resetFn != nil {
		return f.resetFn(ctx)
	}
	return nil
}

func (f *fakeSession) Execute(ctx context.Context, req sandbox.Request) (*sandbox.Result, error) {
	f.executeCalls = append(f.executeCalls, req)
	if f.executeFn != nil {
		return f.executeFn(ctx, req)
	}
	return &sandbox.Result{}, nil
}

func TestRunSingleHopCollapsesResetBuildRunFetch(t *testing.T) {
	fs := &fakeSession{
		runGoFn: func(_ context.Context, req sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
			if !req.Reset || req.ResultPath != resultPath {
				t.Fatalf("unexpected RunGoRequest: %+v", req)
			}
			if !reflect.DeepEqual(req.PreserveDirs, []string{sandbox.PersistDir}) {
				t.Fatalf("PreserveDirs = %q, want [%q]", req.PreserveDirs, sandbox.PersistDir)
			}
			return &sandbox.RunGoResult{
				Phase:         "run",
				ExitCode:      0,
				Stdout:        "user log\n",
				BuildStdout:   markerTidy + "\n" + markerMoved + " k8s.io/api v0.34.0 v0.35.0\n",
				BuildDuration: 400 * time.Millisecond,
				RunDuration:   50 * time.Millisecond,
				Result:        `{"kind":"ok","value":"done"}`,
				ResultSize:    28,
			}, nil
		},
	}

	out, err := runWithSession(context.Background(), fs, Request{
		Reset: true,
		Files: map[string][]byte{"main.go": []byte("package main\nfunc main(){}\n")},
		Env:   map[string]string{"GOOGLE_CLOUD_PROJECT": "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fs.runGoCalls != 1 || fs.resetCalls != 0 || len(fs.executeCalls) != 0 {
		t.Fatalf("expected 1 RunGo call and 0 Reset/Execute calls, got runGo=%d reset=%d execute=%d",
			fs.runGoCalls, fs.resetCalls, len(fs.executeCalls))
	}
	if out.Phase != PhaseRun || !out.Tidied || out.Result == nil || out.Result.Kind != "ok" {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "k8s.io/api v0.34.0 -> v0.35.0") {
		t.Fatalf("warnings = %q, want moved module warning", out.Warnings)
	}
	if out.Duration != 450*time.Millisecond || out.BuildDuration != 400*time.Millisecond {
		t.Fatalf("durations: total=%s build=%s", out.Duration, out.BuildDuration)
	}
}

func TestRunSingleHopFetchesAttachmentsOnlyWhenSignaled(t *testing.T) {
	pngBytes := samplePNG(t)
	fs := &fakeSession{
		runGoFn: func(_ context.Context, req sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
			return &sandbox.RunGoResult{
				Phase:          "run",
				ExitCode:       0,
				Result:         `{"kind":"ok","value":"with-plot"}`,
				ResultSize:     31,
				HasAttachments: true,
			}, nil
		},
		executeFn: func(_ context.Context, req sandbox.Request) (*sandbox.Result, error) {
			if !req.Raw {
				t.Fatalf("expected Raw=true on attachment fetch")
			}
			_, rest, _ := strings.Cut(req.Command, "\nn='")
			nonce, _, _ := strings.Cut(rest, "'")
			bname := base64.StdEncoding.EncodeToString([]byte("plot.png"))
			bdata := base64.StdEncoding.EncodeToString(pngBytes)
			stdout := "\n::" + nonce + ":attachments::\n" +
				"::" + nonce + ":file:68:" + bname + "::\n" +
				bdata + "\n::" + nonce + ":end::\n"
			return &sandbox.Result{Stdout: stdout}, nil
		},
	}

	out, err := runWithSession(context.Background(), fs, Request{Reset: true})
	if err != nil {
		t.Fatal(err)
	}
	if fs.runGoCalls != 1 || len(fs.executeCalls) != 1 {
		t.Fatalf("calls: runGo=%d execute=%d, want 1 and 1", fs.runGoCalls, len(fs.executeCalls))
	}
	if out.Result == nil || out.Result.Kind != "ok" {
		t.Fatalf("Result = %+v, want kind=ok", out.Result)
	}
	if len(out.Attachments) != 1 || out.Attachments[0].Name != "plot.png" || !bytes.Equal(out.Attachments[0].Data, pngBytes) {
		t.Fatalf("Attachments = %+v", out.Attachments)
	}
}

func TestRunSingleHopBuildFailOversizeAndTimeout(t *testing.T) {
	// 1. Build failure strips markers and reports PhaseBuild.
	fsBuild := &fakeSession{
		runGoFn: func(context.Context, sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
			return &sandbox.RunGoResult{
				Phase:         "build",
				ExitCode:      2,
				Stdout:        markerTidy + "\n",
				Stderr:        "main.go:3: undefined: x\n",
				BuildStdout:   markerTidy + "\n",
				BuildDuration: 200 * time.Millisecond,
			}, nil
		},
	}
	out, err := runWithSession(context.Background(), fsBuild, Request{Reset: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != PhaseBuild || out.ExitCode != 2 || !out.Tidied || out.Stdout != "" {
		t.Fatalf("build failure outcome: %+v", out)
	}

	// 2. Oversize result dropped on server surfaces standard size warning.
	fsBig := &fakeSession{
		runGoFn: func(context.Context, sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
			return &sandbox.RunGoResult{
				Phase:      "run",
				ResultSize: 300 << 10,
			}, nil
		},
	}
	out, err = runWithSession(context.Background(), fsBig, Request{Reset: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != nil || !strings.Contains(out.ResultError, "over the 256 KiB limit") {
		t.Fatalf("oversize outcome: result=%v err=%q", out.Result, out.ResultError)
	}

	// 3. Phase timeout surfaces context.DeadlineExceeded.
	fsTimeout := &fakeSession{
		runGoFn: func(context.Context, sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
			return &sandbox.RunGoResult{Phase: "run", TimedOut: true}, nil
		},
	}
	if _, err := runWithSession(context.Background(), fsTimeout, Request{Reset: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v, want DeadlineExceeded", err)
	}
}

func TestRunFallsBackToMultiHopWhenRunGoUnsupported(t *testing.T) {
	step := 0
	fs := &fakeSession{
		runGoFn: func(context.Context, sandbox.RunGoRequest) (*sandbox.RunGoResult, error) {
			return nil, sandbox.ErrRunGoUnsupported
		},
		executeFn: func(_ context.Context, req sandbox.Request) (*sandbox.Result, error) {
			step++
			switch step {
			case 1: // build
				return &sandbox.Result{Stdout: "", Duration: 100 * time.Millisecond}, nil
			case 2: // run
				return &sandbox.Result{Stdout: "fallback\n", Duration: 25 * time.Millisecond}, nil
			default: // fetch
				_, rest, _ := strings.Cut(req.Command, "\nn='")
				nonce, _, _ := strings.Cut(rest, "'")
				return &sandbox.Result{Stdout: "{\"kind\":\"ok\",\"value\":1}\n::" + nonce + ":attachments::\n"}, nil
			}
		},
	}

	out, err := runWithSession(context.Background(), fs, Request{
		Reset: true,
		Files: map[string][]byte{"main.go": []byte("package main\nfunc main(){}\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fs.runGoCalls != 1 || fs.resetCalls != 1 || len(fs.executeCalls) != 3 {
		t.Fatalf("fallback calls: runGo=%d reset=%d execute=%d, want 1/1/3",
			fs.runGoCalls, fs.resetCalls, len(fs.executeCalls))
	}
	if out.Phase != PhaseRun || out.Stdout != "fallback\n" || out.Result == nil || out.Result.Kind != "ok" || out.ResultError != "" {
		t.Fatalf("fallback outcome: %+v", out)
	}
}

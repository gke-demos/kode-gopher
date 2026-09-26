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

// kg-difftest runs every snippet in a directory under the real Go toolchain
// and under one or more interpreters, and reports where the interpreters'
// stdout or exit code differ from Go's.
//
// Go is the oracle: the point is to catch interpreter bugs that produce a
// plausible wrong answer instead of an error, which no amount of "does it
// run" testing finds.
//
//	kg-difftest -interp 'stock=/tmp/yaegi run' -interp 'patched=/tmp/runner' testdata/diff
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type interpFlag []string

func (f *interpFlag) String() string     { return strings.Join(*f, ", ") }
func (f *interpFlag) Set(v string) error { *f = append(*f, v); return nil }

type interpreter struct {
	name string
	argv []string
}

type result struct {
	stdout string
	stderr string
	code   int
}

func run(argv []string, file string, timeout time.Duration) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := argv[1:]
	if file != "" {
		args = append(args, file)
	}
	cmd := exec.CommandContext(ctx, argv[0], args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ctx.Err() != nil {
		code = -2 // timeout
	} else if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// lastLine returns the last non-[poc] line of stderr, for a one-line reason.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "[poc] ok") {
			if len(l) > 160 {
				l = l[:160] + "…"
			}
			return l
		}
	}
	return ""
}

// firstDiff returns the first differing line of two outputs.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d: want %q, got %q", i+1, wl, gl)
		}
	}
	return ""
}

func main() {
	var interps interpFlag
	flag.Var(&interps, "interp", "name=command line of an interpreter to test (repeatable); the snippet path is appended")
	timeout := flag.Duration("timeout", 60*time.Second, "per-run timeout")
	verbose := flag.Bool("v", false, "print full output on mismatch")
	flag.Parse()
	if flag.NArg() != 1 || len(interps) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kg-difftest -interp name=cmd [-interp ...] <dir>")
		os.Exit(2)
	}

	var ins []interpreter
	for _, s := range interps {
		name, cmdline, ok := strings.Cut(s, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "bad -interp %q, want name=command\n", s)
			os.Exit(2)
		}
		ins = append(ins, interpreter{name: name, argv: strings.Fields(cmdline)})
	}

	files, _ := filepath.Glob(filepath.Join(flag.Arg(0), "*.go"))
	sort.Strings(files)

	bindir, err := os.MkdirTemp("", "kg-difftest")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(bindir)

	fails := map[string]int{}
	for _, f := range files {
		// Build then run, rather than `go run`, which reports every nonzero
		// exit as 1 and so loses os.Exit codes.
		bin := filepath.Join(bindir, strings.TrimSuffix(filepath.Base(f), ".go"))
		if out, err := exec.Command("go", "build", "-o", bin, f).CombinedOutput(); err != nil {
			fmt.Printf("%-36s go build FAILED (fix the snippet): %s\n", filepath.Base(f), lastLine(string(out)))
			continue
		}
		want := run([]string{bin}, "", *timeout)
		var cells []string
		var details []string
		for _, in := range ins {
			got := run(in.argv, f, *timeout)
			switch {
			case got.code == -2:
				cells = append(cells, in.name+":TIMEOUT")
				fails[in.name]++
			// An unrecovered panic exits 2 in Go; an interpreter only has to fail.
			case want.code == 2 && got.code > 0 && got.stdout == want.stdout:
				cells = append(cells, in.name+":ok")
			case got.code != want.code:
				cells = append(cells, in.name+":ERROR")
				details = append(details, fmt.Sprintf("    %s: exit %d: %s", in.name, got.code, lastLine(got.stderr)))
				fails[in.name]++
			case got.stdout != want.stdout:
				cells = append(cells, in.name+":WRONG")
				details = append(details, fmt.Sprintf("    %s: %s", in.name, firstDiff(want.stdout, got.stdout)))
				if *verbose {
					details = append(details, "      want:\n"+want.stdout, "      got:\n"+got.stdout)
				}
				fails[in.name]++
			default:
				cells = append(cells, in.name+":ok")
			}
		}
		fmt.Printf("%-36s %s\n", filepath.Base(f), strings.Join(cells, "  "))
		for _, d := range details {
			fmt.Println(d)
		}
	}

	fmt.Printf("\n%d snippets\n", len(files))
	for _, in := range ins {
		fmt.Printf("  %-10s %d wrong or failing\n", in.name, fails[in.name])
	}
}

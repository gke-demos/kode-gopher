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

// Package normalize turns user-supplied Go source(s) into the file set
// that gets shipped to the sandbox.
//
// Two modes:
//
//   - Verbatim: the root package is `main`. Root files ship as-is; the
//     user owns the whole contract (including writing
//     /app/.kode-gopher/result.json themselves if they want a
//     structured result). Exactly one root file must declare
//     `func main`.
//
//   - Wrapped: the root package is anything BUT `main`, and exactly
//     one root file declares
//     `func run(ctx context.Context) (any, error)`. We rewrite ALL
//     root files' package declarations to `main` (Go requires all
//     files in a directory to share a package name — rewriting only
//     the run-file would produce a "found packages main and X" build
//     error) and add the wrapper file that supplies func main().
//
// Multi-file: callers pass a map keyed by relative path. Keys without
// a slash are "root" (the entry-point package); keys with a slash are
// subdirectory helper packages and pass through unchanged regardless
// of mode — their package declarations aren't rewritten and their
// contents aren't inspected. Root-file same-package rules apply only
// to the root.
package normalize

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"sort"
	"strings"

	"github.com/gke-demos/kode-gopher/internal/wrapper"
)

// Mode is which path Normalize took.
type Mode int

const (
	ModeVerbatim Mode = iota
	ModeWrapped
)

func (m Mode) String() string {
	switch m {
	case ModeVerbatim:
		return "verbatim"
	case ModeWrapped:
		return "wrapped"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Result is what Normalize produces: the source files to ship into the
// sandbox (relative to /app) plus the mode tag for diagnostics.
//
// The Files map covers compilation only. Runtime artifacts like
// credentials and the synthesized go.mod are added by the caller.
type Result struct {
	Mode  Mode
	Files map[string][]byte
}

// Options modifies what Normalize emits. The zero value is the
// vanilla path (no extra imports, no other tweaks).
type Options struct {
	// ExtraImports is an optional list of package import paths that
	// the snippet should pull in even if its own source doesn't
	// declare them. Translated to a generated companion file
	// (kg_extra_imports.go) of blank imports in the root package,
	// which makes `go mod tidy` add them to the synthesized go.mod's
	// require list.
	ExtraImports []string
}

// extraImportsFilename is the generated companion file path inside
// the sandbox. Distinctive prefix so a user-written snippet can't
// collide.
const extraImportsFilename = "kg_extra_imports.go"

// Normalize partitions input into root files (entry-point package)
// and subdirectory files (helper packages), determines the mode from
// the root, and produces the sandbox file set.
//
// Root-file rules:
//   - All root files must declare the same package name.
//   - Verbatim mode (root package "main"): exactly one root file
//     declares `func main`. Root files pass through unchanged.
//   - Wrapped mode (root package other than "main"): exactly one root
//     file declares `func run(ctx context.Context) (any, error)`. All
//     root files' package decls are rewritten to `main`, and the
//     wrapper file is added at the root.
//
// Subdirectory files (keys containing "/") pass through unchanged
// regardless of mode.
//
// If opts.ExtraImports is non-empty, kg_extra_imports.go is added at
// the root as a blank-import companion. Emitted with `package main`
// so it slots into either mode.
func Normalize(files map[string][]byte, opts Options) (*Result, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("no files provided")
	}

	root, subdir := partition(files)
	if len(root) == 0 {
		return nil, fmt.Errorf("no root files (all keys contain '/'); at least one entry-point file must live at the root")
	}

	// Parse root files, collect their package names + function decls.
	fset := token.NewFileSet()
	type parsed struct {
		name string
		src  []byte
		ast  *ast.File
	}
	parsedRoot := make([]parsed, 0, len(root))
	for _, name := range sortedKeys(root) {
		src := root[name]
		f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		parsedRoot = append(parsedRoot, parsed{name: name, src: src, ast: f})
	}

	// All root files must share the same package name (Go language
	// rule). If they don't, the sandbox `go build` would fail with
	// "found packages main and X"; catch it here with a legible message
	// instead.
	pkg := parsedRoot[0].ast.Name.Name
	for _, p := range parsedRoot[1:] {
		if p.ast.Name.Name != pkg {
			return nil, fmt.Errorf(
				"root files must share a package name, but %s is `package %s` and %s is `package %s`",
				parsedRoot[0].name, pkg, p.name, p.ast.Name.Name,
			)
		}
	}

	out := map[string][]byte{}
	// Subdirectory files pass through unchanged in all modes.
	for k, v := range subdir {
		out[k] = v
	}

	var mode Mode
	if pkg == "main" {
		mode = ModeVerbatim
		mainCount := 0
		for _, p := range parsedRoot {
			if declaresFunc(p.ast, "main") {
				mainCount++
			}
			// Verbatim: copy so caller mutations don't leak.
			buf := make([]byte, len(p.src))
			copy(buf, p.src)
			out[p.name] = buf
		}
		if mainCount == 0 {
			return nil, fmt.Errorf("verbatim mode (`package main`): no root file declares `func main`")
		}
		if mainCount > 1 {
			return nil, fmt.Errorf("verbatim mode (`package main`): %d root files declare `func main` (must be exactly one)", mainCount)
		}
	} else {
		mode = ModeWrapped
		runCount := 0
		for _, p := range parsedRoot {
			if declaresFunc(p.ast, "run") {
				runCount++
			}
		}
		if runCount == 0 {
			return nil, fmt.Errorf("wrapped mode (`package %s`): no root file declares `func run(ctx context.Context) (any, error)`", pkg)
		}
		if runCount > 1 {
			return nil, fmt.Errorf("wrapped mode (`package %s`): %d root files declare `func run` (must be exactly one)", pkg, runCount)
		}
		// Rewrite ALL root files' package decl to main together, so
		// Go's same-package rule holds after rewrite.
		for _, p := range parsedRoot {
			p.ast.Name.Name = "main"
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, p.ast); err != nil {
				return nil, fmt.Errorf("format %s: %w", p.name, err)
			}
			out[p.name] = buf.Bytes()
		}
		out[wrapper.Filename] = wrapper.Source()
	}

	if len(opts.ExtraImports) > 0 {
		out[extraImportsFilename] = renderExtraImports(opts.ExtraImports)
	}

	return &Result{Mode: mode, Files: out}, nil
}

// partition splits files by whether the key contains a "/". Keys with
// no slash are "root" (the entry-point package's directory).
func partition(files map[string][]byte) (root, sub map[string][]byte) {
	root = map[string][]byte{}
	sub = map[string][]byte{}
	for k, v := range files {
		if strings.Contains(k, "/") {
			sub[k] = v
		} else {
			root[k] = v
		}
	}
	return
}

// sortedKeys returns m's keys in lexicographic order so parse order
// (and thus error-message deterministic-ness) doesn't depend on Go's
// randomized map iteration.
func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderExtraImports emits a Go source file that blank-imports each
// path so `go mod tidy` resolves them into the synthesized go.mod.
// Emitted with `package main` — after Normalize's rewrite, the root
// is always `package main` (verbatim requires it; wrapped rewrites
// to it), so this slots in unambiguously.
func renderExtraImports(paths []string) []byte {
	var b bytes.Buffer
	b.WriteString("// Code generated by kode-gopher; DO NOT EDIT.\n")
	b.WriteString("// Pulled in by the caller's extra_imports request so go mod tidy\n")
	b.WriteString("// resolves these packages into go.mod even if the user snippet\n")
	b.WriteString("// doesn't import them directly.\n")
	b.WriteString("package main\n\nimport (\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "\t_ %q\n", p)
	}
	b.WriteString(")\n")
	return b.Bytes()
}

// declaresFunc returns true if f has a top-level `func name(...)` (a
// free function, not a method). Signature validation is deferred to
// the sandbox `go build` — a mismatch there produces a clearer error
// than anything we'd say here.
func declaresFunc(f *ast.File, name string) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv != nil {
			continue // method, not a free function
		}
		if fn.Name.Name == name {
			return true
		}
	}
	return false
}

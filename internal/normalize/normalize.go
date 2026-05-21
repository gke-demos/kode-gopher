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

// Package normalize turns user-supplied Go source into the file set
// that gets shipped to the sandbox.
//
// Two modes:
//
//   - Verbatim: input declares `package main`. Shipped as-is. The
//     user owns the whole contract, including writing
//     /app/.kode-gopher/result.json themselves if they want a
//     structured result.
//
//   - Wrapped: input declares any other package and a
//     `func run(ctx context.Context) (any, error)`. We rewrite the
//     package declaration to `main` and add the wrapper file
//     (internal/wrapper) which provides `func main()` and the result
//     protocol.
package normalize

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"

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

// Normalize parses src and returns the file set for the sandbox.
//
//   - If src declares `package main`, Result.Mode is ModeVerbatim and
//     Files["main.go"] is the unmodified src.
//   - Otherwise, src must declare `func run(ctx context.Context) (any, error)`.
//     We rewrite the package declaration to `main`, format the AST
//     back out, and add wrapper.Filename to Files.
//
// Returns an error on parse failure or, in wrapped mode, on a missing
// `run` function declaration. Signature validation beyond the name is
// deferred — a signature mismatch fails fast at sandbox compile time
// with a clear Go error.
func Normalize(src []byte) (*Result, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if f.Name.Name == "main" {
		// Copy src so the caller can mutate its slice without
		// affecting our output.
		out := make([]byte, len(src))
		copy(out, src)
		return &Result{
			Mode:  ModeVerbatim,
			Files: map[string][]byte{"main.go": out},
		}, nil
	}

	if !declaresRun(f) {
		return nil, fmt.Errorf("snippet must declare 'func run(ctx context.Context) (any, error)' (package=%s)", f.Name.Name)
	}

	f.Name.Name = "main"
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, f); err != nil {
		return nil, fmt.Errorf("format: %w", err)
	}

	return &Result{
		Mode: ModeWrapped,
		Files: map[string][]byte{
			"main.go":          buf.Bytes(),
			wrapper.Filename:   wrapper.Source(),
		},
	}, nil
}

// declaresRun returns true if f has a top-level `func run(...)`. We
// don't validate the full signature here — a mismatch will surface as
// a clear compile error from the sandbox ("undefined: run" or "too
// many arguments"), which is more actionable than anything we'd say.
func declaresRun(f *ast.File) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv != nil {
			continue // method, not a free function
		}
		if fn.Name.Name == "run" {
			return true
		}
	}
	return false
}

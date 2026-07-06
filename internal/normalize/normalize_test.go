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

package normalize_test

import (
	"strings"
	"testing"

	"github.com/gke-demos/kode-gopher/internal/normalize"
	"github.com/gke-demos/kode-gopher/internal/wrapper"
)

func single(src []byte) map[string][]byte { return map[string][]byte{"main.go": src} }

func TestNormalize_VerbatimPassesThrough(t *testing.T) {
	src := []byte(`package main

import "fmt"

func main() { fmt.Println("hi") }
`)
	got, err := normalize.Normalize(single(src), normalize.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Mode != normalize.ModeVerbatim {
		t.Errorf("Mode = %s, want verbatim", got.Mode)
	}
	if len(got.Files) != 1 {
		t.Errorf("len(Files) = %d, want 1; got keys %v", len(got.Files), keys(got.Files))
	}
	if string(got.Files["main.go"]) != string(src) {
		t.Errorf("main.go body mutated\n got: %q\nwant: %q", got.Files["main.go"], src)
	}
}

func TestNormalize_WrappedRewritesPackageAndShipsWrapper(t *testing.T) {
	src := []byte(`package kode_gopher_snippet

import "context"

func run(ctx context.Context) (any, error) {
	return map[string]int{"answer": 42}, nil
}
`)
	got, err := normalize.Normalize(single(src), normalize.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Mode != normalize.ModeWrapped {
		t.Errorf("Mode = %s, want wrapped", got.Mode)
	}
	if len(got.Files) != 2 {
		t.Fatalf("len(Files) = %d, want 2; got keys %v", len(got.Files), keys(got.Files))
	}
	main := string(got.Files["main.go"])
	if !strings.HasPrefix(main, "package main") {
		t.Errorf("main.go does not start with `package main`:\n%s", main)
	}
	if !strings.Contains(main, "func run(ctx context.Context) (any, error)") {
		t.Errorf("main.go lost run signature:\n%s", main)
	}
	wrap, ok := got.Files[wrapper.Filename]
	if !ok {
		t.Fatalf("missing wrapper file %q; have %v", wrapper.Filename, keys(got.Files))
	}
	if !strings.Contains(string(wrap), "func main()") {
		t.Errorf("wrapper missing func main():\n%s", wrap)
	}
}

func TestNormalize_WrappedRequiresRun(t *testing.T) {
	src := []byte(`package kode_gopher_snippet

import "context"

func helper(ctx context.Context) error { return nil }
`)
	_, err := normalize.Normalize(single(src), normalize.Options{})
	if err == nil {
		t.Fatalf("expected error for missing run func, got nil")
	}
	if !strings.Contains(err.Error(), "func run") {
		t.Errorf("error should mention 'func run', got: %v", err)
	}
}

func TestNormalize_ParseErrorSurfaces(t *testing.T) {
	src := []byte(`this is not Go`)
	_, err := normalize.Normalize(single(src), normalize.Options{})
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse, got: %v", err)
	}
}

func TestNormalize_VerbatimWithRunStaysVerbatim(t *testing.T) {
	// A package-main file that happens to declare `func run`. Verbatim
	// path doesn't care — the user owns the contract.
	src := []byte(`package main

import "context"

func run(ctx context.Context) (any, error) { return nil, nil }
func main() { _, _ = run(context.Background()) }
`)
	got, err := normalize.Normalize(single(src), normalize.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Mode != normalize.ModeVerbatim {
		t.Errorf("Mode = %s, want verbatim", got.Mode)
	}
	if _, ok := got.Files[wrapper.Filename]; ok {
		t.Errorf("verbatim mode should not ship wrapper file")
	}
}

func TestNormalize_ExtraImportsAddsCompanionFile(t *testing.T) {
	src := []byte(`package kode_gopher_snippet

import "context"

func run(ctx context.Context) (any, error) { return nil, nil }
`)
	got, err := normalize.Normalize(single(src), normalize.Options{
		ExtraImports: []string{"cloud.google.com/go/bigquery", "cloud.google.com/go/secretmanager/apiv1"},
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	body, ok := got.Files["kg_extra_imports.go"]
	if !ok {
		t.Fatalf("expected kg_extra_imports.go, got keys %v", keys(got.Files))
	}
	s := string(body)
	if !strings.HasPrefix(s, "// Code generated") {
		t.Errorf("companion missing generated header:\n%s", s)
	}
	if !strings.Contains(s, "package main") {
		t.Errorf("companion missing `package main`:\n%s", s)
	}
	for _, want := range []string{
		`_ "cloud.google.com/go/bigquery"`,
		`_ "cloud.google.com/go/secretmanager/apiv1"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("companion missing blank import %q:\n%s", want, s)
		}
	}
}

func TestNormalize_ExtraImportsEmptyOmitsFile(t *testing.T) {
	src := []byte(`package main
func main() {}
`)
	got, err := normalize.Normalize(single(src), normalize.Options{ExtraImports: nil})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if _, ok := got.Files["kg_extra_imports.go"]; ok {
		t.Errorf("expected no kg_extra_imports.go when ExtraImports is empty; keys %v", keys(got.Files))
	}
}

// --- Multi-file test cases (slice 4) ---

func TestNormalize_MultiFile_WrappedRewritesAllRoot(t *testing.T) {
	// Two root files sharing package `snippet`; one has run, one has
	// a helper. Both must have their package decl rewritten to `main`
	// (Go same-package rule), and the wrapper must land at root.
	files := map[string][]byte{
		"main.go": []byte(`package snippet

import "context"

func run(ctx context.Context) (any, error) {
	return helper(), nil
}
`),
		"util.go": []byte(`package snippet

func helper() int { return 42 }
`),
	}
	got, err := normalize.Normalize(files, normalize.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got.Mode != normalize.ModeWrapped {
		t.Errorf("Mode = %s, want wrapped", got.Mode)
	}
	// Both root files present, both rewritten.
	for _, name := range []string{"main.go", "util.go"} {
		body := string(got.Files[name])
		if !strings.HasPrefix(body, "package main") {
			t.Errorf("%s not rewritten to `package main`:\n%s", name, body)
		}
	}
	if _, ok := got.Files[wrapper.Filename]; !ok {
		t.Errorf("wrapper missing; keys %v", keys(got.Files))
	}
}

func TestNormalize_MultiFile_SubdirPackagesPassThrough(t *testing.T) {
	// Root wraps to main; a helper package lives in a subdirectory.
	// Subdir file must pass through UNCHANGED (still `package helper`).
	files := map[string][]byte{
		"main.go": []byte(`package snippet

import (
	"context"
	"u/helper"
)

func run(ctx context.Context) (any, error) { return helper.Answer(), nil }
`),
		"helper/util.go": []byte(`package helper

func Answer() int { return 42 }
`),
	}
	got, err := normalize.Normalize(files, normalize.Options{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// Root is rewritten.
	if !strings.HasPrefix(string(got.Files["main.go"]), "package main") {
		t.Errorf("root main.go not rewritten to `package main`:\n%s", got.Files["main.go"])
	}
	// Subdir passes through UNCHANGED.
	sub, ok := got.Files["helper/util.go"]
	if !ok {
		t.Fatalf("subdir file dropped; keys %v", keys(got.Files))
	}
	if !strings.HasPrefix(string(sub), "package helper") {
		t.Errorf("subdir file's package decl mutated:\n%s", sub)
	}
}

func TestNormalize_MultiFile_MixedRootPackagesRejected(t *testing.T) {
	files := map[string][]byte{
		"main.go":  []byte(`package snippet` + "\n" + `import "context"` + "\n" + `func run(ctx context.Context) (any, error) { return nil, nil }`),
		"other.go": []byte(`package different` + "\n" + `func Ignored() {}`),
	}
	_, err := normalize.Normalize(files, normalize.Options{})
	if err == nil {
		t.Fatal("expected error for mixed root package names, got nil")
	}
	if !strings.Contains(err.Error(), "share a package name") {
		t.Errorf("error should mention shared package name, got: %v", err)
	}
}

func TestNormalize_MultiFile_MultipleRunRejected(t *testing.T) {
	files := map[string][]byte{
		"a.go": []byte(`package snippet` + "\n" + `import "context"` + "\n" + `func run(ctx context.Context) (any, error) { return 1, nil }`),
		"b.go": []byte(`package snippet` + "\n" + `import "context"` + "\n" + `func run(ctx context.Context) (any, error) { return 2, nil }`),
	}
	_, err := normalize.Normalize(files, normalize.Options{})
	if err == nil {
		t.Fatal("expected error for multiple run funcs, got nil")
	}
	if !strings.Contains(err.Error(), "run") {
		t.Errorf("error should mention run, got: %v", err)
	}
}

func TestNormalize_MultiFile_VerbatimMultipleMainRejected(t *testing.T) {
	files := map[string][]byte{
		"a.go": []byte(`package main` + "\n" + `func main() {}`),
		"b.go": []byte(`package main` + "\n" + `func main() {}`),
	}
	_, err := normalize.Normalize(files, normalize.Options{})
	if err == nil {
		t.Fatal("expected error for multiple main funcs, got nil")
	}
	if !strings.Contains(err.Error(), "main") {
		t.Errorf("error should mention main, got: %v", err)
	}
}

func TestNormalize_MultiFile_NoRootFilesRejected(t *testing.T) {
	files := map[string][]byte{
		"helper/util.go": []byte(`package helper` + "\n" + `func Hi() {}`),
	}
	_, err := normalize.Normalize(files, normalize.Options{})
	if err == nil {
		t.Fatal("expected error for no root files, got nil")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("error should mention root, got: %v", err)
	}
}

func TestNormalize_EmptyInputRejected(t *testing.T) {
	_, err := normalize.Normalize(nil, normalize.Options{})
	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

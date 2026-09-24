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

// benchimporter times how long it takes to obtain a fully type-checked
// *types.Package — the input yaegi's extractor needs — using export data
// via go/packages, instead of the source importer yaegi hardcodes.
//
// yaegi/extract/extract.go:450 does:
//
//	importer.ForCompiler(token.NewFileSet(), "source", nil).Import(pkgIdent)
//
// The "source" importer parses and type-checks the entire transitive
// dependency graph from source, in-process, ignoring the Go build cache
// completely. For k8s.io/client-go/kubernetes that ran past 50 minutes.
//
// go/packages with NeedTypes reads compiled export data instead, so the
// work is done once by the Go build cache and reused. This measures the
// difference. Both runs are reported: the first may have to compile
// export data, the second should be nearly free.
//
//	go run ./benchimporter k8s.io/client-go/kubernetes
package main

import (
	"fmt"
	"go/types"
	"os"
	"time"

	"golang.org/x/tools/go/packages"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: benchimporter <import-path>...")
		os.Exit(2)
	}

	for _, path := range os.Args[1:] {
		for pass := 1; pass <= 2; pass++ {
			start := time.Now()
			pkg, err := loadTypes(path)
			elapsed := time.Since(start)

			if err != nil {
				fmt.Printf("%-40s pass %d  FAILED after %v: %v\n", path, pass, elapsed.Round(time.Millisecond), err)
				break
			}
			fmt.Printf("%-40s pass %d  %8v  %d exported symbols, %d direct imports\n",
				path, pass, elapsed.Round(time.Millisecond),
				countExported(pkg), len(pkg.Imports()))
		}
	}
}

// loadTypes returns the same thing yaegi's extractor gets from the source
// importer: a complete *types.Package with its dependencies resolved.
func loadTypes(path string) (*types.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
	}
	pkgs, err := packages.Load(cfg, path)
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %q", path)
	}
	p := pkgs[0]
	if len(p.Errors) > 0 {
		return nil, p.Errors[0]
	}
	if p.Types == nil {
		return nil, fmt.Errorf("%s: no type information returned", path)
	}
	return p.Types, nil
}

func countExported(p *types.Package) int {
	var n int
	scope := p.Scope()
	for _, name := range scope.Names() {
		if scope.Lookup(name).Exported() {
			n++
		}
	}
	return n
}

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

package fastextract

import (
	"fmt"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"
)

// loadPackage returns a fully type-checked package for importPath, the
// same thing yaegi's source importer returns — but sourced from compiled
// export data via the Go build cache rather than re-parsing the whole
// dependency graph from source.
//
// Measured against the source importer: k8s.io/client-go/kubernetes goes
// from ">12 hours, never completed" to ~2 s; compute/apiv1 from 52 s to
// ~2.4 s.
//
// NeedDeps + NeedTypes is what makes the types of imported packages
// available, which genContent needs when it walks exported signatures.
func loadPackage(importPath string) (*types.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
	}

	pkgs, err := packages.Load(cfg, importPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", importPath, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %q", importPath)
	}
	if len(pkgs) > 1 {
		return nil, fmt.Errorf("%q matched %d packages, want exactly 1", importPath, len(pkgs))
	}

	p := pkgs[0]

	// packages.Load reports load/type errors per-package rather than
	// returning them, and a partially-typed package would silently yield
	// an incomplete symbol table — much worse than failing here.
	if len(p.Errors) > 0 {
		msgs := make([]string, 0, len(p.Errors))
		for _, e := range p.Errors {
			msgs = append(msgs, e.Error())
		}
		return nil, fmt.Errorf("type-check %s:\n\t%s", importPath, strings.Join(msgs, "\n\t"))
	}
	if p.Types == nil {
		return nil, fmt.Errorf("%s: no type information returned", importPath)
	}
	if !p.Types.Complete() {
		return nil, fmt.Errorf("%s: incomplete type information", importPath)
	}

	return p.Types, nil
}

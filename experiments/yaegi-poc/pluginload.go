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
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"sort"
	"strings"

	"github.com/traefik/yaegi/interp"
)

// loadPluginSymbols dlopens every *.so under the directories named in
// KG_SYMBOL_PLUGINS (colon-separated) and merges their exported `Symbols`
// tables. Answers whether a yaegi symbol set can ship as a mountable
// artifact — an OCI image volume, say — instead of being linked into the
// runner binary at build time.
//
// Go plugins are strict: the .so and the host binary must be built from
// identical versions of every package they share, with the same toolchain
// and build flags. A mismatch fails at Open() with a loud error rather
// than misbehaving, which is the failure mode we want.
func loadPluginSymbols() (interp.Exports, []string, error) {
	spec := os.Getenv("KG_SYMBOL_PLUGINS")
	if spec == "" {
		return nil, nil, nil
	}

	merged := interp.Exports{}
	var loaded []string

	for _, dir := range strings.Split(spec, ":") {
		if dir == "" {
			continue
		}
		sos, err := filepath.Glob(filepath.Join(dir, "*.so"))
		if err != nil {
			return nil, nil, fmt.Errorf("glob %s: %w", dir, err)
		}
		sort.Strings(sos)

		for _, so := range sos {
			p, err := plugin.Open(so)
			if err != nil {
				return nil, nil, fmt.Errorf("open %s: %w", so, err)
			}
			sym, err := p.Lookup("Symbols")
			if err != nil {
				return nil, nil, fmt.Errorf("lookup Symbols in %s: %w", so, err)
			}
			exports, ok := sym.(*interp.Exports)
			if !ok {
				return nil, nil, fmt.Errorf("%s: Symbols is %T, want *interp.Exports", so, sym)
			}
			for path, syms := range *exports {
				merged[path] = syms
			}
			loaded = append(loaded, fmt.Sprintf("%s(%d pkgs)", filepath.Base(so), len(*exports)))
		}
	}

	return merged, loaded, nil
}

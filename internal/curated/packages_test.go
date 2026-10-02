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

package curated

import (
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// TestPrewarmMatches: the packages the sandbox image precompiles (the
// blank imports of internal/prewarm, a separate module) are exactly the
// curated set, which the model is told about and lookup_package_docs
// serves. A package only in Packages would be advertised as fast but
// build slowly; one only in prewarm would cost image size for nothing.
func TestPrewarmMatches(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../prewarm/main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var prewarm []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		prewarm = append(prewarm, path)
	}
	curated := slices.Clone(Packages)
	slices.Sort(prewarm)
	slices.Sort(curated)
	if !slices.Equal(prewarm, curated) {
		t.Errorf("internal/prewarm/main.go imports and curated.Packages differ:\n  prewarm: %v\n  curated: %v", prewarm, curated)
	}
}

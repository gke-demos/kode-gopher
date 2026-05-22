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
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestNeedsTar(t *testing.T) {
	cases := []struct {
		name  string
		files map[string][]byte
		want  bool
	}{
		{"flat", map[string][]byte{"main.go": nil, "go.mod": nil}, false},
		{"nested", map[string][]byte{"main.go": nil, "sub/x.go": nil}, true},
		{"empty", map[string][]byte{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsTar(c.files); got != c.want {
				t.Errorf("needsTar(%v) = %v, want %v", c.files, got, c.want)
			}
		})
	}
}

func TestBuildTar_EmitsParentDirsThenFiles(t *testing.T) {
	files := map[string][]byte{
		"main.go":           []byte("package main\n"),
		"sub/x.go":          []byte("package sub\n"),
		"sub/inner/y.go":    []byte("package inner\n"),
	}
	archive, err := buildTar(files)
	if err != nil {
		t.Fatalf("buildTar: %v", err)
	}
	r := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		names = append(names, h.Name)
	}
	// Sorted-keys + parent-dirs-first guarantees a stable order.
	want := []string{"main.go", "sub/", "sub/inner/", "sub/inner/y.go", "sub/x.go"}
	if len(names) != len(want) {
		t.Fatalf("entries: got %d want %d (%v)", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry %d: got %q want %q (full %v)", i, names[i], want[i], names)
		}
	}
}

func TestValidatePath(t *testing.T) {
	good := []string{"main.go", "sub/x.go", "a/b/c.txt"}
	bad := []string{"", "/abs", "a//b", "a/./b", "a/../b"}
	for _, p := range good {
		if err := validatePath(p); err != nil {
			t.Errorf("validatePath(%q) unexpectedly errored: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := validatePath(p); err == nil {
			t.Errorf("validatePath(%q) should have errored", p)
		}
	}
}

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
	"reflect"
	"strings"
	"testing"
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

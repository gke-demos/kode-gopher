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

package version

import (
	"runtime/debug"
	"testing"
)

var unset = Info{Version: "dev", Commit: "none", Date: "unknown"}

func TestResolve(t *testing.T) {
	checkout := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
			{Key: "vcs.time", Value: "2026-10-01T12:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	tests := []struct {
		name string
		ld   Info
		bi   *debug.BuildInfo
		want Info
	}{
		{"no build info", unset, nil, unset},
		{
			"ldflags win over build info",
			Info{Version: "v0.1.0", Commit: "abc", Date: "2026-10-01T00:00:00Z"},
			checkout,
			Info{Version: "v0.1.0", Commit: "abc", Date: "2026-10-01T00:00:00Z"},
		},
		{
			"checkout build uses vcs settings",
			unset, checkout,
			Info{Version: "dev", Commit: "0123456789abcdef0123456789abcdef01234567", Date: "2026-10-01T12:00:00Z", Dirty: true},
		},
		{
			"go install module@version uses the module version",
			unset, &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0-rc.1"}},
			Info{Version: "v0.2.0-rc.1", Commit: "none", Date: "unknown"},
		},
		{
			"empty injected commit counts as not injected",
			Info{Version: "v0.1.0", Commit: "", Date: "unknown"}, checkout,
			Info{Version: "v0.1.0", Commit: "0123456789abcdef0123456789abcdef01234567", Date: "2026-10-01T12:00:00Z", Dirty: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.ld, tc.bi); got != tc.want {
				t.Errorf("resolve() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		in   Info
		want string
	}{
		{unset, "kode-gopher dev"},
		{
			Info{Version: "v0.1.0", Commit: "0123456789abcdef", Date: "2026-10-01T00:00:00Z"},
			"kode-gopher v0.1.0 (commit 01234567, built 2026-10-01T00:00:00Z)",
		},
		{Info{Version: "dev", Commit: "abc", Date: "unknown", Dirty: true}, "kode-gopher dev (commit abc, modified)"},
		{Info{Version: "v0.1.0", Commit: "none", Date: "2026-10-01T00:00:00Z"}, "kode-gopher v0.1.0 (built 2026-10-01T00:00:00Z)"},
	}
	for _, tc := range tests {
		if got := format("kode-gopher", tc.in); got != tc.want {
			t.Errorf("format(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveNotEmpty(t *testing.T) {
	if Effective() == "" {
		t.Fatal("Effective() is empty")
	}
}

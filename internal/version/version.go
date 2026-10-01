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

// Package version reports the build identity of kode-gopher: the
// `kode-gopher version` subcommand and the MCP server's
// Implementation.Version both read it.
//
// Release builds get Version, Commit and Date from -ldflags, set by
// .goreleaser.yaml (keep the package path there in sync when moving
// these variables):
//
//	go build -ldflags "\
//	  -X github.com/gke-demos/kode-gopher/internal/version.Version=v0.1.0 \
//	  -X github.com/gke-demos/kode-gopher/internal/version.Commit=<sha> \
//	  -X github.com/gke-demos/kode-gopher/internal/version.Date=<RFC 3339>" \
//	  ./cmd/kode-gopher
//
// Without -ldflags the package falls back to what Go embeds in the
// binary (runtime/debug.ReadBuildInfo): the module version for
// `go install module@version` builds, and the VCS revision, time and
// dirty flag for builds from a git checkout. See docs/release-process.md.
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
)

// Build-time metadata. The defaults are sentinels meaning "not
// injected"; release builds override all three via -ldflags.
var (
	// Version is the SemVer tag (e.g. v0.1.0, v0.2.0-rc.1).
	Version = "dev"
	// Commit is the full git SHA the binary was built from.
	Commit = "none"
	// Date is the build (commit) time, RFC 3339.
	Date = "unknown"
)

// Info is the resolved build identity.
type Info struct {
	Version string
	Commit  string // "none" when unknown
	Date    string // "unknown" when unknown
	Dirty   bool   // built from a modified checkout
}

// Get returns the build identity: -ldflags values where injected,
// debug.ReadBuildInfo otherwise. Cached; the inputs are fixed at link
// time.
func Get() Info { return get() }

var get = sync.OnceValue(func() Info {
	bi, _ := debug.ReadBuildInfo()
	return resolve(Info{Version: Version, Commit: Commit, Date: Date}, bi)
})

// Effective is the version token alone, for surfaces that advertise a
// version string (MCP Implementation.Version).
func Effective() string { return Get().Version }

// String renders the identity for `<prog> version`:
//
//	<prog> <version> (commit <8-char sha>[, modified], built <date>)
//
// Unknown fields are omitted, and the parenthesized suffix with them
// when nothing is known.
func String(prog string) string { return format(prog, Get()) }

// resolve fills the not-injected fields of ld from bi (which may be
// nil). Split from Get so tests can pass a constructed BuildInfo.
func resolve(ld Info, bi *debug.BuildInfo) Info {
	if bi == nil {
		return ld
	}
	out := ld
	if out.Version == "dev" || out.Version == "" {
		out.Version = "dev"
		if mv := bi.Main.Version; mv != "" && mv != "(devel)" {
			out.Version = mv
		}
	}
	injected := ld.Commit != "none" && ld.Commit != ""
	if injected {
		return out
	}
	out.Commit = "none"
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && s.Value != "":
			out.Commit = s.Value
		case s.Key == "vcs.time" && s.Value != "" && (ld.Date == "unknown" || ld.Date == ""):
			out.Date = s.Value
		case s.Key == "vcs.modified" && s.Value == "true":
			out.Dirty = true
		}
	}
	return out
}

func format(prog string, in Info) string {
	var parts []string
	if in.Commit != "none" && in.Commit != "" {
		c := "commit " + in.Commit[:min(8, len(in.Commit))]
		if in.Dirty {
			c += ", modified"
		}
		parts = append(parts, c)
	}
	if in.Date != "unknown" && in.Date != "" {
		parts = append(parts, "built "+in.Date)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s %s", prog, in.Version)
	}
	return fmt.Sprintf("%s %s (%s)", prog, in.Version, strings.Join(parts, ", "))
}

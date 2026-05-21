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

// Package curated is the canonical list of GCP packages whose build
// artifacts are pre-cached in the sandbox image's $GOCACHE and whose
// module sources are pre-downloaded into $GOMODCACHE.
//
// Anything in this list compiles "instantly" inside the sandbox (no
// network for module fetch, no compile time for the package + transitive
// deps). Anything *not* in this list pays the full cold cost on first
// use — and may exceed the upstream agent-sandbox HTTP layer's ~60s
// per-call response-header cap. See docs/decisions.md.
//
// This file is the single source of truth:
//   - internal/prewarm/main.go must blank-import every entry below
//     (keep manually in sync; v1 is small enough that go-generate
//     scaffolding isn't worth it yet).
//   - Later slices: internal/prompts/system.md is generated from this
//     list, and lookup_package_docs uses it as an allow-list.
package curated

// Packages is the set of GCP SDK packages baked into the kode-gopher
// sandbox image. Add sparingly; each entry adds image size + build
// time.
var Packages = []string{
	"cloud.google.com/go/storage",
	"cloud.google.com/go/compute/apiv1",
	"cloud.google.com/go/container/apiv1",
	"cloud.google.com/go/bigquery",
	"cloud.google.com/go/secretmanager/apiv1",
	"google.golang.org/api/option",
}

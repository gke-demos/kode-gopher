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

// Prewarm imports every package in internal/curated/packages.go so that
// `go build` against this main:
//   - downloads each module + transitive deps into $GOMODCACHE
//   - compiles each package's object code into $GOCACHE
//
// Building it inside sandbox/Dockerfile under USER 1000 (the same user
// the runtime sandbox uses) ensures the caches end up at paths the
// runtime can read.
//
// KEEP IN SYNC with internal/curated/packages.go. Blank imports are
// sufficient — the compiler builds & caches the imported package even
// without referenced symbols.
package main

import (
	_ "cloud.google.com/go/bigquery"
	_ "cloud.google.com/go/compute/apiv1"
	_ "cloud.google.com/go/container/apiv1"
	_ "cloud.google.com/go/secretmanager/apiv1"
	_ "cloud.google.com/go/storage"
	_ "google.golang.org/api/option"
)

func main() {}

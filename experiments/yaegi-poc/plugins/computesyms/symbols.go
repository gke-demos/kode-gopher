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

// Plugin-packaged yaegi symbols for cloud.google.com/go/compute/apiv1.
//
// Built as a Go plugin rather than linked into the runner:
//
//	go build -buildmode=plugin -o /tmp/computesyms.so ./plugins/computesyms
//
// The runner dlopens it and looks up `Symbols`. This is the experiment
// behind "can symbol sets live outside the core binary and be mounted in
// (e.g. via an OCI image volume) rather than baked into every build?"
package main

import "github.com/traefik/yaegi/interp"

// Symbols is populated by the generated extract files' init() functions,
// exactly as in the statically-linked runner. Exported so plugin.Lookup
// can find it.
var Symbols = interp.Exports{}

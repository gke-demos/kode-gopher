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

// Package wrapper holds the generated main() that the normalizer ships
// into the sandbox alongside a snippet-style user program.
//
// The wrapper is a real Go file (wrapper.go.tmpl — the .tmpl suffix is
// purely so the host toolchain ignores it when building this module)
// that lives next to the user's main.go in the sandbox at runtime.
// They compile together as package main: the wrapper provides
// `func main()`, the user provides `func run(ctx) (any, error)`.
package wrapper

import _ "embed"

// Filename is the name we materialize the wrapper under in the sandbox.
// Distinctive prefix so users can't accidentally collide with it from
// their own snippet.
const Filename = "kg_wrapper.go"

//go:embed wrapper.go.tmpl
var source []byte

// Source returns the wrapper source bytes. It's a copy so callers
// can't mutate the embedded data.
func Source() []byte {
	out := make([]byte, len(source))
	copy(out, source)
	return out
}

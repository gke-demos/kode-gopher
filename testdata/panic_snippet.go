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

// panic_snippet.go is a slice-1 pass-criterion check: a snippet whose
// run() panics. Expectation:
//
//   - phase = run  (the wrapper recovered the panic, so the binary
//     exited cleanly; the build/run distinction is unchanged)
//   - exit code = 0  (wrapper's own main() succeeded)
//   - result.kind = "panic"
//   - result.message contains "kode-gopher demo panic"
//   - result.stack is non-empty
//
// Not run by the default smoketest pair — invoked ad-hoc to exercise
// the panic discriminator.
package kode_gopher_snippet

import "context"

func run(ctx context.Context) (any, error) {
	panic("kode-gopher demo panic")
}

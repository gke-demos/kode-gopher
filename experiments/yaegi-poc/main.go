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

// experiments/yaegi-poc — local-only proof of concept answering whether
// the Yaegi Go interpreter can run kode-gopher-shaped snippets without
// the compile step.
//
// Convention: snippets are `package main` files with a `func main()`
// that does the whole job — call the SDK, marshal a result, print it.
// Mirrors our compiled-path wrapper. Yaegi runs main() automatically
// when EvalPath sees a main package.
//
// Usage:
//
//	go run . testdata/hello.go                            # stdlib-only baseline
//	go run -tags rest . testdata/list_buckets_rest.go     # needs yaegi-extract symbols
//	go run -tags grpc . testdata/list_buckets_grpc.go     # needs yaegi-extract symbols
//
// See README.md for findings.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: yaegi-poc <snippet.go>")
		os.Exit(2)
	}
	path := os.Args[1]

	startSetup := time.Now()
	i := interp.New(interp.Options{
		Env: os.Environ(),
	})
	if err := i.Use(stdlib.Symbols); err != nil {
		die("Use(stdlib): %v", err)
	}
	extras := extraSymbols()
	if len(extras) > 0 {
		if err := i.Use(extras); err != nil {
			die("Use(extras): %v", err)
		}
	}
	setupMS := time.Since(startSetup).Milliseconds()
	fmt.Fprintf(os.Stderr, "[poc] interpreter ready in %dms (stdlib + %d extra packages)\n", setupMS, len(extras))

	// EvalPath of a `package main` file with `func main()` will run
	// main() to completion (or panic). All snippet output goes to
	// stdout/stderr as normal.
	startRun := time.Now()
	_, err := i.EvalPath(path)
	runMS := time.Since(startRun).Milliseconds()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[poc] EvalPath failed in %dms: %v\n", runMS, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[poc] ok — setup=%dms eval+run=%dms total=%dms\n",
		setupMS, runMS, setupMS+runMS)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[poc] "+format+"\n", args...)
	os.Exit(1)
}

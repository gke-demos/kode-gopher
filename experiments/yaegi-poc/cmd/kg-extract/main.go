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

// kg-extract is a drop-in replacement for `yaegi extract` that loads type
// information from compiled export data instead of re-parsing every
// dependency from source. Same flags, same output filenames, so its
// output can be diffed directly against yaegi's.
//
//	kg-extract -name main -tag k8s k8s.io/client-go/kubernetes
//
// See fastextract/extract.go for why this exists.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gke-demos/kode-gopher/experiments/yaegi-poc/fastextract"
)

func main() {
	var (
		name    = flag.String("name", "", "name of the generated package")
		exclude = flag.String("exclude", "", "comma separated list of regexp matching symbols to exclude")
		include = flag.String("include", "", "comma separated list of regexp matching symbols to include")
		tag     = flag.String("tag", "", "comma separated list of build tags to be added")
		outDir  = flag.String("out", ".", "directory to write generated files into")
	)
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kg-extract [flags] <import-path>...")
		flag.PrintDefaults()
		os.Exit(2)
	}

	dest := *name
	if dest == "" {
		dest = "main"
	}

	ext := fastextract.Extractor{
		Dest:    dest,
		Exclude: splitList(*exclude),
		Include: splitList(*include),
		Tag:     splitList(*tag),
	}

	var failed bool
	for _, path := range paths {
		start := time.Now()

		var buf bytes.Buffer
		if _, err := ext.Extract(path, "", &buf); err != nil {
			fmt.Fprintf(os.Stderr, "kg-extract: %s: %v\n", path, err)
			failed = true
			continue
		}

		out := filepath.Join(*outDir, outputName(path))
		if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "kg-extract: write %s: %v\n", out, err)
			failed = true
			continue
		}

		fmt.Printf("%s -> %s (%d bytes, %v)\n",
			path, out, buf.Len(), time.Since(start).Round(time.Millisecond))
	}

	if failed {
		os.Exit(1)
	}
}

// outputName mirrors yaegi's filename convention so generated files land
// where the existing build-tagged extracts already live, and so output can
// be diffed against them.
func outputName(importPath string) string {
	return strings.NewReplacer("/", "-", ".", "_").Replace(importPath) + ".go"
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

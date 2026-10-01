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

package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/curated"
	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

// LookupPackageDocsArgs is the input schema.
type LookupPackageDocsArgs struct {
	Package string `json:"package" jsonschema:"Import path of a curated package to look up, e.g. 'cloud.google.com/go/storage' or 'k8s.io/client-go/kubernetes'. Only curated packages are supported — the tool returns a descriptive error listing them otherwise."`
	Symbol  string `json:"symbol,omitempty" jsonschema:"Optional specific symbol within the package to focus on, e.g. 'Client' or 'Client.Bucket'. If empty, returns the package-level overview. Must be a valid Go identifier (letters, digits, underscore, dot for method receivers)."`
}

// LookupPackageDocsOutput carries the raw godoc text — the LLM parses
// what it needs from there rather than us pre-structuring it.
type LookupPackageDocsOutput struct {
	Package string `json:"package"`
	Symbol  string `json:"symbol,omitempty"`
	Docs    string `json:"docs"`
}

// symbolPattern enforces the "valid Go identifier optionally dotted"
// shape on the Symbol arg. Blocks shell metacharacters — Symbol goes
// into an `sh -c` command line downstream, so we validate strictly
// rather than shell-escape.
var symbolPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func (s *Server) handleLookupPackageDocs(ctx context.Context, req *sdk.CallToolRequest, args LookupPackageDocsArgs) (*sdk.CallToolResult, *LookupPackageDocsOutput, error) {
	if args.Package == "" {
		return toolError("package is required (see curated list in execute_go_code's description)"), nil, nil
	}
	if !isCurated(args.Package) {
		return toolError(fmt.Sprintf(
			"package %q is not in the curated set. Curated packages: %s",
			args.Package, strings.Join(curated.Packages, ", "),
		)), nil, nil
	}
	if args.Symbol != "" && !symbolPattern.MatchString(args.Symbol) {
		return toolError(fmt.Sprintf(
			"symbol %q is not a valid Go identifier (letters, digits, underscore, dot for methods)",
			args.Symbol,
		)), nil, nil
	}

	// Serialize with execute_go_code — we share the session, and a
	// concurrent Reset from execute_go_code would race with our
	// `go doc` invocation (though we skip Reset ourselves, we still
	// need to not step on a concurrent build).
	sl := s.slotFor(req)
	sl.execMu.Lock()
	defer sl.execMu.Unlock()

	sess, err := s.ensureSession(ctx, sl)
	if err != nil {
		return toolError(err.Error()), nil, nil
	}

	// go doc in module mode requires a go.mod in CWD that requires the
	// target module. /app is empty post-Reset; /opt/kode-gopher-base
	// is where sandbox/Dockerfile preserves the prewarm's tidied go.mod
	// with every curated package as a require. Perfect CWD.
	cmd := fmt.Sprintf("cd /opt/kode-gopher-base && go doc %s", args.Package)
	if args.Symbol != "" {
		cmd = fmt.Sprintf("cd /opt/kode-gopher-base && go doc %s %s", args.Package, args.Symbol)
	}
	res, err := sess.Execute(ctx, sandbox.Request{
		Command: cmd,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return toolError(fmt.Sprintf("go doc: %v", err)), nil, nil
	}
	if res.ExitCode != 0 {
		text := res.Stderr
		if text == "" {
			text = res.Stdout
		}
		return toolError(fmt.Sprintf("go doc failed (exit %d): %s", res.ExitCode, text)), nil, nil
	}

	out := &LookupPackageDocsOutput{
		Package: args.Package,
		Symbol:  args.Symbol,
		Docs:    res.Stdout,
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: res.Stdout}},
	}, out, nil
}

func isCurated(pkg string) bool {
	for _, p := range curated.Packages {
		if p == pkg {
			return true
		}
	}
	return false
}

const lookupPackageDocsDescription = `Return godoc-style documentation for a curated Go package (optionally scoped to a specific symbol). Runs 'go doc <package> [symbol]' inside the sandbox against the prewarmed module cache — no build, no network, subsecond. Use when writing execute_go_code snippets and you need to check a function signature, method set, or constant name that isn't in your prior knowledge.

Curated set (only these packages are supported):

  cloud.google.com/go/storage
  cloud.google.com/go/bigquery
  cloud.google.com/go/compute/apiv1
  cloud.google.com/go/container/apiv1
  cloud.google.com/go/secretmanager/apiv1
  google.golang.org/api/option
  k8s.io/client-go/kubernetes
  k8s.io/client-go/tools/clientcmd
  k8s.io/client-go/dynamic
  k8s.io/client-go/tools/watch
  k8s.io/apimachinery/pkg/apis/meta/v1

The 'symbol' argument, when provided, narrows the output to that name (e.g. 'Client', 'Client.Bucket', 'NewClient'). Must be a plain Go identifier — no shell metacharacters.`

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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/curated"
	"github.com/gke-demos/kode-gopher/internal/prompts"
	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

const maxDocQueries = 10

var batchDocTruncateConfig = sandbox.TruncateConfig{
	HeadBytes: 4096,
	TailBytes: 4096,
}

// DocQuery is one (package, optional symbol) lookup inside a batched
// lookup_package_docs call.
type DocQuery struct {
	Package string `json:"package" jsonschema:"Import path of a prewarmed package to look up, e.g. 'cloud.google.com/go/storage' or 'k8s.io/api/core/v1'."`
	Symbol  string `json:"symbol,omitempty" jsonschema:"Optional symbol within the package, e.g. 'Client', 'Client.Bucket', or 'PodSpec'. Must be a valid Go identifier."`
}

// DocResult is one entry's go doc output in a batched lookup_package_docs response.
type DocResult struct {
	Package string `json:"package" jsonschema:"The package looked up."`
	Symbol  string `json:"symbol,omitempty" jsonschema:"The symbol looked up, if any."`
	Docs    string `json:"docs" jsonschema:"The output of go doc for this query."`
	Error   string `json:"error,omitempty" jsonschema:"Batch mode: go doc error for this query, when this entry failed."`
}

// LookupPackageDocsArgs is the input schema. Callers pass either
// Package (with optional Symbol) for a single lookup, or Queries to
// look up up to 10 packages/symbols in one sandbox round-trip.
type LookupPackageDocsArgs struct {
	Package string     `json:"package,omitempty" jsonschema:"Import path of a prewarmed package to look up, e.g. 'cloud.google.com/go/storage' or 'k8s.io/api/core/v1'. Any module in the sandbox's prewarmed go.mod is supported. Mutually exclusive with 'queries'."`
	Symbol  string     `json:"symbol,omitempty" jsonschema:"Optional specific symbol within 'package' to focus on, e.g. 'Client' or 'Client.Bucket'. If empty, returns the package-level overview. Must be a valid Go identifier (letters, digits, underscore, dot for method receivers). Only valid with 'package'."`
	Queries []DocQuery `json:"queries,omitempty" jsonschema:"Batch form: 1 to 10 {package, symbol?} lookups executed in a single sandbox round-trip. Mutually exclusive with 'package' and 'symbol'."`
}

// LookupPackageDocsOutput carries the raw godoc text — the LLM parses
// what it needs from there rather than us pre-structuring it.
type LookupPackageDocsOutput struct {
	Package string      `json:"package,omitempty" jsonschema:"Single-lookup mode: the package looked up."`
	Symbol  string      `json:"symbol,omitempty" jsonschema:"Single-lookup mode: the symbol looked up, if any."`
	Docs    string      `json:"docs" jsonschema:"The output of go doc (combined across entries when 'queries' is used)."`
	Results []DocResult `json:"results,omitempty" jsonschema:"Batch mode ('queries'): per-query go doc results in request order."`
}

// symbolPattern enforces the "valid Go identifier optionally dotted"
// shape on the Symbol arg. Blocks shell metacharacters — Symbol goes
// into an `sh -c` command line downstream, so we validate strictly
// rather than shell-escape.
var symbolPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func (s *Server) handleLookupPackageDocs(ctx context.Context, req *sdk.CallToolRequest, args LookupPackageDocsArgs) (*sdk.CallToolResult, *LookupPackageDocsOutput, error) {
	start := time.Now()
	queries, batch, errMsg := resolveDocQueries(args)
	status := "error"
	defer func() {
		s.cfg.Metrics.RecordLookup(ctx, time.Since(start), batch, status)
	}()
	if errMsg != "" {
		return toolError(errMsg), nil, nil
	}
	cmd, token, err := buildDocCommand(queries, batch)
	if err != nil {
		return toolError(fmt.Sprintf("go doc setup: %v", err)), nil, nil
	}

	res, err := s.runDocRequest(ctx, req, cmd, batch)
	if err != nil {
		return toolError(fmt.Sprintf("go doc: %v", err)), nil, nil
	}
	if res.ExitCode != 0 {
		text := res.Stderr
		if text == "" {
			text = res.Stdout
		}
		text, _ = sandbox.Truncate(text, sandbox.DefaultTruncateConfig)
		return toolError(fmt.Sprintf("go doc failed (exit %d): %s", res.ExitCode, text)), nil, nil
	}

	out, isErr, err := buildDocOutput(queries, batch, res.Stdout, token)
	if err != nil {
		return toolError(fmt.Sprintf("go doc: %v", err)), nil, nil
	}
	if !isErr {
		status = "ok"
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: out.Docs}},
		IsError: isErr,
	}, out, nil
}

func (s *Server) runDocRequest(ctx context.Context, req *sdk.CallToolRequest, cmd string, batch bool) (*sandbox.Result, error) {
	timeout := 30 * time.Second
	if batch {
		timeout = 60 * time.Second
	}
	// Raw: true skips whole-stdout head+tail truncation so batch
	// delimiters in the middle of multi-symbol output are never
	// elided; buildDocOutput truncates per entry instead.
	execReq := sandbox.Request{
		Command: cmd,
		Timeout: timeout,
		Raw:     true,
	}
	if s.testExec != nil {
		return s.testExec(ctx, execReq)
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
		return nil, err
	}
	res, err := sess.Execute(ctx, execReq)
	if err != nil && errors.Is(err, sandbox.ErrSessionDead) && ctx.Err() == nil {
		origErr := err
		log.Printf("lookup_package_docs: session dead, closing and retrying once: %v", err)
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 30*time.Second)
		s.dropSession(closeCtx, sl, sess)
		cancelClose()
		if sess2, e := s.ensureSession(ctx, sl); e == nil {
			if r2, e2 := sess2.Execute(ctx, execReq); e2 == nil {
				return r2, nil
			} else {
				log.Printf("lookup_package_docs: retry also failed: %v", e2)
				return nil, origErr
			}
		}
	}
	return res, err
}

func resolveDocQueries(args LookupPackageDocsArgs) (queries []DocQuery, batch bool, errMsg string) {
	hasSingle := args.Package != "" || args.Symbol != ""
	hasBatch := len(args.Queries) > 0
	if hasSingle && hasBatch {
		return nil, false, "provide either 'package' (with optional 'symbol') or 'queries', not both"
	}
	if !hasSingle && !hasBatch {
		return nil, false, "package or queries is required (see curated list in lookup_package_docs's description)"
	}
	if !hasBatch {
		if msg := validateDocQuery(DocQuery{Package: args.Package, Symbol: args.Symbol}); msg != "" {
			return nil, false, msg
		}
		return []DocQuery{{Package: args.Package, Symbol: args.Symbol}}, false, ""
	}
	if len(args.Queries) > maxDocQueries {
		return nil, false, fmt.Sprintf("at most %d queries per call (got %d)", maxDocQueries, len(args.Queries))
	}
	for _, q := range args.Queries {
		if msg := validateDocQuery(q); msg != "" {
			return nil, false, msg
		}
	}
	return args.Queries, true, ""
}

func validateDocQuery(q DocQuery) string {
	if q.Package == "" {
		return "package is required (see curated list in lookup_package_docs's description)"
	}
	if !isCurated(q.Package) {
		return fmt.Sprintf(
			"package %q is not in the sandbox's prewarmed module graph. Curated entry-point packages: %s",
			q.Package, strings.Join(curated.Packages, ", "),
		)
	}
	if q.Symbol != "" && !symbolPattern.MatchString(q.Symbol) {
		return fmt.Sprintf(
			"symbol %q is not a valid Go identifier (letters, digits, underscore, dot for methods)",
			q.Symbol,
		)
	}
	return ""
}

// buildDocCommand builds a single shell command executed in
// /opt/kode-gopher-base, where sandbox/Dockerfile preserves the
// prewarm's tidied go.mod. Batch queries run in one shell call with
// per-query exit capture and a per-call random boundary so package
// comments cannot collide with the delimiter and one typo'd symbol
// does not abort the rest of the batch.
func buildDocCommand(queries []DocQuery, batch bool) (cmd, token string, err error) {
	if !batch {
		return "cd /opt/kode-gopher-base && " + singleDocCmd(queries[0]), "", nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b[:])
	steps := make([]string, 0, 2*len(queries)-1)
	for i, q := range queries {
		if i > 0 {
			steps = append(steps, fmt.Sprintf("printf '\\n__KGDOC_%s__\\n'", token))
		}
		steps = append(steps, fmt.Sprintf("{ %s 2>&1; printf '\\n__KGEXIT_%s_%d:%%d__' \"$?\"; }", singleDocCmd(q), token, i))
	}
	return fmt.Sprintf("cd /opt/kode-gopher-base && { %s; }", strings.Join(steps, "; ")), token, nil
}

func singleDocCmd(q DocQuery) string {
	if q.Symbol != "" {
		return fmt.Sprintf("GOPROXY=off go doc %s %s", q.Package, q.Symbol)
	}
	return fmt.Sprintf("GOPROXY=off go doc %s", q.Package)
}

func buildDocOutput(queries []DocQuery, batch bool, stdout, token string) (*LookupPackageDocsOutput, bool, error) {
	if !batch {
		doc, _ := sandbox.Truncate(stdout, sandbox.DefaultTruncateConfig)
		return &LookupPackageDocsOutput{
			Package: queries[0].Package,
			Symbol:  queries[0].Symbol,
			Docs:    doc,
		}, false, nil
	}
	rawParts := []string{stdout}
	if len(queries) > 1 {
		sep := "\n__KGDOC_" + token + "__\n"
		rawParts = strings.Split(stdout, sep)
	}
	if len(rawParts) != len(queries) {
		return nil, true, fmt.Errorf("batch output truncated or malformed: got %d sections for %d queries", len(rawParts), len(queries))
	}

	results := make([]DocResult, len(queries))
	var combined strings.Builder
	anyOK := false
	for i, q := range queries {
		res, section, err := formatBatchDocEntry(rawParts[i], token, i, q)
		if err != nil {
			return nil, true, err
		}
		if res.Error == "" {
			anyOK = true
		}
		results[i] = res
		if i > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString(section)
	}
	return &LookupPackageDocsOutput{
		Docs:    combined.String(),
		Results: results,
	}, !anyOK, nil
}

func formatBatchDocEntry(part, token string, index int, q DocQuery) (DocResult, string, error) {
	body, exitCode, err := parseBatchPart(part, token, index, q.Package)
	if err != nil {
		return DocResult{}, "", err
	}
	header := q.Package
	if q.Symbol != "" {
		header += " " + q.Symbol
	}
	if exitCode != 0 {
		errText, _ := sandbox.Truncate(strings.TrimSpace(body), batchDocTruncateConfig)
		if errText == "" {
			errText = fmt.Sprintf("exit %d", exitCode)
		}
		msg := fmt.Sprintf("go doc failed (exit %d): %s", exitCode, errText)
		return DocResult{
			Package: q.Package,
			Symbol:  q.Symbol,
			Error:   msg,
		}, fmt.Sprintf("── %s (error) ──\n%s", header, ensureNewline(msg)), nil
	}
	doc, _ := sandbox.Truncate(body, batchDocTruncateConfig)
	return DocResult{
		Package: q.Package,
		Symbol:  q.Symbol,
		Docs:    doc,
	}, fmt.Sprintf("── %s ──\n%s", header, ensureNewline(doc)), nil
}

func parseBatchPart(part, token string, index int, pkg string) (body string, exitCode int, err error) {
	markerPrefix := fmt.Sprintf("\n__KGEXIT_%s_%d:", token, index)
	idx := strings.LastIndex(part, markerPrefix)
	if idx < 0 || !strings.HasSuffix(part, "__") {
		return "", 0, fmt.Errorf("query %d (%s): missing exit marker (output may have hit sandbox stream cap)", index, pkg)
	}
	codeStr := part[idx+len(markerPrefix) : len(part)-2]
	code, convErr := strconv.Atoi(codeStr)
	if convErr != nil {
		return "", 0, fmt.Errorf("query %d (%s): invalid exit marker %q", index, pkg, codeStr)
	}
	return part[:idx], code, nil
}

// packagePattern is a plain Go import path. The path is interpolated
// into a shell command, so isCurated requires it before any prefix
// match: no spaces, quotes, $, ; or other shell characters.
var packagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)

// isCurated reports whether pkg belongs to any module in the sandbox's
// prewarmed go.mod (prompts.PrewarmModules). The root umbrella module
// cloud.google.com/go is special-cased to its own packages (such as
// civil) so uncached sibling GCP modules (like cloud.google.com/go/pubsub)
// don't match by prefix.
func isCurated(pkg string) bool {
	if !packagePattern.MatchString(pkg) || strings.Contains(pkg, "..") {
		return false
	}
	for _, m := range prompts.PrewarmModules {
		if m == "cloud.google.com/go" {
			if isCloudUmbrellaPackage(pkg) {
				return true
			}
			continue
		}
		if pkg == m || strings.HasPrefix(pkg, m+"/") {
			return true
		}
	}
	return false
}

func isCloudUmbrellaPackage(pkg string) bool {
	for _, sub := range []string{"civil", "httpreplay", "rpcreplay"} {
		p := "cloud.google.com/go/" + sub
		if pkg == p || strings.HasPrefix(pkg, p+"/") {
			return true
		}
	}
	return pkg == "cloud.google.com/go"
}

// lookupPackageDocsDescription lists curated.Packages itself, so the
// tool never advertises a different set than the sandbox prewarms.
var lookupPackageDocsDescription = `Return godoc-style documentation for one or more Go packages in the sandbox's prewarmed module graph (optionally scoped to specific symbols). Runs 'go doc <package> [symbol]' inside the sandbox against the prewarmed module cache — no build, no network, subsecond. Use when writing execute_go_code snippets and you need to check a function signature, method set, struct fields, or constant names.

Pass either 'package' (with optional 'symbol') for one lookup, or 'queries' (up to 10 {package, symbol?} items) to inspect multiple types in a single round-trip.

Curated entry-point packages; every other package in their module graph works too, including *pb request/response types (monitoringpb, containerpb, iampb), k8s.io/api/* resource types, cloud.google.com/go/iam, cloud.google.com/go/civil, google.golang.org/genproto/..., and google.golang.org/grpc/...:

  ` + strings.Join(curated.Packages, "\n  ") + `

Each 'symbol', when provided, narrows output to that name (e.g. 'Client', 'Client.Bucket', 'PodSpec'). Must be a plain Go identifier — no shell metacharacters.`

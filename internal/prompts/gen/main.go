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

// gen produces internal/prompts/system.md and internal/prompts/description.go
// from internal/curated.Packages. Run via `make prompts` (which is
// `go generate ./...` in disguise). The two generated artifacts share
// one source of truth so a curated-set change updates both surfaces
// with a single command instead of the two-file update the pre-slice-4
// state needed.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/gke-demos/kode-gopher/internal/curated"
)

func main() {
	// Working directory is the directive's file location
	// (internal/curated/); we emit to ../prompts/.
	outDir, err := filepath.Abs(filepath.Join("..", "prompts"))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	packagesBlock := ""
	for _, p := range curated.Packages {
		packagesBlock += "  " + p + "\n"
	}
	modules, err := parsePrewarmModules()
	if err != nil {
		log.Fatalf("parse prewarm go.mod: %v", err)
	}
	data := struct {
		Packages     []string
		PackageBlock string
		Modules      []string
	}{
		Packages:     curated.Packages,
		PackageBlock: strings.TrimRight(packagesBlock, "\n"),
		Modules:      modules,
	}

	// system.md — LLM-facing prompt. Human-readable Markdown, safe to
	// paste into a Claude/Gemini/etc system prompt slot.
	sysMD, err := renderTemplate(systemMDTemplate, data)
	if err != nil {
		log.Fatalf("render system.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "system.md"), sysMD, 0o644); err != nil {
		log.Fatalf("write system.md: %v", err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(outDir, "system.md"), len(sysMD))

	// description.go — Go source with ExecuteGoCodeDescription as a
	// const. This is what internal/mcp/execute_go_code.go references
	// when registering the tool, so a curated-set change flows into
	// the tool description automatically.
	descGo, err := renderTemplate(descriptionGoTemplate, data)
	if err != nil {
		log.Fatalf("render description.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "description.go"), descGo, 0o644); err != nil {
		log.Fatalf("write description.go: %v", err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(outDir, "description.go"), len(descGo))

	// modules.go — Go source with PrewarmModules, every module required
	// by internal/prewarm/go.mod, so lookup_package_docs can allow any
	// module already present in the sandbox's module cache.
	modsGo, err := renderTemplate(modulesGoTemplate, data)
	if err != nil {
		log.Fatalf("render modules.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "modules.go"), modsGo, 0o644); err != nil {
		log.Fatalf("write modules.go: %v", err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", filepath.Join(outDir, "modules.go"), len(modsGo))

	// The docs site's reference page for the same set.
	pkgMD, err := renderTemplate(packagesMDTemplate, data)
	if err != nil {
		log.Fatalf("render packages.md: %v", err)
	}
	pkgPath, err := filepath.Abs(filepath.Join("..", "..", "docs", "site", "src", "content", "docs", "reference", "packages.md"))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(pkgPath, pkgMD, 0o644); err != nil {
		log.Fatalf("write packages.md: %v", err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", pkgPath, len(pkgMD))
}

func renderTemplate(tmpl string, data any) ([]byte, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parsePrewarmModules extracts every required module path (direct and
// indirect) from ../prewarm/go.mod via `go mod edit -json`, sorted.
func parsePrewarmModules() ([]string, error) {
	raw, err := exec.Command("go", "mod", "edit", "-json", "../prewarm/go.mod").Output()
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Require []struct {
			Path string `json:"Path"`
		} `json:"Require"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	mods := make([]string, 0, len(parsed.Require))
	for _, r := range parsed.Require {
		mods = append(mods, r.Path)
	}
	slices.Sort(mods)
	return slices.Compact(mods), nil
}

// systemMDTemplate is the LLM-facing prompt. Written in the second
// person because it's addressed to the model, not to a human reader.
const systemMDTemplate = "<!-- Code generated by internal/prompts/gen; DO NOT EDIT. Regenerate via `make prompts`. -->\n" +
	`# kode-gopher: how to write Go for the execute_go_code MCP tool

You have access to the ` + "`execute_go_code`" + ` MCP tool. It builds and runs Go code in a sandboxed Kubernetes pod (gVisor-isolated on GKE, runsc-free on local kind) and returns a structured result. You also have ` + "`gcp_auth_status`" + ` (report the sandbox's credential identity) and ` + "`lookup_package_docs`" + ` (fetch godoc for prewarmed packages and their dependencies).

## Prewarmed curated packages

The sandbox image ships a prewarmed ` + "`$GOCACHE`" + ` + ` + "`$GOMODCACHE`" + ` for exactly this set of packages plus their transitive deps:

{{.PackageBlock}}

Other packages from the same modules are compiled too: the generated ` + "`*pb`" + ` request and response types (` + "`monitoringpb`" + `, ` + "`tracepb`" + `, ` + "`containerpb`" + `, ...), ` + "`cloud.google.com/go/logging`" + `, ` + "`google.golang.org/api/iterator`" + ` and the well-known protobuf types. Snippets that stay within these modules build in ~5 seconds. A package from any other module triggers ` + "`go mod tidy`" + ` on first use — slower and may re-tidy shared transitives, invalidating the compile cache.

Use ` + "`lookup_package_docs`" + ` to check a function signature, method set or request field from any package in the sandbox's prewarmed module graph (pass ` + "`queries`" + ` to inspect multiple packages/symbols in one call) without paying an ` + "`execute_go_code`" + ` round-trip.

## Two input shapes

**Single-file** (` + "`code`" + ` arg): pass Go source as a string. Choose one of:

1. **Snippet mode (recommended)**. Any package name other than ` + "`main`" + `. Declare exactly:
   ` + "```go" + `
   func run(ctx context.Context) (any, error)
   ` + "```" + `
   Your return value is JSON-marshaled into ` + "`result.value`" + `. Errors and panics land as ` + "`result.kind=error`" + ` / ` + "`result.kind=panic`" + `. The wrapper provides ` + "`func main()`" + ` — don't write one.

2. **Full program mode**. ` + "`package main`" + ` with ` + "`func main()`" + `. Your code owns stdout/stderr. If you want a structured result, write JSON to ` + "`/app/.kode-gopher/result.json`" + ` before exiting.

**Multi-file** (` + "`files`" + ` arg): pass a ` + "`map[string]string`" + ` keyed by path. Root files (no ` + "`/`" + `) form the entry-point package. Subdirectory files are separate helper packages. The synthesized module name is ` + "`kode_gopher_user`" + `, so a helper at ` + "`helper/util.go`" + ` is imported as ` + "`kode_gopher_user/helper`" + `.

## Session scratch (` + "`/app/persist`" + `)

Each MCP session reuses one sandbox pod. Before every ` + "`execute_go_code`" + ` call, ` + "`/app`" + ` is wiped **except ` + "`/app/persist`" + `** (relative path ` + "`persist`" + `), which already exists and survives across calls in the same session alongside ` + "`$GOCACHE`" + ` and ` + "`$GOMODCACHE`" + `.

Use ` + "`/app/persist`" + ` in multi-turn investigations: save large intermediate datasets (JSON, CSV, filtered resource lists) to ` + "`/app/persist/...`" + ` and return a compact summary from ` + "`run`" + `, then read that file on follow-up turns instead of round-tripping large payloads through ` + "`result.value`" + ` (capped at 256 KiB) or re-querying APIs. Treat it as ephemeral cache — it goes away when the session ends or if a dead pod is replaced mid-session, so handle ` + "`os.ErrNotExist`" + ` cleanly, and never store credentials or tokens there.

## Credentials

Google Cloud calls authenticate with Application Default Credentials, which the client libraries find on their own: ` + "`storage.NewClient(ctx)`" + ` and the like need no credential options. Who that is depends on how kode-gopher runs: the developer running it locally, the user signed in to a shared server, the server's own Workload Identity, or a service account. Call ` + "`gcp_auth_status`" + ` to confirm the identity and project before consequential API calls.

Environment: ` + "`$GOOGLE_CLOUD_PROJECT`" + ` may be set; prefer it to hard-coding a project ID, and if it's empty, use the ` + "`project_id`" + ` that ` + "`gcp_auth_status`" + ` reports. Clients that need a project (such as ` + "`bigquery.NewClient`" + `) take it as an argument. Don't look for a credentials file or a token in the environment: there may be none. When kode-gopher runs in the cluster, a metadata server inside the sandbox hands the libraries a short-lived token for each run. For purely investigative Google Cloud calls, set ` + "`read_only: true`" + ` on ` + "`execute_go_code`" + ` when the credential mode supports OAuth scope narrowing (service-account impersonation, service-account or workload-identity-federation ADC in access-token mode, or a ` + "`--read-only`" + ` OAuth server); if ` + "`gcp_auth_status`" + ` reports ` + "`read_only=true`" + `, every run already carries read-only Google Cloud scopes.

## Kubernetes API access

The sandbox has no Kubernetes service-account token, so ` + "`rest.InClusterConfig()`" + ` fails, and the cluster's internal addresses (API server, cluster Services) are blocked. Reach every GKE cluster, including the one the sandbox runs in, through Google's API with the snippet's Google credentials: use ` + "`container/apiv1`" + ` ` + "`ListClusters`" + `/` + "`GetCluster`" + ` to find the cluster, prefer ` + "`c.ControlPlaneEndpointsConfig.DnsEndpointConfig.Endpoint`" + ` over ` + "`c.Endpoint`" + ` (the DNS endpoint routes through Google's control plane, works for private-endpoint clusters, uses system-trust TLS), and wrap an ` + "`oauth2.Transport`" + ` sourced from ` + "`google.DefaultTokenSource`" + ` as ` + "`rest.Config.WrapTransport`" + `. What the snippet may do in the cluster is whatever Kubernetes RBAC grants the credentials' Google identity.

## Keeping results small

` + "`result.value`" + ` (or ` + "`/app/.kode-gopher/result.json`" + `) is capped at 256 KiB; anything larger is dropped with a warning. Filter, aggregate, and project API responses inside ` + "`run(ctx)`" + ` into small structs, maps, or slices carrying only the fields needed to answer the question. Avoid returning raw Google Cloud protobuf structs (such as ` + "`*containerpb.Cluster`" + `) or full Kubernetes list objects (` + "`*corev1.PodList`" + `, whose ` + "`metadata.managedFields`" + ` and ` + "`last-applied-configuration`" + ` annotations routinely exceed 256 KiB) verbatim.

## Failure surfaces and image attachments

The tool returns ` + "`phase`" + ` (build/run), ` + "`exit_code`" + `, ` + "`stdout`" + `, ` + "`stderr`" + `, and ` + "`result`" + ` (kind + value/message/stack). ` + "`IsError`" + ` is true when either the program crashed or ` + "`result.kind != \"ok\"`" + `.

To return rendered charts or downloaded images without base64-bloating JSON, create ` + "`/app/.kode-gopher/attachments/`" + ` and write regular PNG, JPEG, or WebP files there (up to 8 images and 2 MiB total); ` + "`execute_go_code`" + ` surfaces them as native MCP ` + "`ImageContent`" + ` blocks alongside ` + "`attachments`" + ` metadata.
`

// packagesMDTemplate is the docs site's reference page for the
// curated set.
const packagesMDTemplate = `---
title: Precompiled packages
description: The Go packages the sandbox image has precompiled, so snippets that use them build in seconds.
---

<!-- Code generated by internal/prompts/gen; DO NOT EDIT. Regenerate via ` + "`make prompts`" + `. -->

The sandbox image ships with these packages, and everything they depend on, already downloaded and compiled. A snippet that imports only these builds in a few seconds, with ` + "`tidied: false`" + ` in the result:

| Package |
|---|
{{range .Packages}}| ` + "`{{.}}`" + ` |
{{end}}
The list lives in ` + "`internal/curated`" + `. The same list feeds the ` + "`execute_go_code`" + ` tool description, so the model sees it too, and ` + "`lookup_package_docs`" + ` serves ` + "`go doc`" + ` for these packages and every module they depend on.

**Other packages work too.** Any pure-Go import is allowed. The precompiled set is a speed optimization, not a limit. A snippet that imports something else makes the build run ` + "`go mod tidy`" + ` first (` + "`tidied: true`" + `), downloading and compiling the new package. The sandbox keeps its module and build caches for the rest of the MCP session, so later calls are faster. cgo isn't available: the image has no C compiler.

**Pinning versions.** With the ` + "`files`" + ` argument, a snippet can include its own ` + "`go.mod`" + `. kode-gopher then uses those versions instead of the image's lockfile, and the build pays for any recompilation.
`

const descriptionGoTemplate = `// Code generated by internal/prompts/gen; DO NOT EDIT. Regenerate via ` + "`make prompts`" + `.

package prompts

// ExecuteGoCodeDescription is the tool description for execute_go_code.
// Generated from internal/curated.Packages so a curated-set change
// updates both this string and internal/prompts/system.md in one shot.
const ExecuteGoCodeDescription = ` + "`" + `Build and run Go code in a sandboxed Kubernetes pod (gVisor-isolated on GKE, runsc-free on local kind). The pod has the Go toolchain plus a prewarmed cache of curated Google Cloud + Kubernetes client packages:

{{.PackageBlock}}

The snippet's go.mod is inherited from the prewarm lockfile so builds cache-hit at steady state (~5 s). Other packages in the same modules (the *pb request types, cloud.google.com/go/logging, iterator) are compiled too; a package from any other module is resolved on demand via go mod tidy (slower; may re-tidy shared transitives and cache-miss).

Two input modes. Provide EXACTLY ONE:

1) SINGLE-FILE via 'code': a Go source blob. Either a snippet declaring 'func run(ctx context.Context) (any, error)' in any package other than 'main' (the wrapper provides func main(); your return value becomes result.value), OR a complete 'package main' program (you own stdout/stderr; write /app/.kode-gopher/result.json for structured output).

2) MULTI-FILE via 'files': map of path -> source. Root files (no '/' in key) share the entry-point package; subdirectory files are separate helper packages. The synthesized module name is 'kode_gopher_user' — a helper at 'helper/util.go' is imported as 'kode_gopher_user/helper'. If you include a 'go.mod' file the executor uses YOUR version pins instead of the prewarm lockfile; you take on any resulting recompile cost.

Session scratch: before each call, /app is wiped EXCEPT /app/persist ('persist/'), which already exists and survives across execute_go_code calls in the same MCP session. Save large intermediate API/query results to /app/persist/... and return a compact summary from run() rather than exceeding the 256 KiB result cap or re-fetching on follow-up turns. Ephemeral: lost when the session ends or if an evicted pod is replaced, so handle os.ErrNotExist.

Google Cloud calls authenticate automatically with Application Default Credentials, so cloud.google.com/go/* clients need no credential options. The identity depends on the deployment (the local developer, the signed-in user, the server's Workload Identity, or a service account); call gcp_auth_status to see it and its project. GOOGLE_CLOUD_PROJECT may be set; if it's empty, use gcp_auth_status's project_id.

Structured results are capped at 256 KiB (larger payloads are dropped with a warning). Filter, aggregate, and project API responses inside run(ctx) into small structs or slices rather than returning raw *pb structs or full Kubernetes objects (such as corev1.PodList with metadata.managedFields) verbatim.

Image attachments: write PNG, JPEG, or WebP files under /app/.kode-gopher/attachments/ (up to 8 regular files, 2 MiB total) to return them as native MCP ImageContent blocks instead of base64-encoding images in JSON.` + "`" + `
`

const modulesGoTemplate = `// Code generated by internal/prompts/gen; DO NOT EDIT. Regenerate via ` + "`make prompts`" + `.

package prompts

// PrewarmModules is every module required (directly or indirectly) by
// internal/prewarm/go.mod, sorted. Every package in these modules is
// present in the sandbox image's $GOMODCACHE, so lookup_package_docs
// can serve offline go doc for them.
var PrewarmModules = []string{
{{range .Modules}}	{{printf "%q" .}},
{{end}}}
`

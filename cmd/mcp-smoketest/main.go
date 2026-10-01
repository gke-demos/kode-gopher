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

// cmd/mcp-smoketest spawns `kode-gopher serve` as a subprocess, speaks
// MCP to it over stdio (or streamable HTTP with --transport=http), and
// exercises execute_go_code against two test files (verbatim +
// wrapped). Validates the MCP layer end-to-end without involving an
// LLM.
//
// --offline runs only the checks that need no Google credentials (CI's
// kind e2e, dev/ci/e2e/kind.sh): a snippet that calls no Google API, a
// build error, a panic, gcp_auth_status reporting mode=none, and
// lookup_package_docs.
//
// Usage:
//
//	go build -o ./bin/kode-gopher ./cmd/kode-gopher
//	go build -o ./bin/mcp-smoketest ./cmd/mcp-smoketest
//	GOOGLE_CLOUD_PROJECT=... ./bin/mcp-smoketest --namespace=codemode
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	serverPath := flag.String("server", "./bin/kode-gopher", "path to kode-gopher binary")
	namespace := flag.String("namespace", "default", "k8s namespace (must already exist; the SandboxTemplate must be deployed there)")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall test timeout")
	compareNames := flag.Bool("compare", false, "fetch `gcloud storage buckets list` and assert the snippet result matches name order")
	offline := flag.Bool("offline", false, "run only the checks that need no Google credentials (kode-gopher must have none)")
	transport := flag.String("transport", "stdio", "how to reach kode-gopher serve: stdio, or http (a local streamable HTTP server with a generated static token)")
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("mcp-smoketest: ")

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var session *sdk.ClientSession
	var err error
	switch *transport {
	case "stdio":
		session, err = connect(ctx, *serverPath, *namespace)
	case "http":
		session, err = connectHTTP(ctx, *serverPath, *namespace, nil)
	default:
		err = fmt.Errorf("--transport must be stdio or http, got %q", *transport)
	}
	if err != nil {
		fatalf("connect: %v", err)
	}
	defer func() {
		_ = session.Close()
		if stopServer != nil {
			stopServer()
		}
	}()

	// 1. Tool discovery — must advertise execute_go_code, gcp_auth_status,
	// and lookup_package_docs (slice 4 registers the latter two alongside
	// the original).
	tools, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		fatalf("list tools: %v", err)
	}
	fmt.Printf("tools advertised: %d\n", len(tools.Tools))
	expected := map[string]bool{"execute_go_code": false, "gcp_auth_status": false, "lookup_package_docs": false}
	for _, t := range tools.Tools {
		fmt.Printf("  - %s — %s\n", t.Name, firstLine(t.Description))
		if _, ok := expected[t.Name]; ok {
			expected[t.Name] = true
		}
	}
	for name, seen := range expected {
		if !seen {
			fatalf("%s not advertised by server", name)
		}
	}
	if *offline {
		runOffline(ctx, session)
		return
	}

	// 2. Exercise the test files (verbatim GCS + wrapped GCS + GKE
	// composition) and parse the structured output from each. The
	// gke-compose case proves the container/apiv1 and k8s.io/client-go
	// additions to the curated set are prewarmed, and that a snippet can
	// reach a GKE cluster with forwarded Google credentials.
	var snippetResult []map[string]any
	for _, tc := range []struct{ label, path string }{
		{"verbatim", "testdata/list_buckets.go"},
		{"wrapped", "testdata/list_buckets_snippet.go"},
		{"gke-compose", "testdata/list_gke_pods_snippet.go"},
	} {
		out, err := callExecuteGoCode(ctx, session, tc.path)
		if err != nil {
			fatalf("[%s] call: %v", tc.label, err)
		}
		fmt.Printf("\n========== %s (%s) ==========\nphase=%s mode=%s exit=%d duration=%dms\n",
			tc.label, tc.path, out.Phase, out.Mode, out.ExitCode, out.DurationMS)
		if out.ExitCode != 0 {
			fatalf("[%s] exit=%d (expected 0). stderr:\n%s", tc.label, out.ExitCode, out.Stderr)
		}
		switch tc.label {
		case "wrapped":
			// Wrapper produces a structured result; assert kind=ok
			// and extract the bucket list for the compare step.
			if out.Result == nil {
				fatalf("[%s] expected non-nil result (wrapper should always populate)", tc.label)
			}
			if out.Result.Kind != "ok" {
				fatalf("[%s] expected result.kind=ok, got %q (message=%q)", tc.label, out.Result.Kind, out.Result.Message)
			}
			if err := json.Unmarshal(out.Result.Value, &snippetResult); err != nil {
				fatalf("[%s] decode result.value as []bucket: %v", tc.label, err)
			}
			fmt.Printf("  result.kind=ok  value has %d buckets\n", len(snippetResult))
		case "verbatim":
			// Verbatim mode: user code prints JSON to stdout, no
			// structured result (the test program doesn't write
			// result.json). Parse stdout directly to assert it
			// produced *something* sensible.
			var stdoutResult []map[string]any
			if err := json.Unmarshal([]byte(out.Stdout), &stdoutResult); err != nil {
				fatalf("[%s] decode stdout as []bucket: %v\nstdout:\n%s", tc.label, err, out.Stdout)
			}
			fmt.Printf("  stdout has %d buckets\n", len(stdoutResult))
		case "k8s":
			// Wrapped snippet returning k8s *version.Info. Assert
			// result.kind=ok and that a couple of stable fields
			// (major, gitVersion) are populated — proves the
			// discovery client round-tripped to the API server.
			if out.Result == nil {
				fatalf("[%s] expected non-nil result", tc.label)
			}
			if out.Result.Kind != "ok" {
				fatalf("[%s] expected result.kind=ok, got %q (message=%q)", tc.label, out.Result.Kind, out.Result.Message)
			}
			var info map[string]any
			if err := json.Unmarshal(out.Result.Value, &info); err != nil {
				fatalf("[%s] decode result.value as version.Info: %v", tc.label, err)
			}
			major, _ := info["major"].(string)
			git, _ := info["gitVersion"].(string)
			if major == "" || git == "" {
				fatalf("[%s] version.Info missing expected fields (major=%q, gitVersion=%q); full=%v", tc.label, major, git, info)
			}
			fmt.Printf("  result.kind=ok  server major=%s gitVersion=%s\n", major, git)
		case "gke-compose":
			// The cross-API composition demo: container/apiv1
			// ListClusters + client-go pods.List in one snippet.
			// Assert result.kind=ok, cluster field populated, and
			// >=1 pod returned with non-empty namespace+name.
			if out.Result == nil {
				fatalf("[%s] expected non-nil result", tc.label)
			}
			if out.Result.Kind != "ok" {
				fatalf("[%s] expected result.kind=ok, got %q (message=%q)", tc.label, out.Result.Kind, out.Result.Message)
			}
			var res struct {
				Cluster      string           `json:"cluster"`
				Location     string           `json:"location"`
				Endpoint     string           `json:"endpoint"`
				EndpointKind string           `json:"endpoint_kind"`
				Pods         []map[string]any `json:"pods"`
			}
			if err := json.Unmarshal(out.Result.Value, &res); err != nil {
				fatalf("[%s] decode result.value: %v", tc.label, err)
			}
			if res.Cluster == "" {
				fatalf("[%s] cluster field empty in result: %s", tc.label, string(out.Result.Value))
			}
			if len(res.Pods) == 0 {
				fatalf("[%s] expected >=1 pod in kube-system on cluster %s (result: %s)", tc.label, res.Cluster, string(out.Result.Value))
			}
			firstName, _ := res.Pods[0]["name"].(string)
			firstNS, _ := res.Pods[0]["namespace"].(string)
			if firstName == "" || firstNS == "" {
				fatalf("[%s] first pod missing name/namespace: %v", tc.label, res.Pods[0])
			}
			fmt.Printf("  result.kind=ok  cluster=%s (%s) endpoint=%s (%s) pods_in_kube_system=%d first=%s/%s\n",
				res.Cluster, res.Location, res.Endpoint, res.EndpointKind, len(res.Pods), firstNS, firstName)
		}
	}

	// 3. Optional: compare the snippet's bucket list against gcloud.
	if *compareNames {
		fmt.Println("\n========== compare against gcloud ==========")
		project := os.Getenv("GOOGLE_CLOUD_PROJECT")
		if project == "" {
			fatal("--compare requires GOOGLE_CLOUD_PROJECT")
		}
		gcloud, err := gcloudBucketNames(ctx, project)
		if err != nil {
			fatalf("gcloud reference: %v", err)
		}
		ours := bucketNamesSortedByTime(snippetResult)
		if diff := compareSlices(ours, gcloud); diff != "" {
			fatalf("snippet result differs from gcloud:\n%s", diff)
		}
		fmt.Printf("✅ match: %d buckets in same chronological order as gcloud\n", len(ours))
	}

	// 4. gcp_auth_status — proves creds.Source.Identity plumbing works
	// end-to-end (JSON parse, userinfo lookup, structured output).
	fmt.Println("\n========== gcp_auth_status ==========")
	auth := callAuthStatus(ctx, session)
	if auth.Email == "" {
		fatalf("gcp_auth_status: email empty — expected userinfo lookup to succeed for authorized_user creds (got %+v)", auth)
	}
	fmt.Printf("  mode=%s credential_type=%s email=%s project_id=%s\n", auth.Mode, auth.CredType, auth.Email, auth.ProjectID)

	// 5. lookup_package_docs — proves the tool registers, args validate,
	// and `go doc` inside the sandbox against /opt/kode-gopher-base
	// returns non-empty output for a curated package.
	fmt.Println("\n========== lookup_package_docs ==========")
	checkPackageDocs(ctx, session)

	// 6. Multi-file snippet — the `files` MCP arg with a root file +
	// helper subpackage. Proves normalize's multi-file plumbing works
	// end-to-end and that helper subpackages resolve under the
	// kode_gopher_user module name the executor synthesizes.
	fmt.Println("\n========== multi-file snippet ==========")
	multiFiles := map[string]any{
		"main.go":             readSnippet("testdata/multi_file_helper_snippet/main.go"),
		"formatter/format.go": readSnippet("testdata/multi_file_helper_snippet/formatter/format.go"),
	}
	mfRes, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "execute_go_code",
		Arguments: map[string]any{
			"files": multiFiles,
		},
	})
	if err != nil {
		fatalf("multi-file call: %v", err)
	}
	if mfRes.StructuredContent == nil {
		fatal("multi-file: missing structuredContent")
	}
	var mfOut executeGoCodeOutput
	if raw, e := json.Marshal(mfRes.StructuredContent); e == nil {
		if e2 := json.Unmarshal(raw, &mfOut); e2 != nil {
			fatalf("multi-file: decode structured: %v", e2)
		}
	}
	if mfOut.ExitCode != 0 {
		fatalf("multi-file: exit=%d stderr=\n%s", mfOut.ExitCode, mfOut.Stderr)
	}
	if mfOut.Result == nil || mfOut.Result.Kind != "ok" {
		fatalf("multi-file: expected result.kind=ok, got %+v", mfOut.Result)
	}
	var mfVal struct {
		Count   int      `json:"count"`
		Buckets []string `json:"buckets"`
	}
	if err := json.Unmarshal(mfOut.Result.Value, &mfVal); err != nil {
		fatalf("multi-file: decode result.value: %v", err)
	}
	if mfVal.Count == 0 {
		fatalf("multi-file: expected >=1 bucket (project has none?); result=%s", string(mfOut.Result.Value))
	}
	// Formatter contract: each description must contain "d old)" — a signal
	// the helper subpackage actually ran (not just the root file).
	found := 0
	for _, d := range mfVal.Buckets {
		if strings.Contains(d, "d old)") {
			found++
		}
	}
	if found == 0 {
		fatalf("multi-file: no bucket description carries the formatter's 'd old)' suffix — helper subpackage may not have shipped (result: %s)", string(mfOut.Result.Value))
	}
	fmt.Printf("  result.kind=ok  buckets=%d formatter_applied=%d/%d first=%s\n",
		mfVal.Count, found, mfVal.Count, mfVal.Buckets[0])

	fmt.Println("\n✅ MCP smoketest complete")
}

type authStatus struct {
	Mode      string `json:"mode"`
	CredType  string `json:"credential_type"`
	Email     string `json:"email"`
	ProjectID string `json:"project_id"`
}

// callAuthStatus calls gcp_auth_status and decodes its structured
// output, which must name a mode.
func callAuthStatus(ctx context.Context, session *sdk.ClientSession) authStatus {
	res, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "gcp_auth_status",
		Arguments: map[string]any{},
	})
	if err != nil {
		fatalf("gcp_auth_status call: %v", err)
	}
	if res.StructuredContent == nil {
		fatal("gcp_auth_status: missing structuredContent")
	}
	var auth authStatus
	if raw, e := json.Marshal(res.StructuredContent); e == nil {
		_ = json.Unmarshal(raw, &auth)
	}
	if auth.Mode == "" {
		fatalf("gcp_auth_status: mode empty (structured=%v)", res.StructuredContent)
	}
	return auth
}

// checkPackageDocs proves lookup_package_docs registers, its args
// validate, and `go doc` inside the sandbox against
// /opt/kode-gopher-base returns real output for a curated package.
func checkPackageDocs(ctx context.Context, session *sdk.ClientSession) {
	docsRes, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "lookup_package_docs",
		Arguments: map[string]any{
			"package": "cloud.google.com/go/storage",
			"symbol":  "Client",
		},
	})
	if err != nil {
		fatalf("lookup_package_docs call: %v", err)
	}
	if docsRes.IsError {
		fatalf("lookup_package_docs returned IsError: %v", contentText(docsRes.Content))
	}
	var docs struct {
		Package string `json:"package"`
		Symbol  string `json:"symbol"`
		Docs    string `json:"docs"`
	}
	if raw, e := json.Marshal(docsRes.StructuredContent); e == nil {
		_ = json.Unmarshal(raw, &docs)
	}
	if !strings.Contains(docs.Docs, "type Client struct") {
		fatalf("lookup_package_docs: expected Docs to contain 'type Client struct'; got:\n%s", docs.Docs)
	}
	// Terse: first 3 lines is enough to confirm we got real godoc output.
	summary := docs.Docs
	if lines := strings.SplitN(summary, "\n", 4); len(lines) > 3 {
		summary = strings.Join(lines[:3], "\n") + "\n  [...]"
	}
	fmt.Printf("  package=%s symbol=%s docs=\n    %s\n", docs.Package, docs.Symbol, strings.ReplaceAll(summary, "\n", "\n    "))
}

// readSnippet reads a testdata source into a string for the
// multi-file `files` MCP arg. Fatal on error — smoketest bugs out
// immediately if testdata is missing.
func readSnippet(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func contentText(c []sdk.Content) string {
	for _, item := range c {
		if t, ok := item.(*sdk.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// connect spawns `kode-gopher serve --namespace=NS` and brings up an
// MCP session over its stdio.
func connect(ctx context.Context, serverPath, namespace string) (*sdk.ClientSession, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: "mcp-smoketest", Version: "0.1.0"}, nil)
	// #nosec G204 -- the operator's own flags.
	cmd := exec.CommandContext(ctx, serverPath, "serve", "--namespace="+namespace)
	cmd.Env = os.Environ() // pass GOOGLE_CLOUD_PROJECT etc. through
	cmd.Stderr = os.Stderr // surface server logs to our stderr live
	transport := &sdk.CommandTransport{Command: cmd}
	return client.Connect(ctx, transport, nil)
}

// executeGoCodeOutput mirrors the server's ExecuteGoCodeOutput so we
// can decode the StructuredContent of each call.
type executeGoCodeOutput struct {
	Phase      string `json:"phase"`
	Mode       string `json:"mode"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Tidied     bool   `json:"tidied"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Result     *struct {
		Kind    string          `json:"kind"`
		Value   json.RawMessage `json:"value"`
		Message string          `json:"message"`
		Stack   string          `json:"stack"`
		Type    string          `json:"type"`
	} `json:"result"`
}

func callExecuteGoCode(ctx context.Context, s *sdk.ClientSession, path string) (*executeGoCodeOutput, error) {
	code, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return callExecuteGoCodeSource(ctx, s, string(code))
}

func callExecuteGoCodeSource(ctx context.Context, s *sdk.ClientSession, code string) (*executeGoCodeOutput, error) {
	res, err := s.CallTool(ctx, &sdk.CallToolParams{
		Name:      "execute_go_code",
		Arguments: map[string]any{"code": code},
	})
	if err != nil {
		return nil, err
	}
	// res.StructuredContent is the server's typed Out value, sent
	// over the wire as a JSON object and arriving here as
	// map[string]any. Round-trip through json to get our typed view.
	if res.StructuredContent == nil {
		return nil, errors.New("response missing structuredContent")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, fmt.Errorf("remarshal structured content: %w", err)
	}
	var out executeGoCodeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode structured content: %w", err)
	}
	return &out, nil
}

// gcloudBucketNames runs `gcloud storage buckets list` and returns
// bucket names sorted by creation_time. Same logic as the bash
// smoketest's jq filter, but in-process so the smoketest is
// self-contained.
func gcloudBucketNames(ctx context.Context, project string) ([]string, error) {
	// #nosec G204 G702 -- the operator's own project.
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "buckets", "list", "--format=json", "--project="+project)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gcloud: %w", err)
	}
	var raw []struct {
		Name         string `json:"name"`
		CreationTime string `json:"creation_time"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode gcloud json: %w", err)
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].CreationTime < raw[j].CreationTime })
	names := make([]string, len(raw))
	for i, r := range raw {
		names[i] = r.Name
	}
	return names, nil
}

// bucketNamesSortedByTime takes the wrapper's result.value (decoded
// as []map[string]any from the {name, timeCreated} struct the snippet
// returns) and produces a chronologically-sorted name list.
func bucketNamesSortedByTime(items []map[string]any) []string {
	sort.Slice(items, func(i, j int) bool {
		ti, _ := items[i]["timeCreated"].(string)
		tj, _ := items[j]["timeCreated"].(string)
		return ti < tj
	})
	names := make([]string, len(items))
	for i, it := range items {
		names[i], _ = it["name"].(string)
	}
	return names
}

func compareSlices(got, want []string) string {
	if len(got) != len(want) {
		return fmt.Sprintf("length mismatch: got %d, want %d\n  got:  %v\n  want: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("differ at index %d:\n  got:  %q\n  want: %q\n(full got=%v, want=%v)", i, got[i], want[i], got, want)
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

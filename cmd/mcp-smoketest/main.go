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
// MCP to it over stdio, and exercises execute_go_code against two test
// files (verbatim + wrapped). Validates the MCP layer end-to-end
// without involving an LLM.
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
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("mcp-smoketest: ")

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	session, err := connect(ctx, *serverPath, *namespace)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// 1. Tool discovery — must advertise execute_go_code.
	tools, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		log.Fatalf("list tools: %v", err)
	}
	fmt.Printf("tools advertised: %d\n", len(tools.Tools))
	var foundExec bool
	for _, t := range tools.Tools {
		fmt.Printf("  - %s — %s\n", t.Name, firstLine(t.Description))
		if t.Name == "execute_go_code" {
			foundExec = true
		}
	}
	if !foundExec {
		log.Fatal("execute_go_code not advertised by server")
	}

	// 2. Exercise both test files (verbatim + wrapped) and parse the
	// structured output from each.
	var snippetResult []map[string]any
	for _, tc := range []struct{ label, path string }{
		{"verbatim", "testdata/list_buckets.go"},
		{"wrapped", "testdata/list_buckets_snippet.go"},
	} {
		out, err := callExecuteGoCode(ctx, session, tc.path)
		if err != nil {
			log.Fatalf("[%s] call: %v", tc.label, err)
		}
		fmt.Printf("\n========== %s (%s) ==========\nphase=%s mode=%s exit=%d duration=%dms\n",
			tc.label, tc.path, out.Phase, out.Mode, out.ExitCode, out.DurationMS)
		if out.ExitCode != 0 {
			log.Fatalf("[%s] exit=%d (expected 0). stderr:\n%s", tc.label, out.ExitCode, out.Stderr)
		}
		if tc.label == "wrapped" {
			// Wrapper produces a structured result; assert kind=ok
			// and extract the bucket list for the compare step.
			if out.Result == nil {
				log.Fatalf("[%s] expected non-nil result (wrapper should always populate)", tc.label)
			}
			if out.Result.Kind != "ok" {
				log.Fatalf("[%s] expected result.kind=ok, got %q (message=%q)", tc.label, out.Result.Kind, out.Result.Message)
			}
			if err := json.Unmarshal(out.Result.Value, &snippetResult); err != nil {
				log.Fatalf("[%s] decode result.value as []bucket: %v", tc.label, err)
			}
			fmt.Printf("  result.kind=ok  value has %d buckets\n", len(snippetResult))
		} else {
			// Verbatim mode: user code prints JSON to stdout, no
			// structured result (the test program doesn't write
			// result.json). Parse stdout directly to assert it
			// produced *something* sensible.
			var stdoutResult []map[string]any
			if err := json.Unmarshal([]byte(out.Stdout), &stdoutResult); err != nil {
				log.Fatalf("[%s] decode stdout as []bucket: %v\nstdout:\n%s", tc.label, err, out.Stdout)
			}
			fmt.Printf("  stdout has %d buckets\n", len(stdoutResult))
		}
	}

	// 3. Optional: compare the snippet's bucket list against gcloud.
	if *compareNames {
		fmt.Println("\n========== compare against gcloud ==========")
		project := os.Getenv("GOOGLE_CLOUD_PROJECT")
		if project == "" {
			log.Fatal("--compare requires GOOGLE_CLOUD_PROJECT")
		}
		gcloud, err := gcloudBucketNames(ctx, project)
		if err != nil {
			log.Fatalf("gcloud reference: %v", err)
		}
		ours := bucketNamesSortedByTime(snippetResult)
		if diff := compareSlices(ours, gcloud); diff != "" {
			log.Fatalf("snippet result differs from gcloud:\n%s", diff)
		}
		fmt.Printf("✅ match: %d buckets in same chronological order as gcloud\n", len(ours))
	}

	fmt.Println("\n✅ MCP smoketest complete")
}

// connect spawns `kode-gopher serve --namespace=NS` and brings up an
// MCP session over its stdio.
func connect(ctx context.Context, serverPath, namespace string) (*sdk.ClientSession, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: "mcp-smoketest", Version: "0.1.0"}, nil)
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
	res, err := s.CallTool(ctx, &sdk.CallToolParams{
		Name:      "execute_go_code",
		Arguments: map[string]any{"code": string(code)},
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

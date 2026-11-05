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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/prompts"
	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

func TestIsCurated(t *testing.T) {
	for pkg, want := range map[string]bool{
		// Curated packages themselves.
		"cloud.google.com/go/storage":                        true,
		"cloud.google.com/go/monitoring/apiv3/v2":            true,
		"golang.org/x/oauth2/google":                         true,
		"golang.org/x/sync/errgroup":                         true,
		"google.golang.org/protobuf/types/known/timestamppb": true,
		"k8s.io/api/apps/v1":                                 true,
		"k8s.io/api/core/v1":                                 true,
		"k8s.io/client-go/kubernetes":                        true,
		// Other packages in prewarmed modules (direct and indirect).
		"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb":   true,
		"cloud.google.com/go/trace/apiv1/tracepb":                true,
		"cloud.google.com/go/logging":                            true,
		"cloud.google.com/go/iam":                                true,
		"cloud.google.com/go/iam/apiv1/iampb":                    true,
		"cloud.google.com/go/longrunning/autogen/longrunningpb":  true,
		"cloud.google.com/go/civil":                              true,
		"cloud.google.com/go":                                    true,
		"golang.org/x/oauth2":                                    true,
		"google.golang.org/api/iterator":                         true,
		"google.golang.org/genproto/googleapis/api/monitoredres": true,
		"google.golang.org/grpc/codes":                           true,
		"google.golang.org/protobuf/types/known/durationpb":      true,
		"k8s.io/api/batch/v1":                                    true,
		"k8s.io/apimachinery/pkg/types":                          true,
		"sigs.k8s.io/yaml":                                       true,
		// Sibling GCP modules not in internal/prewarm/go.mod must not
		// match the root cloud.google.com/go umbrella module.
		"cloud.google.com/go/pubsub":          false,
		"cloud.google.com/go/storagetransfer": false,
		"cloud.google.com/go/run/apiv2":       false,
		"cloud.google.com/go/civilization":    false,
		// Not in any prewarmed module.
		"github.com/foo/bar":     false,
		"k8s.io/kubectl/pkg/cmd": false,
		"":                       false,
		// The path goes into a shell command line: none of these may pass.
		"cloud.google.com/go/storage;id":             false,
		"cloud.google.com/go/storage/$(id)":          false,
		"cloud.google.com/go/storage `id`":           false,
		"cloud.google.com/go/storage/../../etc":      false,
		"cloud.google.com/go/storage/x|cat /etc/pwd": false,
		"cloud.google.com/go/storage\nid":            false,
		"-cloud.google.com/go/storage":               false,
	} {
		if got := isCurated(pkg); got != want {
			t.Errorf("isCurated(%q) = %v, want %v", pkg, got, want)
		}
	}
}

func TestPrewarmModulesMatchGoMod(t *testing.T) {
	raw, err := exec.Command("go", "mod", "edit", "-json", "../../internal/prewarm/go.mod").Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var parsed struct {
		Require []struct {
			Path string `json:"Path"`
		} `json:"Require"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	mods := make([]string, 0, len(parsed.Require))
	for _, r := range parsed.Require {
		mods = append(mods, r.Path)
	}
	slices.Sort(mods)
	mods = slices.Compact(mods)
	if !slices.Equal(mods, prompts.PrewarmModules) {
		t.Errorf("prompts.PrewarmModules out of sync with internal/prewarm/go.mod (run `make prompts`)\ngot:  %v\nwant: %v",
			prompts.PrewarmModules, mods)
	}
}

func TestResolveDocQueries(t *testing.T) {
	t.Run("single package", func(t *testing.T) {
		qs, batch, errMsg := resolveDocQueries(LookupPackageDocsArgs{
			Package: "k8s.io/api/core/v1",
			Symbol:  "PodSpec",
		})
		if errMsg != "" || batch || len(qs) != 1 || qs[0].Package != "k8s.io/api/core/v1" || qs[0].Symbol != "PodSpec" {
			t.Fatalf("got qs=%v batch=%v err=%q", qs, batch, errMsg)
		}
	})

	t.Run("single-item batch stays in batch mode", func(t *testing.T) {
		qs, batch, errMsg := resolveDocQueries(LookupPackageDocsArgs{
			Queries: []DocQuery{{Package: "k8s.io/api/core/v1", Symbol: "PodSpec"}},
		})
		if errMsg != "" || !batch || len(qs) != 1 {
			t.Fatalf("got qs=%v batch=%v err=%q", qs, batch, errMsg)
		}
	})

	t.Run("valid multi-item batch", func(t *testing.T) {
		qs, batch, errMsg := resolveDocQueries(LookupPackageDocsArgs{
			Queries: []DocQuery{
				{Package: "k8s.io/api/core/v1", Symbol: "PodSpec"},
				{Package: "golang.org/x/sync/errgroup"},
			},
		})
		if errMsg != "" || !batch || len(qs) != 2 {
			t.Fatalf("got qs=%v batch=%v err=%q", qs, batch, errMsg)
		}
	})

	var tooMany []DocQuery
	for range maxDocQueries + 1 {
		tooMany = append(tooMany, DocQuery{Package: "cloud.google.com/go/storage"})
	}

	for name, args := range map[string]LookupPackageDocsArgs{
		"empty": {},
		"symbol without package": {
			Symbol: "Client",
		},
		"both package and queries": {
			Package: "cloud.google.com/go/storage",
			Queries: []DocQuery{{Package: "k8s.io/api/core/v1"}},
		},
		"both symbol and queries": {
			Symbol:  "Client",
			Queries: []DocQuery{{Package: "k8s.io/api/core/v1"}},
		},
		"too many queries": {
			Queries: tooMany,
		},
		"empty package in batch": {
			Queries: []DocQuery{
				{Package: "cloud.google.com/go/storage"},
				{Symbol: "Pod"},
			},
		},
		"uncached package in batch": {
			Queries: []DocQuery{
				{Package: "cloud.google.com/go/storage"},
				{Package: "cloud.google.com/go/pubsub"},
			},
		},
		"invalid symbol in batch": {
			Queries: []DocQuery{
				{Package: "cloud.google.com/go/storage", Symbol: "Client;id"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, errMsg := resolveDocQueries(args); errMsg == "" {
				t.Fatalf("expected validation error for %+v, got none", args)
			}
		})
	}
}

func TestBuildDocCommandAndTruncatedBoundaryDetection(t *testing.T) {
	t.Run("single lookup runs offline go doc", func(t *testing.T) {
		qs := []DocQuery{{Package: "k8s.io/api/core/v1", Symbol: "Pod"}}
		cmd, token, err := buildDocCommand(qs, false)
		if err != nil {
			t.Fatal(err)
		}
		if want := "cd /opt/kode-gopher-base && GOPROXY=off go doc k8s.io/api/core/v1 Pod"; cmd != want {
			t.Errorf("cmd = %q, want %q", cmd, want)
		}
		if token != "" {
			t.Errorf("token = %q, want empty", token)
		}
	})

	t.Run("batch tokens are unique per call", func(t *testing.T) {
		qs := []DocQuery{
			{Package: "k8s.io/api/core/v1", Symbol: "Pod"},
			{Package: "golang.org/x/sync/errgroup"},
		}
		_, tok1, err := buildDocCommand(qs, true)
		if err != nil {
			t.Fatal(err)
		}
		_, tok2, err := buildDocCommand(qs, true)
		if err != nil {
			t.Fatal(err)
		}
		if tok1 == "" || tok1 == tok2 {
			t.Fatalf("expected distinct non-empty tokens, got %q and %q", tok1, tok2)
		}
	})

	t.Run("missing boundary or exit marker fails loudly instead of misaligning results", func(t *testing.T) {
		qs := []DocQuery{
			{Package: "k8s.io/api/core/v1", Symbol: "PodSpec"},
			{Package: "k8s.io/api/core/v1", Symbol: "ServiceSpec"},
			{Package: "k8s.io/apimachinery/pkg/apis/meta/v1", Symbol: "ObjectMeta"},
		}
		_, tok, err := buildDocCommand(qs, true)
		if err != nil {
			t.Fatal(err)
		}
		sep := "\n__KGDOC_" + tok + "__\n"

		// Simulate whole-stdout elision deleting boundary 0 (leaving only 2 sections for 3 queries).
		elided := "type PodSpec struct {\n... [elided] ..." + sep + "type ObjectMeta struct {}\n\n__KGEXIT_" + tok + "_2:0__"
		if out, _, err := buildDocOutput(qs, true, elided, tok); err == nil {
			t.Fatalf("expected error on elided boundary, got %+v", out)
		}

		// Simulate stream cutoff removing the final exit marker while section count matches.
		cutoff := "type PodSpec struct {}\n\n__KGEXIT_" + tok + "_0:0__" + sep +
			"type ServiceSpec struct {}\n\n__KGEXIT_" + tok + "_1:0__" + sep +
			"type ObjectMeta struct { truncated..."
		if out, _, err := buildDocOutput(qs, true, cutoff, tok); err == nil {
			t.Fatalf("expected error on missing exit marker, got %+v", out)
		}
	})
}

func TestLookupPackageDocsOverMCP(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	baseDir := filepath.Join(tmp, "base")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}

	podSpecBody := "type PodSpec struct {\n" + strings.Repeat("\tField int // padding line for >20KiB doc output\n", 480) + "} // END_PODSPEC\n"
	serviceSpecBody := "type ServiceSpec struct {\n" + strings.Repeat("\tPort int // padding line for >12KiB doc output\n", 280) + "} // END_SERVICESPEC\n"
	objectMetaBody := "type ObjectMeta struct {\n\tName string\n\tNamespace string\n} // END_OBJECTMETA\n"

	if err := os.WriteFile(filepath.Join(tmp, "podspec.txt"), []byte(podSpecBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "servicespec.txt"), []byte(serviceSpecBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "objectmeta.txt"), []byte(objectMetaBody), 0o644); err != nil {
		t.Fatal(err)
	}

	stubGo := `#!/bin/sh
if [ "${GOPROXY:-}" != "off" ]; then
  echo "expected GOPROXY=off, got '${GOPROXY:-}'" >&2
  exit 99
fi
if [ "$1" != "doc" ]; then
  echo "unexpected subcommand: $1" >&2
  exit 98
fi
case "$2:${3:-}" in
  "cloud.google.com/go/storage:Client")
    printf 'type Client struct {}\n'
    ;;
  "k8s.io/api/core/v1:PodSpec")
    cat "` + filepath.Join(tmp, "podspec.txt") + `"
    ;;
  "k8s.io/api/core/v1:ServiceSpec")
    cat "` + filepath.Join(tmp, "servicespec.txt") + `"
    ;;
  "k8s.io/apimachinery/pkg/apis/meta/v1:ObjectMeta")
    cat "` + filepath.Join(tmp, "objectmeta.txt") + `"
    ;;
  "k8s.io/api/core/v1:NoSuchSymbol")
    printf 'doc: symbol NoSuchSymbol is not present in package k8s.io/api/core/v1\n' >&2
    exit 1
    ;;
  *)
    printf 'unexpected query %s %s\n' "$2" "${3:-}" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(stubGo), 0o755); err != nil {
		t.Fatal(err)
	}

	var lastReq sandbox.Request
	srv := New(Config{})
	srv.testExec = func(ctx context.Context, req sandbox.Request) (*sandbox.Result, error) {
		lastReq = req
		rewritten := strings.Replace(req.Command, "/opt/kode-gopher-base", baseDir, 1)
		cmd := exec.CommandContext(ctx, "sh", "-c", rewritten)
		cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				return nil, err
			}
			exitCode = ee.ExitCode()
		}
		resOut := stdout.String()
		resErr := stderr.String()
		if !req.Raw {
			resOut, _ = sandbox.Truncate(resOut, sandbox.DefaultTruncateConfig)
			resErr, _ = sandbox.Truncate(resErr, sandbox.DefaultTruncateConfig)
		}
		return &sandbox.Result{
			Stdout:   resOut,
			Stderr:   resErr,
			ExitCode: exitCode,
		}, nil
	}

	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.newSDKServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	decodeStructured := func(t *testing.T, res *sdk.CallToolResult) LookupPackageDocsOutput {
		t.Helper()
		if res.StructuredContent == nil {
			t.Fatalf("missing StructuredContent on result (IsError=%v, text=%q)", res.IsError, firstText(res))
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out LookupPackageDocsOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("validation rejects mutually exclusive package and queries", func(t *testing.T) {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{
			Name: "lookup_package_docs",
			Arguments: map[string]any{
				"package": "cloud.google.com/go/storage",
				"queries": []map[string]any{{"package": "k8s.io/api/core/v1"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError=true, got %+v", res)
		}
	})

	t.Run("single lookup >16KiB validates OutputSchema and truncates cleanly", func(t *testing.T) {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{
			Name: "lookup_package_docs",
			Arguments: map[string]any{
				"package": "k8s.io/api/core/v1",
				"symbol":  "PodSpec",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("unexpected IsError: %s", firstText(res))
		}
		if !lastReq.Raw || lastReq.Timeout != 30*time.Second {
			t.Fatalf("expected Raw=true Timeout=30s, got Raw=%v Timeout=%v", lastReq.Raw, lastReq.Timeout)
		}
		out := decodeStructured(t, res)
		if out.Package != "k8s.io/api/core/v1" || out.Symbol != "PodSpec" || out.Results != nil {
			t.Fatalf("unexpected metadata: %+v", out)
		}
		if !strings.HasPrefix(out.Docs, "type PodSpec struct {") || !strings.Contains(out.Docs, "bytes elided]") || !strings.Contains(out.Docs, "END_PODSPEC") {
			t.Fatalf("expected head+tail truncated PodSpec docs, got len=%d", len(out.Docs))
		}
	})

	t.Run("batch >35KiB preserves boundaries and truncates per query without misattribution", func(t *testing.T) {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{
			Name: "lookup_package_docs",
			Arguments: map[string]any{
				"queries": []map[string]any{
					{"package": "k8s.io/api/core/v1", "symbol": "PodSpec"},
					{"package": "k8s.io/api/core/v1", "symbol": "ServiceSpec"},
					{"package": "k8s.io/apimachinery/pkg/apis/meta/v1", "symbol": "ObjectMeta"},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("unexpected IsError: %s", firstText(res))
		}
		if !lastReq.Raw || lastReq.Timeout != 60*time.Second {
			t.Fatalf("expected Raw=true Timeout=60s, got Raw=%v Timeout=%v", lastReq.Raw, lastReq.Timeout)
		}
		out := decodeStructured(t, res)
		if len(out.Results) != 3 {
			t.Fatalf("len(Results) = %d, want 3", len(out.Results))
		}
		if !strings.HasPrefix(out.Results[0].Docs, "type PodSpec struct {") || !strings.Contains(out.Results[0].Docs, "END_PODSPEC") || strings.Contains(out.Results[0].Docs, "ServiceSpec") {
			t.Errorf("Results[0] corrupted: %q", out.Results[0].Docs)
		}
		if !strings.HasPrefix(out.Results[1].Docs, "type ServiceSpec struct {") || !strings.Contains(out.Results[1].Docs, "END_SERVICESPEC") || strings.Contains(out.Results[1].Docs, "ObjectMeta") {
			t.Errorf("Results[1] corrupted or misattributed: %q", out.Results[1].Docs)
		}
		if out.Results[2].Docs != objectMetaBody {
			t.Errorf("Results[2] = %q, want exact %q", out.Results[2].Docs, objectMetaBody)
		}
	})

	t.Run("batch isolates per-query failure without aborting valid queries", func(t *testing.T) {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{
			Name: "lookup_package_docs",
			Arguments: map[string]any{
				"queries": []map[string]any{
					{"package": "cloud.google.com/go/storage", "symbol": "Client"},
					{"package": "k8s.io/api/core/v1", "symbol": "NoSuchSymbol"},
					{"package": "k8s.io/apimachinery/pkg/apis/meta/v1", "symbol": "ObjectMeta"},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("partial batch failure should set IsError=false so valid docs are returned, got text=%s", firstText(res))
		}
		out := decodeStructured(t, res)
		if len(out.Results) != 3 {
			t.Fatalf("len(Results) = %d, want 3", len(out.Results))
		}
		if out.Results[0].Docs != "type Client struct {}\n" || out.Results[0].Error != "" {
			t.Errorf("Results[0] = %+v", out.Results[0])
		}
		if out.Results[1].Docs != "" || !strings.Contains(out.Results[1].Error, "NoSuchSymbol") {
			t.Errorf("Results[1] expected per-item error, got %+v", out.Results[1])
		}
		if out.Results[2].Docs != objectMetaBody || out.Results[2].Error != "" {
			t.Errorf("Results[2] = %+v", out.Results[2])
		}
	})

	t.Run("batch sets IsError when every query fails", func(t *testing.T) {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{
			Name: "lookup_package_docs",
			Arguments: map[string]any{
				"queries": []map[string]any{
					{"package": "k8s.io/api/core/v1", "symbol": "NoSuchSymbol"},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError=true when every batch query failed, got %+v", res)
		}
	})
}

func firstText(res *sdk.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

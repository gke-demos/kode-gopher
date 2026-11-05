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
	"encoding/json"
	"strings"
	"testing"

	"github.com/gke-demos/kode-gopher/internal/executor"
)

func TestRenderTextCompactResultAndPreservesSections(t *testing.T) {
	type podSummary struct {
		Namespace string   `json:"namespace"`
		Name      string   `json:"name"`
		Phase     string   `json:"phase"`
		Restarts  int32    `json:"restarts"`
		Filter    string   `json:"filter"`
		Labels    []string `json:"labels"`
	}
	pods := make([]podSummary, 20)
	for i := range pods {
		pods[i] = podSummary{
			Namespace: "kube-system",
			Name:      "coredns-abc",
			Phase:     "Running",
			Restarts:  0,
			Filter:    "restarts > 0 && phase != 'Succeeded'",
			Labels:    []string{"k8s-app=kube-dns", "tier=control-plane"},
		}
	}
	rawVal, err := json.Marshal(pods)
	if err != nil {
		t.Fatal(err)
	}

	out := &ExecuteGoCodeOutput{
		Phase:      "run",
		Mode:       "wrapped",
		ExitCode:   0,
		DurationMS: 420,
		BuildMS:    310,
		Tidied:     false,
		Warnings:   []string{"sample warning"},
		Stdout:     "logged line",
		Stderr:     "stderr note\n",
		Result:     toWireResult(&executor.Result{Kind: "ok", Value: rawVal}),
	}

	got := renderText(out)

	for _, want := range []string{
		"phase=run  mode=wrapped  exit=0  tidied=false  (420ms, build 310ms)\n",
		"\nwarning: sample warning\n",
		"\n── stdout ──\nlogged line\n",
		"\n── stderr ──\nstderr note\n",
		"\n── result ──\n",
		"restarts > 0 && phase != 'Succeeded'", // HTML characters not escaped
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderText missing %q in:\n%s", want, got)
		}
	}

	_, resultBlock, ok := strings.Cut(got, "\n── result ──\n")
	if !ok {
		t.Fatalf("missing result block in:\n%s", got)
	}
	if strings.Count(resultBlock, "\n") != 1 {
		t.Fatalf("expected single-line compact JSON in result block, got %d newlines:\n%s", strings.Count(resultBlock, "\n"), resultBlock)
	}
	var decoded Result
	if err := json.Unmarshal([]byte(resultBlock), &decoded); err != nil {
		t.Fatalf("result block is not valid JSON: %v\n%s", err, resultBlock)
	}

	// Verify compact encoding materially shrinks nested output vs 2-space indent.
	var indented bytes.Buffer
	enc := json.NewEncoder(&indented)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out.Result); err != nil {
		t.Fatal(err)
	}
	if len(resultBlock) >= indented.Len()*85/100 {
		t.Errorf("compact result (%d bytes) did not save at least 15%% vs indented (%d bytes)", len(resultBlock), indented.Len())
	}
}

func TestToWireResultDefensiveFallback(t *testing.T) {
	if toWireResult(nil) != nil {
		t.Fatal("expected nil for nil input")
	}
	w := toWireResult(&executor.Result{
		Kind:  "ok",
		Value: json.RawMessage("not-valid-json"),
	})
	if w.Value != "not-valid-json" {
		t.Fatalf("got %#v, want raw string fallback", w.Value)
	}
}

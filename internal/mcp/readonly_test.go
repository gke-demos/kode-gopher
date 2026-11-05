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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

func TestExecuteGoCodeReadOnlyFailsClosedBeforeSandboxOpen(t *testing.T) {
	ctx := context.Background()
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Namespace/KubeContext are intentionally invalid; read_only fail-closed
	// must reject before attempting sandbox.Open.
	srv := New(Config{
		Namespace:   "nonexistent",
		KubeContext: "nonexistent-context",
		Credentials: creds.NewForwarded(adc, nil),
	})
	res, out, err := srv.handleExecuteGoCode(ctx, &sdk.CallToolRequest{}, ExecuteGoCodeArgs{
		Code:     "package p\nimport \"context\"\nfunc run(ctx context.Context) (any, error) { return 1, nil }\n",
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unexpected protocol err: %v", err)
	}
	if out != nil || res == nil || !res.IsError {
		t.Fatalf("res=%+v out=%+v, want toolError", res, out)
	}
	txt := res.Content[0].(*sdk.TextContent).Text
	if !strings.Contains(txt, "forwarded mode") {
		t.Errorf("error text = %q, want forwarded mode explanation", txt)
	}
}

func TestGCPAuthStatusReadOnlyField(t *testing.T) {
	ctx := context.Background()
	roUser := &creds.OAuthUser{
		Token:    "tok-ro",
		Expiry:   time.Now().Add(time.Hour),
		Email:    "alice@example.com",
		Project:  "proj-1",
		ReadOnly: true,
	}
	srv := New(Config{Credentials: roUser})
	res, out, err := srv.handleGCPAuthStatus(ctx, &sdk.CallToolRequest{}, GCPAuthStatusArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.ReadOnly {
		t.Errorf("GCPAuthStatusOutput = %+v, want ReadOnly=true", out)
	}
	txt := res.Content[0].(*sdk.TextContent).Text
	if !strings.Contains(txt, "read_only=true") {
		t.Errorf("text = %q, want read_only=true", txt)
	}

	// Default non-read-only keeps legacy text formatting without read_only=false noise.
	rwUser := &creds.OAuthUser{
		Token:   "tok-rw",
		Expiry:  time.Now().Add(time.Hour),
		Email:   "alice@example.com",
		Project: "proj-1",
	}
	srvRW := New(Config{Credentials: rwUser})
	resRW, outRW, err := srvRW.handleGCPAuthStatus(ctx, &sdk.CallToolRequest{}, GCPAuthStatusArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if outRW.ReadOnly {
		t.Errorf("outRW.ReadOnly = true, want false")
	}
	if strings.Contains(resRW.Content[0].(*sdk.TextContent).Text, "read_only") {
		t.Errorf("unexpected read_only suffix in default text: %q", resRW.Content[0].(*sdk.TextContent).Text)
	}
}

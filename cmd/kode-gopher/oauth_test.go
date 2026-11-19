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

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gke-demos/kode-gopher/internal/mcp"
	"github.com/gke-demos/kode-gopher/internal/secretmgr"
)

func TestGroupsClientRejectsNonEmail(t *testing.T) {
	_, err := groupsClient(context.Background(), "kode-gopher-groups")
	if err == nil || !strings.Contains(err.Error(), "isn't a service account email") {
		t.Errorf("groupsClient(no @) error = %v, want a not-an-email error", err)
	}
}

func TestParseDomainMapAndProviders(t *testing.T) {
	ctx := context.Background()
	loader := &secretmgr.Loader{}
	m, err := parseDomainMap("Google.com=/etc/a.json, gke.ninja=/etc/b.json", "--flag")
	if err != nil || m["google.com"] != "/etc/a.json" || m["gke.ninja"] != "/etc/b.json" {
		t.Fatalf("parseDomainMap = %v, %v", m, err)
	}
	for _, bad := range []string{"no-equals", "=empty-domain", "google.com=", "a.com=1,A.COM=2"} {
		if _, err := parseDomainMap(bad, "--flag"); err == nil {
			t.Errorf("parseDomainMap(%q) succeeded, want error", bad)
		}
	}

	clientFile := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(clientFile, []byte(`{"web":{"client_id":"cid","client_secret":"csec"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildVaultProviders(
		ctx, loader,
		map[string]string{"google.com": clientFile},
		map[string]string{"google.com": "projects/p/locations/l/authProviders/corp"},
		nil, false,
	); err != nil {
		t.Fatalf("buildVaultProviders valid: %v", err)
	}
	if _, err := buildVaultProviders(
		ctx, loader,
		map[string]string{"google.com": clientFile},
		nil, nil, false,
	); err == nil {
		t.Error("buildVaultProviders missing vault auth provider succeeded")
	}
	if _, err := buildVaultProviders(
		ctx, loader,
		nil,
		map[string]string{"google.com": "projects/p/locations/l/authProviders/corp"},
		nil, false,
	); err == nil {
		t.Error("buildVaultProviders missing client file succeeded")
	}
	if _, err := buildSealedProviders(ctx, loader, map[string]string{"google.com": clientFile}, false); err != nil {
		t.Fatalf("buildSealedProviders valid: %v", err)
	}
}

func TestOAuthAndStaticTokenSecretManagerURIs(t *testing.T) {
	ctx := context.Background()
	key32 := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	secrets := map[string]string{
		"/v1/projects/p/secrets/static-token/versions/1:access":             "static-bearer-token-at-least-32-chars-long\n",
		"/v1/projects/p/secrets/short-token/versions/1:access":              "too-short",
		"/v1/projects/p/secrets/google-client/versions/2:access":            `{"web":{"client_id":"main-cid","client_secret":"main-csec"}}`,
		"/v1/projects/p/secrets/google-client-google_com/versions/1:access": `{"web":{"client_id":"corp-cid","client_secret":"corp-csec"}}`,
		"/v1/projects/p/secrets/keyring/versions/1:access":                  "k1 " + key32 + "\n",
		"/v1/projects/p/secrets/clients/versions/4:access":                  `[{"client_id":"cloud-live","client_name":"Cloud Live","redirect_uris":["https://cloud-live.example.com/oauth/callback"]}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		val, ok := secrets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payload": map[string]any{
				"data": base64.StdEncoding.EncodeToString([]byte(val)),
			},
		})
	}))
	defer srv.Close()

	loader := &secretmgr.Loader{Client: srv.Client(), Endpoint: srv.URL}

	// Static bearer token via sm:// URI.
	fsStatic := flag.NewFlagSet("serve-static", flag.ContinueOnError)
	ofStatic := addOAuthFlags(fsStatic)
	ofStatic.loader = loader
	var optsStatic mcp.HTTPOptions
	if err := httpAuth(ctx, &optsStatic, "static", "sm://projects/p/secrets/static-token/versions/1", "operator", false, false, ofStatic, nil); err != nil {
		t.Fatalf("httpAuth(static sm://) failed: %v", err)
	}
	if optsStatic.Verifier == nil {
		t.Fatal("expected static Verifier to be configured")
	}
	if err := httpAuth(ctx, &optsStatic, "static", "sm://projects/p/secrets/short-token/versions/1", "operator", false, false, ofStatic, nil); err == nil || !strings.Contains(err.Error(), "shorter than 32") {
		t.Fatalf("httpAuth(short sm:// token) err = %v, want shorter than 32", err)
	}

	// OAuth server (sealed custody, multi-domain + pre-registered clients) via sm:// URIs.
	fsOAuth := flag.NewFlagSet("serve-oauth", flag.ContinueOnError)
	ofOAuth := addOAuthFlags(fsOAuth)
	ofOAuth.loader = loader
	err := fsOAuth.Parse([]string{
		"--oauth-issuer=https://kg.example.com",
		"--oauth-custody=sealed",
		"--oauth-google-client-file=sm://projects/p/secrets/google-client/versions/2",
		"--oauth-domain-google-client-files=google.com=sm://projects/p/secrets/google-client-google_com/versions/1",
		"--oauth-keyring-file=sm://projects/p/secrets/keyring/versions/1",
		"--oauth-clients-file=sm://projects/p/secrets/clients/versions/4",
		"--oauth-allow-domains=example.com,google.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	var optsOAuth mcp.HTTPOptions
	if err := httpAuth(ctx, &optsOAuth, "oauth", "", "", true, false, ofOAuth, nil); err != nil {
		t.Fatalf("httpAuth(oauth sm://) failed: %v", err)
	}
	if optsOAuth.Routes == nil || optsOAuth.Protect == nil {
		t.Fatal("expected OAuth Routes and Protect to be configured")
	}
}

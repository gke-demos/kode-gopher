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

package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func dcrConfig() *auth.AuthorizationCodeHandlerConfig {
	return &auth.AuthorizationCodeHandlerConfig{DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
		Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "dcr-client", RedirectURIs: []string{clientRedirect}},
	}}
}

// whoamiFields checks whoami's "<sub> <email> <project> <token>".
func whoamiFields(t *testing.T, got string, sub, email string) {
	t.Helper()
	f := strings.Fields(got)
	if len(f) != 4 || f[0] != sub || f[1] != email || f[2] != "proj-1" || !strings.HasPrefix(f[3], "ya29.fake-") {
		t.Fatalf("whoami = %q, want %s %s proj-1 ya29.fake-...", got, sub, email)
	}
}

func TestE2EDCRVault(t *testing.T) {
	e := newTestEnv(t, false, nil)
	b := newBrowser(t)
	got, err := e.connect(b, dcrConfig())
	if err != nil {
		t.Fatal(err)
	}
	whoamiFields(t, got, alice.Sub, alice.Email)
	if !b.sawPage {
		t.Error("a DCR client skipped kode-gopher's consent page")
	}
	if a := e.google.lastAuth; a.Get("access_type") == "offline" || strings.Contains(a.Get("scope"), scopeCloudPlatform) {
		t.Errorf("vault custody sign-in asked Google for %q offline=%q; want identity only", a.Get("scope"), a.Get("access_type"))
	}
	if _, ok := e.vault.grants[alice.Sub]; !ok {
		t.Error("vault holds no grant for alice after consent")
	}
}

func TestE2EPreregisteredSealed(t *testing.T) {
	e := newTestEnv(t, true, func(c *Config) {
		c.Clients = []StaticClient{{ID: "cli", Secret: "s3cret", Name: "CLI", RedirectURIs: []string{"http://127.0.0.1/callback"}}}
		c.OpenRegistration = false
	})
	e.google.signInAs = bob // admitted by group, not domain
	b := newBrowser(t)
	got, err := e.connect(b, &auth.AuthorizationCodeHandlerConfig{PreregisteredClient: &oauthex.ClientCredentials{
		ClientID: "cli", ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: "s3cret"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	whoamiFields(t, got, bob.Sub, bob.Email)
	if b.sawPage {
		t.Error("a pre-registered client got the consent page")
	}
	if a := e.google.lastAuth; a.Get("access_type") != "offline" || !strings.Contains(a.Get("scope"), scopeCloudPlatform) {
		t.Errorf("sealed custody sign-in asked Google for %q offline=%q", a.Get("scope"), a.Get("access_type"))
	}
	// Closed registration: no DCR endpoint advertised or served.
	resp, err := http.Post(e.srv.URL+"/register", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /register with closed registration = %d, want 404", resp.StatusCode)
	}
}

func TestE2ECIMD(t *testing.T) {
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"client_id": docURL, "client_name": "cimd-client", "redirect_uris": []string{clientRedirect},
			"token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(doc.Close)
	docURL = doc.URL + "/client.json"
	e := newTestEnv(t, false, func(c *Config) { c.CIMDClient = doc.Client() })
	b := newBrowser(t)
	got, err := e.connect(b, &auth.AuthorizationCodeHandlerConfig{ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: docURL}})
	if err != nil {
		t.Fatal(err)
	}
	whoamiFields(t, got, alice.Sub, alice.Email)
	if !b.sawPage {
		t.Error("a CIMD client skipped kode-gopher's consent page")
	}
}

func TestE2ENotAllowed(t *testing.T) {
	e := newTestEnv(t, false, nil)
	e.google.signInAs = mallory
	_, err := e.connect(newBrowser(t), dcrConfig())
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("sign-in by a user outside the allow-list: err = %v, want access_denied", err)
	}
	if len(e.vault.grants) != 0 {
		t.Error("a refused user reached the vault")
	}
}

func TestE2EConsentDenied(t *testing.T) {
	e := newTestEnv(t, false, nil)
	b := newBrowser(t)
	b.deny = true
	_, err := e.connect(b, dcrConfig())
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("declined consent: err = %v, want access_denied", err)
	}
}

func TestE2EVaultWrongAccount(t *testing.T) {
	e := newTestEnv(t, false, nil)
	e.vault.consentAs = bob // alice signs in but consents as bob
	_, err := e.connect(newBrowser(t), dcrConfig())
	if err == nil {
		t.Fatal("sign-in succeeded with another account's Google grant")
	}
}

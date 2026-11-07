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
	"net/url"
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

func TestE2EDomainProvidersVault(t *testing.T) {
	carol := User{Sub: "444", Email: "carol@google.com", Domain: "google.com"}
	corpGoogle := newFakeGoogle(t)
	corpGoogle.signInAs = carol
	corpVault := newFakeVault(t, corpGoogle)
	corpVault.consentAs = carol

	e := newTestEnv(t, false, func(c *Config) {
		c.Allow.Domains = []string{"example.com", "google.com"}
		c.Clients = []StaticClient{{ID: "cloud-live", Name: "Cloud Live", RedirectURIs: []string{clientRedirect}}}
		c.Providers = map[string]Provider{
			"Google.com": {Google: corpGoogle.client(), Custody: corpVault.custody()},
		}
	})

	// 1. Pre-registered client with ?hd=google.com routes to corpGoogle + corpVault.
	bCorp := newBrowser(t)
	bCorp.hd = "google.com"
	got, err := e.connect(bCorp, &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: "cloud-live"},
	})
	if err != nil {
		t.Fatalf("corp sign-in failed: %v", err)
	}
	whoamiFields(t, got, carol.Sub, carol.Email)
	if gotHD := corpGoogle.lastAuth.Get("hd"); gotHD != "google.com" {
		t.Errorf("corpGoogle lastAuth hd = %q, want google.com", gotHD)
	}
	if _, ok := corpVault.grants[carol.Sub]; !ok {
		t.Error("corpVault holds no grant for carol")
	}
	if len(e.vault.grants) != 0 {
		t.Error("default vault was touched during google.com sign-in")
	}

	// 2. DCR client choosing "approve:google.com" on the consent page routes to corpGoogle + corpVault.
	bDCR := newBrowser(t)
	bDCR.action = "approve:google.com"
	got, err = e.connect(bDCR, dcrConfig())
	if err != nil {
		t.Fatalf("DCR corp sign-in failed: %v", err)
	}
	whoamiFields(t, got, carol.Sub, carol.Email)
	if !bDCR.sawPage {
		t.Error("DCR client skipped consent page")
	}

	// 3. Default sign-in without hd still routes alice@example.com to the default provider.
	got, err = e.connect(newBrowser(t), &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: "cloud-live"},
	})
	if err != nil {
		t.Fatalf("default sign-in failed: %v", err)
	}
	whoamiFields(t, got, alice.Sub, alice.Email)
	if _, ok := e.vault.grants[alice.Sub]; !ok {
		t.Error("default vault holds no grant for alice")
	}

	// 4. Refresh token for carol@google.com refreshes against corpVault.
	verifier := "v-" + strings.Repeat("a", 43)
	res, err := bCorp.fetcher(t.Context(), &auth.AuthorizationArgs{
		URL: e.authorizeURL("cloud-live", verifier, nil),
	})
	if err != nil {
		t.Fatalf("fetch code for refresh test: %v", err)
	}
	first := e.token(exchangeForm("cloud-live", res.Code, verifier), "", "")
	if first.Refresh == "" {
		t.Fatalf("missing refresh_token: %+v", first)
	}
	ref := e.token(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {"cloud-live"},
		"refresh_token": {first.Refresh},
	}, "", "")
	if ref.status != http.StatusOK || ref.Access == "" {
		t.Fatalf("refresh = %+v, want 200 with access token", ref)
	}

	// 5. Under VaultCustody, signing in without hd via the default identity
	// client as a user from a dedicated domain (carol@google.com) upgrades
	// the custody leg to that domain's vault provider (corpVault).
	delete(corpVault.grants, carol.Sub)
	e.google.mu.Lock()
	e.google.signInAs = carol
	e.google.mu.Unlock()
	bNoHD := newBrowser(t)
	if _, err := e.connect(bNoHD, &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: "cloud-live"},
	}); err != nil {
		t.Fatalf("vault sign-in without hd failed: %v", err)
	}
	if _, ok := corpVault.grants[carol.Sub]; !ok {
		t.Error("corpVault holds no grant after hd-less sign-in for carol@google.com")
	}
	e.google.mu.Lock()
	e.google.signInAs = alice
	e.google.mu.Unlock()

	// 6. Domain mismatch (requesting hd=google.com but signing in as alice@example.com) is refused.
	corpGoogle.mu.Lock()
	corpGoogle.signInAs = alice
	corpGoogle.mu.Unlock()
	bMismatch := newBrowser(t)
	bMismatch.hd = "google.com"
	if _, err := e.connect(bMismatch, &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: "cloud-live"},
	}); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("domain mismatch err = %v, want access_denied", err)
	}
}

func TestE2EDomainProvidersSealed(t *testing.T) {
	carol := User{Sub: "444", Email: "carol@google.com", Domain: "google.com"}
	corpGoogle := newFakeGoogle(t)
	corpGoogle.signInAs = carol

	e := newTestEnv(t, true, func(c *Config) {
		c.Allow.Domains = []string{"example.com", "google.com"}
		c.Clients = []StaticClient{{ID: "cloud-live", Name: "Cloud Live", RedirectURIs: []string{clientRedirect}}}
		c.Providers = map[string]Provider{
			"google.com": {
				Google:  corpGoogle.client(),
				Custody: SealedCustody{},
			},
		}
	})

	// 1. Signing in with hd=google.com routes through corpGoogle and refreshes against corpGoogle.
	bCorp := newBrowser(t)
	bCorp.hd = "google.com"
	verifier := "v-" + strings.Repeat("b", 43)
	res, err := bCorp.fetcher(t.Context(), &auth.AuthorizationArgs{
		URL: e.authorizeURL("cloud-live", verifier, nil),
	})
	if err != nil {
		t.Fatalf("sealed domain sign-in failed: %v", err)
	}
	first := e.token(exchangeForm("cloud-live", res.Code, verifier), "", "")
	if first.Refresh == "" {
		t.Fatalf("missing refresh_token: %+v", first)
	}
	ref := e.token(url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {"cloud-live"},
		"refresh_token": {first.Refresh},
	}, "", "")
	if ref.status != http.StatusOK || ref.Access == "" {
		t.Fatalf("sealed domain refresh = %+v, want 200 with access token", ref)
	}

	// 2. Under SealedCustody, signing in WITHOUT hd via the default Google client
	// as carol@google.com is refused at /callback so a default-client refresh
	// token cannot be sealed under a domain that refreshes against corpGoogle.
	e.google.mu.Lock()
	e.google.signInAs = carol
	e.google.mu.Unlock()
	bNoHD := newBrowser(t)
	if _, err := e.connect(bNoHD, &auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: &oauthex.ClientCredentials{ClientID: "cloud-live"},
	}); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("sealed hd-less sign-in for dedicated domain err = %v, want access_denied", err)
	}
}

func TestDomainProvidersValidation(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k1", Secret: []byte(strings.Repeat("k", 32))})
	base := func() Config {
		return Config{
			Issuer:  "https://kg.example.com",
			Google:  GoogleClient{ClientID: "id", ClientSecret: "sec"},
			Custody: SealedCustody{},
			Allow:   &AllowList{Domains: []string{"example.com"}},
			Keys:    keys,
		}
	}
	for name, mutate := range map[string]func(*Config){
		"bad domain": func(c *Config) {
			c.Providers = map[string]Provider{"not a domain": {Google: c.Google, Custody: SealedCustody{}}}
		},
		"missing client secret": func(c *Config) {
			c.Providers = map[string]Provider{"google.com": {Google: GoogleClient{ClientID: "id"}, Custody: SealedCustody{}}}
		},
		"nil custody": func(c *Config) {
			c.Providers = map[string]Provider{"google.com": {Google: c.Google}}
		},
		"readonly mismatch": func(c *Config) {
			c.Providers = map[string]Provider{"google.com": {Google: c.Google, Custody: SealedCustody{ReadOnly: true}}}
		},
		"duplicate folded domain": func(c *Config) {
			c.Providers = map[string]Provider{
				"google.com": {Google: c.Google, Custody: SealedCustody{}},
				"GOOGLE.COM": {Google: c.Google, Custody: SealedCustody{}},
			}
		},
		"insecure endpoint": func(c *Config) {
			g := c.Google
			g.AuthURL = "http://accounts.example.com/auth"
			c.Providers = map[string]Provider{"google.com": {Google: g, Custody: SealedCustody{}}}
		},
	} {
		cfg := base()
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
}

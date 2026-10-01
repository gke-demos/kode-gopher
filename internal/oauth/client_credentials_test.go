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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

const runnerSA = "runner@proj-sa.iam.gserviceaccount.com"

// fakeMinter stands in for creds.Impersonator.
type fakeMinter struct {
	mu    sync.Mutex
	calls []string
	fail  bool
}

func (m *fakeMinter) Token(_ context.Context, sa string) (*oauth2.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, sa)
	if m.fail {
		return nil, errors.New("permission denied")
	}
	return &oauth2.Token{AccessToken: fmt.Sprintf("ya29.fake-sa-%d", len(m.calls)), Expiry: time.Now().Add(time.Hour)}, nil
}

// newServiceEnv has a client-credentials client "ci" and a user client
// "cli" that also has a secret.
func newServiceEnv(t *testing.T) (*testEnv, *fakeMinter) {
	m := &fakeMinter{}
	e := newTestEnv(t, false, func(c *Config) {
		c.Clients = []StaticClient{
			{ID: "ci", Secret: "ci-secret", Name: "CI", ServiceAccount: runnerSA},
			{ID: "cli", Secret: "cli-secret", RedirectURIs: []string{clientRedirect}},
		}
		c.ServiceTokens = m
	})
	return e, m
}

func ccForm(set map[string]string) url.Values {
	f := url.Values{"grant_type": {"client_credentials"}}
	for k, v := range set {
		f.Set(k, v)
	}
	return f
}

func TestE2EClientCredentials(t *testing.T) {
	e, m := newServiceEnv(t)
	h, err := extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
		Credentials: &oauthex.ClientCredentials{ClientID: "ci", ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: "ci-secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := sdk.NewClient(&sdk.Implementation{Name: "ci", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: e.srv.URL + "/mcp", OAuthHandler: h, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "whoami"})
	if err != nil || res.IsError {
		t.Fatalf("whoami: %v %v", err, res)
	}
	got := res.Content[0].(*sdk.TextContent).Text
	if want := "client:ci " + runnerSA + " proj-1 ya29.fake-sa-1"; got != want {
		t.Errorf("whoami = %q, want %q", got, want)
	}
	if len(m.calls) != 1 || m.calls[0] != runnerSA {
		t.Errorf("minted for %v, want [%s]", m.calls, runnerSA)
	}
	if e.google.lastAuth != nil || len(e.vault.grants) != 0 {
		t.Error("client credentials reached Google sign-in or the vault")
	}
}

func TestClientCredentialsToken(t *testing.T) {
	e, _ := newServiceEnv(t)

	// client_secret_basic, then client_secret_post with explicit scope
	// and resource.
	for _, r := range []tokenResp{
		e.token(ccForm(nil), "ci", "ci-secret"),
		e.token(ccForm(map[string]string{"client_id": "ci", "client_secret": "ci-secret", "scope": ScopeExecute, "resource": e.as.Resource()}), "", ""),
	} {
		if r.status != http.StatusOK || r.Access == "" || r.Refresh != "" || r.Scope != ScopeExecute || r.Expires <= 0 || r.Expires > int64(accessTTL.Seconds()) {
			t.Fatalf("client_credentials = %d %+v; want an access token, no refresh token", r.status, r)
		}
		ti, err := e.as.Verifier()(context.Background(), r.Access, nil)
		if err != nil {
			t.Fatal(err)
		}
		u := ti.Extra[creds.TokenInfoKey].(*creds.OAuthUser)
		if id, _ := u.Identity(context.Background()); ti.UserID != "client:ci" || id.Mode != "service" || id.Email != runnerSA {
			t.Errorf("verified as %q, %+v", ti.UserID, id)
		}
	}

	for name, tc := range map[string]struct {
		form         url.Values
		id, secret   string
		status       int
		wantErrorFor string
	}{
		"wrong secret":   {ccForm(nil), "ci", "nope", http.StatusUnauthorized, "invalid_client"},
		"no secret":      {ccForm(map[string]string{"client_id": "ci"}), "", "", http.StatusUnauthorized, "invalid_client"},
		"user client":    {ccForm(nil), "cli", "cli-secret", http.StatusBadRequest, "unauthorized_client"},
		"public client":  {ccForm(map[string]string{"client_id": e.register()}), "", "", http.StatusBadRequest, "unauthorized_client"},
		"other scope":    {ccForm(map[string]string{"scope": "openid"}), "ci", "ci-secret", http.StatusBadRequest, "invalid_scope"},
		"other resource": {ccForm(map[string]string{"resource": "https://elsewhere.example/mcp"}), "ci", "ci-secret", http.StatusBadRequest, "invalid_target"},
	} {
		r := e.token(tc.form, tc.id, tc.secret)
		if r.status != tc.status || r.Error != tc.wantErrorFor || r.Access != "" {
			t.Errorf("%s: %d %q, want %d %q", name, r.status, r.Error, tc.status, tc.wantErrorFor)
		}
	}
}

func TestClientCredentialsMintFails(t *testing.T) {
	e, m := newServiceEnv(t)
	m.fail = true
	if r := e.token(ccForm(nil), "ci", "ci-secret"); r.status != http.StatusInternalServerError || r.Error != "server_error" {
		t.Errorf("mint failure: %d %q, want 500 server_error", r.status, r.Error)
	}
}

// Changing or removing a client's service account revokes its tokens.
func TestClientCredentialsRevokedByConfig(t *testing.T) {
	e, _ := newServiceEnv(t)
	r := e.token(ccForm(nil), "ci", "ci-secret")
	verify := e.as.Verifier()
	e.as.static["ci"] = StaticClient{ID: "ci", Secret: "ci-secret", ServiceAccount: "other@proj-sa.iam.gserviceaccount.com"}
	if _, err := verify(context.Background(), r.Access, nil); err == nil {
		t.Error("token still valid after the client's service account changed")
	}
	delete(e.as.static, "ci")
	if _, err := verify(context.Background(), r.Access, nil); err == nil {
		t.Error("token still valid after the client was removed")
	}
}

func TestClientCredentialsNoSignIn(t *testing.T) {
	e, _ := newServiceEnv(t)
	loc, _ := newBrowser(t).visit(e.authorizeURL("ci", strings.Repeat("v", 43), nil))
	if loc != nil {
		t.Errorf("/authorize for a client-credentials client redirected to %v", loc)
	}
	var m map[string]any
	resp, err := http.Get(e.srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if g := fmt.Sprint(m["grant_types_supported"]); !strings.Contains(g, "client_credentials") {
		t.Errorf("grant_types_supported = %s, want client_credentials", g)
	}
}

func TestServiceClientConfig(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k1", Secret: []byte(strings.Repeat("k", 32))})
	base := Config{
		Issuer: "https://kg.example.com", Google: GoogleClient{ClientID: "g", ClientSecret: "s"},
		Custody: SealedCustody{}, Allow: &AllowList{Domains: []string{"example.com"}}, Keys: keys,
	}
	for name, tc := range map[string]struct {
		c      StaticClient
		minter ServiceTokens
	}{
		"no minter":     {StaticClient{ID: "ci", Secret: "s", ServiceAccount: runnerSA}, nil},
		"no secret":     {StaticClient{ID: "ci", ServiceAccount: runnerSA}, &fakeMinter{}},
		"redirect uris": {StaticClient{ID: "ci", Secret: "s", ServiceAccount: runnerSA, RedirectURIs: []string{clientRedirect}}, &fakeMinter{}},
		"not an email":  {StaticClient{ID: "ci", Secret: "s", ServiceAccount: "runner"}, &fakeMinter{}},
	} {
		cfg := base
		cfg.Clients, cfg.ServiceTokens = []StaticClient{tc.c}, tc.minter
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted %+v", name, tc.c)
		}
	}

	path := filepath.Join(t.TempDir(), "clients.json")
	_ = os.WriteFile(path, []byte(`[{"client_id":"ci","client_secret":"s","service_account":"`+runnerSA+`"}]`), 0o600)
	cs, err := LoadStaticClients(path)
	if err != nil || len(cs) != 1 || cs[0].ServiceAccount != runnerSA {
		t.Errorf("LoadStaticClients = %+v, %v", cs, err)
	}
	_ = os.WriteFile(path, []byte(`[{"client_id":"ci","service_account":"`+runnerSA+`"}]`), 0o600)
	if _, err := LoadStaticClients(path); err == nil {
		t.Error("LoadStaticClients accepted a client-credentials client without a secret")
	}
}

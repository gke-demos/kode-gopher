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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

func wrapReadOnlyGoogleRefresh(g *fakeGoogle) {
	orig := g.srv.Config.Handler
	roScope := strings.Join(googleUserScopes(true), " ")
	g.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/token" {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			vals, _ := url.ParseQuery(string(raw))
			if vals.Get("grant_type") == "refresh_token" {
				g.mu.Lock()
				u, ok := g.refresh[vals.Get("refresh_token")]
				if !ok || g.revoked[u.Sub] {
					g.mu.Unlock()
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
					return
				}
				tok := g.mint(u, roScope, 3600)
				g.mu.Unlock()
				writeJSON(w, http.StatusOK, map[string]any{
					"access_token": tok, "expires_in": 3599, "scope": roScope,
					"id_token": idToken(u, testGoogleID),
				})
				return
			}
		}
		orig.ServeHTTP(w, r)
	})
}

func wrapReadOnlyVault(t *testing.T, v *fakeVault) {
	t.Helper()
	orig := v.srv.Config.Handler
	wantScopes := googleUserScopes(true)
	v.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/authProviders/kg/credentials:retrieve") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			var gotScopes []string
			for _, s := range body["scopes"].([]any) {
				gotScopes = append(gotScopes, s.(string))
			}
			if !slices.Equal(gotScopes, wantScopes) {
				http.Error(w, "unexpected read-only vault scopes", http.StatusBadRequest)
				return
			}
			uid, _ := body["userId"].(string)
			v.mu.Lock()
			if len(v.script) == 0 {
				if u, ok := v.grants[uid]; ok {
					v.g.mu.Lock()
					tok := v.g.mint(u, strings.Join(wantScopes, " "), 3600)
					v.g.mu.Unlock()
					v.mu.Unlock()
					writeJSON(w, http.StatusOK, map[string]any{"success": map[string]any{"token": tok, "expireTime": "2099-01-01T00:00:00Z"}})
					return
				}
			}
			v.mu.Unlock()
			// Delegate consent URI / scripted responses to fakeVault using its baseline scope check.
			body["scopes"] = googleUserScopes(false)
			rewritten, _ := json.Marshal(body)
			r.Body = io.NopCloser(bytes.NewReader(rewritten))
		}
		orig.ServeHTTP(w, r)
	})
}

func TestReadOnlySealedSignInAndRefresh(t *testing.T) {
	e := newTestEnv(t, true, func(cfg *Config) {
		cfg.ReadOnly = true
		cfg.Custody = SealedCustody{ReadOnly: true}
	})
	wrapReadOnlyGoogleRefresh(e.google)
	got, err := e.connect(newBrowser(t), dcrConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !strings.HasPrefix(got, "111 alice@example.com proj-1 ") {
		t.Errorf("whoami = %q", got)
	}
	e.google.mu.Lock()
	authScope := strings.Fields(e.google.lastAuth.Get("scope"))
	e.google.mu.Unlock()
	if slices.Contains(authScope, scopeCloudPlatform) {
		t.Errorf("Google auth requested full cloud-platform scope on read-only server: %v", authScope)
	}
	for _, want := range creds.ReadOnlyScopes {
		if !slices.Contains(authScope, want) {
			t.Errorf("Google auth missing read-only scope %q in %v", want, authScope)
		}
	}

	// Verify refresh_token grant preserves ReadOnly=true on the rotated access token.
	cid := e.register()
	v := rand.Text() + rand.Text()
	code := e.code(cid, v)
	tok := e.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {clientRedirect}, "client_id": {cid}, "code_verifier": {v},
	}, "", "")
	if tok.status != http.StatusOK || tok.Refresh == "" {
		t.Fatalf("code exchange status=%d err=%s", tok.status, tok.Error)
	}
	ref := e.token(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok.Refresh}, "client_id": {cid},
	}, "", "")
	if ref.status != http.StatusOK {
		t.Fatalf("refresh status=%d err=%s", ref.status, ref.Error)
	}
	ti, err := e.as.Verifier()(context.Background(), ref.Access, nil)
	if err != nil {
		t.Fatalf("Verifier(refreshed): %v", err)
	}
	if u := ti.Extra[creds.TokenInfoKey].(*creds.OAuthUser); !u.ReadOnly {
		t.Errorf("refreshed OAuthUser.ReadOnly = false, want true")
	}
}

func TestReadOnlyVaultSignInAndFullScopeRejection(t *testing.T) {
	e := newTestEnv(t, false, func(cfg *Config) {
		cfg.ReadOnly = true
		vc := cfg.Custody.(*VaultCustody)
		vc.ReadOnly = true
	})
	wrapReadOnlyVault(t, e.vault)
	got, err := e.connect(newBrowser(t), dcrConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !strings.HasPrefix(got, "111 alice@example.com proj-1 ") {
		t.Errorf("whoami = %q", got)
	}

	// A pre-existing vault grant carrying full cloud-platform OR a service-specific
	// write scope (e.g. bigquery without cloud-platform) must be refused on a read-only server.
	for _, extraScope := range []string{
		scopeCloudPlatform,
		"https://www.googleapis.com/auth/bigquery",
		"https://www.googleapis.com/auth/devstorage.full_control",
	} {
		e.google.mu.Lock()
		badTok := e.google.mint(alice, scopeOpenID+" "+scopeEmail+" "+creds.CloudPlatformReadOnlyScope+" "+extraScope, 3600)
		e.google.mu.Unlock()
		e.vault.mu.Lock()
		e.vault.script = []string{`{"success":{"token":"` + badTok + `","expireTime":"2099-01-01T00:00:00Z"}}`}
		e.vault.mu.Unlock()
		if _, err := e.connect(newBrowser(t), dcrConfig()); err == nil {
			t.Fatalf("vault token carrying write scope %q was accepted on a read-only server", extraScope)
		}
	}
}

func TestReadOnlySealedRejectsTamperedBrowserWriteScope(t *testing.T) {
	e := newTestEnv(t, true, func(cfg *Config) {
		cfg.ReadOnly = true
		cfg.Custody = SealedCustody{ReadOnly: true}
	})
	cid := e.register()
	b := newBrowser(t)
	v := rand.Text() + rand.Text()
	form := consentForm(t, b, e.authorizeURL(cid, v, nil))
	googleAuthURL, page := approve(t, b, e.srv.URL, form)
	if googleAuthURL == "" {
		t.Fatalf("approve stopped on page: %q", page)
	}
	// Simulate an attacker editing the unauthenticated scope= parameter in the
	// browser URL to request compute + cloud-platform.read-only (omitting cloud-platform).
	parsed, err := url.Parse(googleAuthURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("scope", strings.Join(append(googleUserScopes(true), "https://www.googleapis.com/auth/compute"), " "))
	parsed.RawQuery = q.Encode()

	loc, stopPage := b.visit(parsed.String())
	if loc == nil {
		t.Fatalf("tampered callback stopped on page %q; expected OAuth error redirect", stopPage)
	}
	if loc.Query().Get("code") != "" || loc.Query().Get("error") != "access_denied" {
		t.Fatalf("tampered scope callback returned %v; want error=access_denied and no code", loc)
	}
}

func TestReadOnlyRejectsCrossPolicyInFlightCookies(t *testing.T) {
	sharedKeys, err := NewKeyring(KeyringKey{ID: "k1", Secret: []byte(strings.Repeat("m", 32))})
	if err != nil {
		t.Fatal(err)
	}
	rwEnv := newTestEnv(t, false, func(cfg *Config) { cfg.Keys = sharedKeys })
	roEnv := newTestEnv(t, false, func(cfg *Config) {
		cfg.Keys = sharedKeys
		cfg.ReadOnly = true
		cfg.Issuer = rwEnv.srv.URL
		vc := cfg.Custody.(*VaultCustody)
		vc.ReadOnly = true
	})

	// Seal a pendingConsent from the full-scope pod (ReadOnly=false) and verify
	// roEnv's /consent/continue rejects it BEFORE calling VaultCustody.finish/finalize.
	sealedPending, err := sharedKeys.seal(purposePending, pendingConsent{
		Req:      authRequest{ClientID: "c1", RedirectURI: clientRedirect, State: "st"},
		User:     alice,
		Consent:  Consent{Nonce: "n1", UID: "u1"},
		ReadOnly: false,
		Exp:      time.Now().Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, roEnv.srv.URL+"/consent/continue?state=s1", nil)
	req.AddCookie(&http.Cookie{Name: cookieConsent, Value: sealedPending})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/consent/continue status = %d, want 400", resp.StatusCode)
	}
	roEnv.vault.mu.Lock()
	_, poisoned := roEnv.vault.grants[alice.Sub]
	roEnv.vault.mu.Unlock()
	if poisoned {
		t.Fatal("cross-policy /consent/continue finalized a grant in the vault")
	}
}

type fakeReadOnlyServiceMinter struct {
	rwCalls int
	roCalls int
}

func (f *fakeReadOnlyServiceMinter) Token(_ context.Context, _ string) (*oauth2.Token, error) {
	f.rwCalls++
	return &oauth2.Token{AccessToken: "tok-svc-rw", Expiry: time.Now().Add(time.Hour)}, nil
}

func (f *fakeReadOnlyServiceMinter) ReadOnlyToken(_ context.Context, _ string) (*oauth2.Token, error) {
	f.roCalls++
	return &oauth2.Token{AccessToken: "tok-svc-ro", Expiry: time.Now().Add(time.Hour)}, nil
}

func TestReadOnlyClientCredentialsAndModeIsolation(t *testing.T) {
	minter := &fakeReadOnlyServiceMinter{}
	secret := strings.Repeat("s", 32)
	sharedKeys, err := NewKeyring(KeyringKey{ID: "k1", Secret: []byte(strings.Repeat("z", 32))})
	if err != nil {
		t.Fatal(err)
	}

	rwEnv := newTestEnv(t, true, func(cfg *Config) {
		cfg.Keys = sharedKeys
		cfg.Clients = []StaticClient{{ID: "bot", Secret: secret, ServiceAccount: "bot@proj-1.iam.gserviceaccount.com"}}
		cfg.ServiceTokens = minter
	})
	roEnv := newTestEnv(t, true, func(cfg *Config) {
		cfg.Keys = sharedKeys
		cfg.ReadOnly = true
		cfg.Custody = SealedCustody{ReadOnly: true}
		cfg.Clients = []StaticClient{{ID: "bot", Secret: secret, ServiceAccount: "bot@proj-1.iam.gserviceaccount.com"}}
		cfg.ServiceTokens = minter
		// Match rwEnv's issuer so audience matches; only ReadOnly differs!
		cfg.Issuer = rwEnv.srv.URL
	})

	// Mint a read-only client_credentials token on roEnv and verify it carries ReadOnly=true.
	req, _ := http.NewRequest(http.MethodPost, roEnv.srv.URL+"/token", strings.NewReader(url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {"bot"},
		"client_secret": {secret},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("client_credentials status=%v err=%v", resp, err)
	}
	defer resp.Body.Close()
	if minter.roCalls != 1 || minter.rwCalls != 0 {
		t.Errorf("roCalls=%d rwCalls=%d, want 1 and 0", minter.roCalls, minter.rwCalls)
	}

	// Mint a full-scope token on rwEnv (sharing the same keyring and resource!)
	// and verify roEnv.Verifier() rejects it with ErrInvalidToken.
	rwAt, _, terr := rwEnv.as.sealAccess(accessPayload{
		Sub: "111", Email: "alice@example.com", ClientID: "c1", Aud: rwEnv.as.Resource(),
		Scope: ScopeExecute, Token: "tok-rw", TokenExp: time.Now().Add(time.Hour).Unix(),
		ReadOnly: false,
	}, time.Now())
	if terr != nil {
		t.Fatal(terr)
	}
	if _, err := roEnv.as.Verifier()(context.Background(), rwAt, nil); err == nil {
		t.Error("read-only Verifier accepted a full-scope access token sharing the same keyring")
	}

	// Conversely, a read-only token is verified with OAuthUser.ReadOnly=true on roEnv
	// and rejected by rwEnv.Verifier().
	roAt, _, terr := roEnv.as.sealAccess(accessPayload{
		Sub: "111", Email: "alice@example.com", ClientID: "c1", Aud: roEnv.as.Resource(),
		Scope: ScopeExecute, Token: "tok-ro", TokenExp: time.Now().Add(time.Hour).Unix(),
		ReadOnly: true,
	}, time.Now())
	if terr != nil {
		t.Fatal(terr)
	}
	ti, err := roEnv.as.Verifier()(context.Background(), roAt, nil)
	if err != nil {
		t.Fatalf("roEnv.Verifier(roAt): %v", err)
	}
	u := ti.Extra[creds.TokenInfoKey].(*creds.OAuthUser)
	if !u.ReadOnly {
		t.Errorf("OAuthUser.ReadOnly = false, want true")
	}
	if _, err := rwEnv.as.Verifier()(context.Background(), roAt, nil); err == nil {
		t.Error("full-scope Verifier accepted a read-only access token")
	}

	// A full-scope refresh token from rwEnv (sharing keyring) must be rejected by roEnv's /token.
	rwCid := rwEnv.register()
	rwVer := rand.Text() + rand.Text()
	rwCode := rwEnv.code(rwCid, rwVer)
	rwTok := rwEnv.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {rwCode},
		"redirect_uri": {clientRedirect}, "client_id": {rwCid}, "code_verifier": {rwVer},
	}, "", "")
	if rwTok.status != http.StatusOK || rwTok.Refresh == "" {
		t.Fatalf("rw code exchange status=%d err=%s", rwTok.status, rwTok.Error)
	}
	crossRef := roEnv.token(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {rwTok.Refresh}, "client_id": {rwCid},
	}, "", "")
	if crossRef.status != http.StatusBadRequest || crossRef.Error != "invalid_grant" {
		t.Errorf("cross-mode refresh status=%d err=%q, want 400 invalid_grant", crossRef.status, crossRef.Error)
	}
}

func TestNewRejectsMismatchedReadOnlyCustody(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k1", Secret: []byte(strings.Repeat("k", 32))})
	_, err := New(Config{
		Issuer:   "https://kg.example.com",
		Google:   GoogleClient{ClientID: "id", ClientSecret: "sec"},
		Custody:  SealedCustody{ReadOnly: false},
		Allow:    &AllowList{Domains: []string{"example.com"}},
		Keys:     keys,
		ReadOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "ReadOnly") {
		t.Errorf("New(ReadOnly mismatch) err = %v, want mismatch error", err)
	}
}

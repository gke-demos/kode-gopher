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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// register is DCR for a public client redirecting to clientRedirect.
func (e *testEnv) register() string {
	e.t.Helper()
	resp, err := http.Post(e.srv.URL+"/register", "application/json", strings.NewReader(`{"client_name":"t","redirect_uris":["`+clientRedirect+`"]}`))
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m struct {
		ClientID string `json:"client_id"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&m) != nil {
		e.t.Fatalf("register: %s", resp.Status)
	}
	return m.ClientID
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeURL is a well-formed authorization request; set overrides
// or deletes (empty value) parameters.
func (e *testEnv) authorizeURL(clientID, verifier string, set map[string]string) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {clientRedirect},
		"code_challenge": {challengeFor(verifier)}, "code_challenge_method": {"S256"},
		"state": {"st"}, "resource": {e.as.Resource()}, "scope": {ScopeExecute},
	}
	for k, v := range set {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return e.srv.URL + "/authorize?" + q.Encode()
}

// code signs in through a browser and returns the authorization code.
func (e *testEnv) code(clientID, verifier string) string {
	e.t.Helper()
	loc, page := newBrowser(e.t).visit(e.authorizeURL(clientID, verifier, nil))
	if loc == nil || loc.Query().Get("code") == "" {
		e.t.Fatalf("no code: redirect %v, page %q", loc, page)
	}
	if loc.Query().Get("state") != "st" || loc.Query().Get("iss") != e.srv.URL {
		e.t.Fatalf("redirect %v lacks state or iss", loc)
	}
	return loc.Query().Get("code")
}

type tokenResp struct {
	status  int
	header  http.Header
	Error   string `json:"error"`
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int64  `json:"expires_in"`
	Scope   string `json:"scope"`
}

func (e *testEnv) token(form url.Values, basicID, basicSecret string) tokenResp {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicID != "" {
		req.SetBasicAuth(url.QueryEscape(basicID), url.QueryEscape(basicSecret))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := tokenResp{status: resp.StatusCode, header: resp.Header}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return r
}

func exchangeForm(clientID, code, verifier string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {clientRedirect}}
}

func TestCodeExchange(t *testing.T) {
	e := newTestEnv(t, false, nil)
	id := e.register()
	verifier := rand.Text() + rand.Text()
	code := e.code(id, verifier)
	other := e.register()

	bad := []struct {
		name string
		edit func(url.Values)
		want string
	}{
		{"no verifier", func(f url.Values) { f.Del("code_verifier") }, "invalid_grant"},
		{"wrong verifier", func(f url.Values) { f.Set("code_verifier", rand.Text()+rand.Text()) }, "invalid_grant"},
		{"plain verifier", func(f url.Values) { f.Set("code_verifier", challengeFor(verifier)) }, "invalid_grant"},
		{"other redirect", func(f url.Values) { f.Set("redirect_uri", "http://127.0.0.1:2/callback") }, "invalid_grant"},
		{"other resource", func(f url.Values) { f.Set("resource", "https://evil.example/mcp") }, "invalid_target"},
		{"other client", func(f url.Values) { f.Set("client_id", other) }, "invalid_grant"},
		{"unknown client", func(f url.Values) { f.Set("client_id", "nope") }, "invalid_client"},
		{"garbage code", func(f url.Values) { f.Set("code", "kg1.k1.AAAA") }, "invalid_grant"},
		{"bad grant type", func(f url.Values) { f.Set("grant_type", "password") }, "unsupported_grant_type"},
	}
	for _, tc := range bad {
		f := exchangeForm(id, code, verifier)
		tc.edit(f)
		if r := e.token(f, "", ""); r.Error != tc.want || r.Access != "" {
			t.Errorf("%s: error %q (HTTP %d), want %s", tc.name, r.Error, r.status, tc.want)
		}
	}

	// Failed attempts don't spend the code; the right one does.
	r := e.token(exchangeForm(id, code, verifier), "", "")
	if r.status != http.StatusOK || r.Access == "" || r.Refresh == "" || r.Scope != ScopeExecute {
		t.Fatalf("exchange: %+v", r)
	}
	if r.Expires <= 0 || r.Expires > int64(accessTTL.Seconds()) {
		t.Errorf("expires_in = %d", r.Expires)
	}
	if r := e.token(exchangeForm(id, code, verifier), "", ""); r.Error != "invalid_grant" {
		t.Errorf("reused code: error %q, want invalid_grant", r.Error)
	}
}

func TestConfidentialClient(t *testing.T) {
	e := newTestEnv(t, true, func(c *Config) {
		c.Clients = []StaticClient{{ID: "cli:1", Secret: "s/3+cret", RedirectURIs: []string{clientRedirect}}}
	})
	verifier := rand.Text() + rand.Text()
	code := e.code("cli:1", verifier)
	f := exchangeForm("", code, verifier)
	f.Del("client_id")
	if r := e.token(f, "cli:1", "wrong"); r.status != http.StatusUnauthorized || r.Error != "invalid_client" || r.header.Get("WWW-Authenticate") == "" {
		t.Errorf("wrong Basic secret: %d %q, WWW-Authenticate %q", r.status, r.Error, r.header.Get("WWW-Authenticate"))
	}
	f2 := exchangeForm("cli:1", code, verifier)
	if r := e.token(f2, "", ""); r.Error != "invalid_client" {
		t.Errorf("no secret: error %q, want invalid_client", r.Error)
	}
	// Basic credentials are form-encoded (RFC 6749 §2.3.1).
	if r := e.token(f, "cli:1", "s/3+cret"); r.status != http.StatusOK {
		t.Fatalf("Basic auth exchange: %d %q", r.status, r.Error)
	}
}

func TestAuthorizeRejects(t *testing.T) {
	e := newTestEnv(t, false, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	redirects := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"plain PKCE", map[string]string{"code_challenge_method": "plain", "code_challenge": v}, "invalid_request"},
		{"no PKCE", map[string]string{"code_challenge_method": "", "code_challenge": ""}, "invalid_request"},
		{"token response", map[string]string{"response_type": "token"}, "unsupported_response_type"},
		{"other resource", map[string]string{"resource": "https://evil.example/mcp"}, "invalid_target"},
	}
	for _, tc := range redirects {
		loc, page := newBrowser(t).visit(e.authorizeURL(id, v, tc.set))
		if loc == nil || loc.Query().Get("error") != tc.want || loc.Query().Get("state") != "st" {
			t.Errorf("%s: redirect %v page %q, want error=%s", tc.name, loc, page, tc.want)
		}
	}
	// Without a trustworthy redirect, errors stay on kode-gopher's page.
	pages := []map[string]string{
		{"client_id": "unknown"},
		{"redirect_uri": "https://evil.example/cb"},
	}
	for _, set := range pages {
		resp, err := http.Get(e.authorizeURL(id, v, set))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("%v: HTTP %d, Location %q; want an error page", set, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// A loopback redirect may use any port.
	loc, _ := newBrowser(t).visit(e.authorizeURL(id, v, map[string]string{"redirect_uri": "http://127.0.0.1:1/callback"}))
	if loc == nil || loc.Query().Get("code") == "" {
		t.Errorf("loopback redirect on another port refused: %v", loc)
	}
}

// TestConsentForgery checks the consent form only works in the browser
// that loaded it.
func TestConsentForgery(t *testing.T) {
	e := newTestEnv(t, false, nil)
	id := e.register()
	resp, err := http.Get(e.authorizeURL(id, rand.Text()+rand.Text(), nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := hiddenRequestRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("no consent form")
	}
	// Victim's browser has no kg_auth cookie for this request.
	resp, err = http.PostForm(e.srv.URL+"/authorize", url.Values{"request": {html.UnescapeString(m[1])}, "action": {"approve"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("consent without the binding cookie: HTTP %d, want 400", resp.StatusCode)
	}
}

func TestRefresh(t *testing.T) {
	e := newTestEnv(t, true, nil)
	e.google.signInAs = bob
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	refreshForm := func(rt string) url.Values {
		return url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {rt}}
	}
	other := refreshForm(first.Refresh)
	other.Set("client_id", e.register())
	if r := e.token(other, "", ""); r.Error != "invalid_grant" {
		t.Errorf("refresh by another client: %q", r.Error)
	}
	wrongRes := refreshForm(first.Refresh)
	wrongRes.Set("resource", "https://evil.example/mcp")
	if r := e.token(wrongRes, "", ""); r.Error != "invalid_target" {
		t.Errorf("refresh for another resource: %q", r.Error)
	}

	second := e.token(refreshForm(first.Refresh), "", "")
	if second.status != http.StatusOK || second.Access == "" || second.Refresh == first.Refresh {
		t.Fatalf("refresh: %+v", second)
	}
	if r := e.token(refreshForm(first.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("reused refresh token: %q, want invalid_grant", r.Error)
	}

	// Removed from the group: refused once the cached decision ages out.
	delete(e.groups.members["kg-users@example.com"], bob.Email)
	e.as.cfg.Allow.cache = nil
	if r := e.token(refreshForm(second.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("refresh after removal from the allow-list: %q", r.Error)
	}
}

func TestRefreshRevoked(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	e.google.revoked[alice.Sub] = true
	r := e.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {first.Refresh}}, "", "")
	if r.Error != "invalid_grant" {
		t.Errorf("refresh after Google revoked the grant: %q (HTTP %d), want invalid_grant", r.Error, r.status)
	}
}

func TestVaultRefreshesShortToken(t *testing.T) {
	e := newTestEnv(t, false, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	e.code(id, v) // consent stores the grant
	e.vault.shortLife = true
	r := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	if r.status != http.StatusOK {
		t.Fatalf("exchange: %+v", r)
	}
	if e.vault.forced != 1 {
		t.Errorf("vault force-refreshes = %d, want 1 for a nearly spent token", e.vault.forced)
	}
	if r.Expires < int64((accessTTL - time.Minute).Seconds()) {
		t.Errorf("expires_in = %d after force refresh", r.Expires)
	}
}

func TestProtect(t *testing.T) {
	e := newTestEnv(t, false, nil)
	seal := func(p accessPayload) string {
		s, err := e.as.keys.seal(purposeAccess, p)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Now()
	good := accessPayload{Sub: "1", Aud: e.as.Resource(), Scope: ScopeExecute, Token: "ya29.x", TokenExp: now.Add(time.Hour).Unix(), Exp: now.Add(time.Minute).Unix()}
	noScope, expired, otherAud := good, good, good
	noScope.Scope = ""
	expired.Exp = now.Add(-time.Minute).Unix()
	otherAud.Aud = "https://other.example/mcp"
	refresh, _ := e.as.keys.seal(purposeRefresh, refreshPayload{Aud: e.as.Resource(), Exp: now.Add(time.Hour).Unix()})

	cases := []struct {
		name   string
		token  string
		status int
		want   string
	}{
		{"none", "", http.StatusUnauthorized, `resource_metadata="` + e.as.ResourceMetadataURL() + `"`},
		{"garbage", "abc", http.StatusUnauthorized, "resource_metadata="},
		{"expired", seal(expired), http.StatusUnauthorized, "resource_metadata="},
		{"other audience", seal(otherAud), http.StatusUnauthorized, "resource_metadata="},
		{"refresh token as access", refresh, http.StatusUnauthorized, "resource_metadata="},
		{"no scope", seal(noScope), http.StatusForbidden, `error="insufficient_scope", scope="` + ScopeExecute + `"`},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp", strings.NewReader(`{}`))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if wa := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != tc.status || !strings.Contains(wa, tc.want) {
			t.Errorf("%s: HTTP %d, WWW-Authenticate %q; want %d with %s", tc.name, resp.StatusCode, wa, tc.status, tc.want)
		}
	}
}

func TestMetadata(t *testing.T) {
	e := newTestEnv(t, false, nil)
	get := func(path string) map[string]any {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return m
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		if m := get(p); m["resource"] != e.as.Resource() {
			t.Errorf("%s resource = %v", p, m["resource"])
		}
	}
	m := get("/.well-known/oauth-authorization-server")
	if m["issuer"] != e.srv.URL || m["registration_endpoint"] != e.srv.URL+"/register" || m["client_id_metadata_document_supported"] != true {
		t.Errorf("AS metadata: %v", m)
	}
}

// A transient allow-list failure during refresh must not spend the
// refresh token: the client retries with the same one and succeeds.
func TestRefreshSurvivesTransientAllowListError(t *testing.T) {
	e := newTestEnv(t, true, nil)
	e.google.signInAs = bob
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {first.Refresh}}

	e.as.cfg.Allow.cache = nil // force a group check
	e.groups.failNext = 1
	if r := e.token(form, "", ""); r.Error != "server_error" {
		t.Fatalf("refresh during a directory outage: %q, want server_error", r.Error)
	}
	if r := e.token(form, "", ""); r.status != http.StatusOK || r.Access == "" {
		t.Errorf("retry with the same refresh token: %+v, want new tokens", r)
	}
	if r := e.token(form, "", ""); r.Error != "invalid_grant" {
		t.Errorf("third use after a successful refresh: %q, want invalid_grant", r.Error)
	}
}

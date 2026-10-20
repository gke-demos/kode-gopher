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
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	other := refreshForm(id, first.Refresh)
	other.Set("client_id", e.register())
	if r := e.token(other, "", ""); r.Error != "invalid_grant" {
		t.Errorf("refresh by another client: %q", r.Error)
	}
	wrongRes := refreshForm(id, first.Refresh)
	wrongRes.Set("resource", "https://evil.example/mcp")
	if r := e.token(wrongRes, "", ""); r.Error != "invalid_target" {
		t.Errorf("refresh for another resource: %q", r.Error)
	}

	second := e.token(refreshForm(id, first.Refresh), "", "")
	if second.status != http.StatusOK || second.Access == "" || second.Refresh == first.Refresh {
		t.Fatalf("refresh: %+v", second)
	}
	third := e.token(refreshForm(id, second.Refresh), "", "")
	if third.status != http.StatusOK || third.Refresh == "" {
		t.Fatalf("second refresh: %+v", third)
	}

	// Removed from the group: refused once the cached decision ages out.
	delete(e.groups.members["kg-users@example.com"], bob.Email)
	e.as.cfg.Allow.cache = nil
	if r := e.token(refreshForm(id, third.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("refresh after removal from the allow-list: %q", r.Error)
	}
}

func refreshForm(clientID, rt string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {rt}}
}

// Reusing a rotated refresh token revokes its whole family (OAuth 2.1
// §4.3.1): whichever holder refreshed first, the other isn't the client,
// so the newest token is refused too and the user signs in again. Other
// sign-ins are untouched.
func TestRefreshReuseRevokesFamily(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	other := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	second := e.token(refreshForm(id, first.Refresh), "", "")
	if second.status != http.StatusOK || second.Refresh == "" {
		t.Fatalf("refresh: %+v", second)
	}
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Fatalf("reused refresh token: %q, want invalid_grant", r.Error)
	}
	if r := e.token(refreshForm(id, second.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("newest refresh token after a reuse revoked its family: %q (HTTP %d), want invalid_grant", r.Error, r.status)
	}
	if r := e.token(refreshForm(id, other.Refresh), "", ""); r.status != http.StatusOK {
		t.Errorf("another sign-in's refresh token after a reuse in a different family: %+v", r)
	}
}

// A refresh refused for good (invalid_grant) keeps its token spent; one
// that failed for a transient reason (server_error) gives it back.
func TestRefreshFailuresAndTheFamily(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	tokenURL := e.as.google.client.TokenURL
	e.as.google.client.TokenURL = down.URL
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "server_error" {
		t.Fatalf("refresh while Google's token endpoint is down: %q, want server_error", r.Error)
	}
	e.as.google.client.TokenURL = tokenURL
	second := e.token(refreshForm(id, first.Refresh), "", "")
	if second.status != http.StatusOK {
		t.Fatalf("retry once Google is back: %+v", second)
	}

	e.google.mu.Lock()
	e.google.revoked[alice.Sub] = true
	e.google.mu.Unlock()
	if r := e.token(refreshForm(id, second.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Fatalf("refresh after Google revoked the grant: %q, want invalid_grant", r.Error)
	}
	e.google.mu.Lock()
	e.google.revoked[alice.Sub] = false
	e.google.mu.Unlock()
	if r := e.token(refreshForm(id, second.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("the same refresh token after Google restored the grant: %q, want invalid_grant", r.Error)
	}
}

// Concurrent refreshes with one token: exactly one wins.
func TestRefreshConcurrentReuse(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Go(func() {
			if r := e.token(refreshForm(id, first.Refresh), "", ""); r.status == http.StatusOK {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if ok != 1 {
		t.Errorf("%d concurrent refreshes with one token succeeded, want 1", ok)
	}
}

// A refresh token issued before families (a jti, no family) still works
// once, and starts a family of its own.
func TestRefreshTokenWithoutFamily(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	var p refreshPayload
	if err := e.as.keys.open(purposeRefresh, first.Refresh, &p); err != nil {
		t.Fatal(err)
	}
	p.Family, p.Gen, p.JTI = "", 0, rand.Text()
	legacy, err := e.as.keys.seal(purposeRefresh, p)
	if err != nil {
		t.Fatal(err)
	}
	second := e.token(refreshForm(id, legacy), "", "")
	if second.status != http.StatusOK {
		t.Fatalf("refresh with a pre-family token: %+v", second)
	}
	if r := e.token(refreshForm(id, legacy), "", ""); r.Error != "invalid_grant" {
		t.Errorf("reused pre-family token: %q, want invalid_grant", r.Error)
	}
	if r := e.token(refreshForm(id, second.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("its successor after the reuse: %q, want invalid_grant", r.Error)
	}
	p.JTI = ""
	none, _ := e.as.keys.seal(purposeRefresh, p)
	if r := e.token(refreshForm(id, none), "", ""); r.Error != "invalid_grant" {
		t.Errorf("refresh token with neither family nor jti: %q, want invalid_grant", r.Error)
	}
}

func TestReplayCacheFamilies(t *testing.T) {
	c := &replayCache{seen: map[string]time.Time{}}
	exp := time.Now().Add(time.Hour)
	if !c.rotate("f", 0, exp) || !c.rotate("f", 1, exp) {
		t.Fatal("rotating a fresh family refused")
	}
	c.unrotate("f", 1) // gives back the last rotation
	if !c.rotate("f", 1, exp) {
		t.Error("rotation after unrotate refused")
	}
	c.unrotate("f", 0) // not the last rotation: no effect
	if c.rotate("f", 1, exp) {
		t.Error("a replayed generation accepted")
	}
	if c.rotate("f", 2, exp) {
		t.Error("the current generation accepted after the family was revoked")
	}
	c.unrotate("f", 1) // a revoked family stays revoked
	if c.rotate("f", 2, exp) {
		t.Error("unrotate revived a revoked family")
	}
	if !c.rotate("g", 7, exp) {
		t.Error("an unknown family (forgotten by a restart) refused")
	}
	c.families["f"] = family{gen: 2, exp: time.Now().Add(-time.Second), revoked: true}
	if !c.rotate("f", 0, exp) {
		t.Error("an expired family still refused")
	}
}

// A refresh token sealed for another resource (a server at another
// address with the same keys) is refused, not turned into tokens.
func TestRefreshForAnotherResource(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	var p refreshPayload
	if err := e.as.keys.open(purposeRefresh, first.Refresh, &p); err != nil {
		t.Fatal(err)
	}
	p.Aud = "https://elsewhere.example/mcp"
	rt, err := e.as.keys.seal(purposeRefresh, p)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.token(refreshForm(id, rt), "", ""); r.Error != "invalid_grant" || r.Access != "" {
		t.Errorf("refresh token for another resource: %q (HTTP %d), want invalid_grant", r.Error, r.status)
	}
}

// If the tokens can't be issued, the code isn't spent: nothing was
// handed out, so the client may try it again.
func TestCodeGivenBackWhenIssuingFails(t *testing.T) {
	e := newTestEnv(t, true, nil)
	id := e.register()
	v := rand.Text() + rand.Text()
	now := time.Now()
	// The Google token inside is too nearly spent to seal an access
	// token around (sealAccess's server_error).
	code, err := e.as.keys.seal(purposeCode, codePayload{
		Req:  authRequest{ClientID: id, RedirectURI: clientRedirect, Challenge: challengeFor(v), Resource: e.as.Resource(), Scope: ScopeExecute},
		User: alice, Token: "ya29.x", TokenExp: now.Add(30 * time.Second).Unix(), Secret: "1//x",
		JTI: rand.Text(), Exp: now.Add(codeTTL).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if r := e.token(exchangeForm(id, code, v), "", ""); r.Error != "server_error" {
			t.Errorf("exchange %d: %q (HTTP %d), want server_error, the code not spent", i+1, r.Error, r.status)
		}
	}
}

// An account whose email (or domain) changed since sign-in signs in
// again: the identity sealed into the refresh token, and the allow-list
// decision made on it, are stale.
func TestIdentityChangeAtRefresh(t *testing.T) {
	renamed := User{Sub: alice.Sub, Email: "alice.new@example.com", Domain: alice.Domain}
	recased := User{Sub: alice.Sub, Email: "Alice@Example.com", Domain: alice.Domain}
	for _, tc := range []struct {
		name   string
		sealed bool
		as     User
		want   string
	}{
		{"sealed, renamed", true, renamed, "invalid_grant"},
		{"sealed, domain gone", true, User{Sub: alice.Sub, Email: alice.Email}, "invalid_grant"},
		{"vault, renamed", false, renamed, "invalid_grant"},
		{"vault, same email in another case", false, recased, ""},
	} {
		e := newTestEnv(t, tc.sealed, nil)
		id := e.register()
		v := rand.Text() + rand.Text()
		e.code(id, v) // vault: consent stores the grant
		first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
		if first.status != http.StatusOK {
			t.Fatalf("%s: exchange: %+v", tc.name, first)
		}
		e.google.mu.Lock()
		for rt := range e.google.refresh {
			e.google.refresh[rt] = tc.as
		}
		e.google.mu.Unlock()
		e.vault.mu.Lock()
		e.vault.grants[alice.Sub] = tc.as
		e.vault.mu.Unlock()
		if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != tc.want {
			t.Errorf("%s: refresh: %q (HTTP %d), want %q", tc.name, r.Error, r.status, tc.want)
		}
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

// Vault grants only the user can repair are errReauth (a refresh says
// invalid_grant: sign in again), not transient (server_error: retry).
func TestPermanentVaultErrorsAreReauth(t *testing.T) {
	for _, e := range []error{
		reauthError("vault: the user declined consent"),
		reauthError("vault: token lacks the Google Cloud scope"),
	} {
		if !errors.Is(e, errReauth) {
			t.Errorf("%v doesn't match errReauth", e)
		}
	}
	if errors.Is(errors.New("vault retrieve: HTTP 503"), errReauth) {
		t.Error("a transient vault error matches errReauth")
	}
}

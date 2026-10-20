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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// More than one resource is refused, at /authorize and at every grant
// of /token (RFC 8707): each would check only the first.
func TestOneResource(t *testing.T) {
	e, _ := newServiceEnv(t)
	id := e.register()
	v := rand.Text() + rand.Text()
	twice := "&resource=" + url.QueryEscape(e.as.Resource())
	loc, page := newBrowser(t).visit(e.authorizeURL(id, v, nil) + twice)
	if loc == nil || loc.Query().Get("error") != "invalid_target" {
		t.Errorf("two resources at /authorize: redirect %v page %q, want invalid_target", loc, page)
	}

	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	if first.Refresh == "" {
		t.Fatalf("exchange: %+v", first)
	}
	code := e.code(id, v)
	f := exchangeForm(id, code, v)
	f["resource"] = []string{e.as.Resource(), e.as.Resource()}
	if r := e.token(f, "", ""); r.Error != "invalid_target" {
		t.Errorf("two resources at code exchange: %d %q, want invalid_target", r.status, r.Error)
	}
	// Refused before the code was spent.
	if r := e.token(exchangeForm(id, code, v), "", ""); r.Access == "" {
		t.Errorf("exchange after the refusal: %d %q", r.status, r.Error)
	}
	rf := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {first.Refresh},
		"resource": {e.as.Resource(), "https://evil.example/mcp"}}
	if r := e.token(rf, "", ""); r.Error != "invalid_target" {
		t.Errorf("two resources at refresh: %d %q, want invalid_target", r.status, r.Error)
	}
	cc := ccForm(nil)
	cc["resource"] = []string{e.as.Resource(), "https://evil.example/mcp"}
	if r := e.token(cc, "ci", "ci-secret"); r.Error != "invalid_target" || r.Access != "" {
		t.Errorf("two resources at client_credentials: %d %q, want invalid_target", r.status, r.Error)
	}
}

// outageVault is env's vault behind a front that answers status to
// every call of method (retrieve or finalize).
func outageVault(t *testing.T, e *testEnv, method string, status int) *VaultCustody {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials:"+method) {
			http.Error(w, `{"error":{"status":"UNAVAILABLE"}}`, status)
			return
		}
		e.vault.api(w, r)
	}))
	t.Cleanup(srv.Close)
	c := e.vault.custody()
	c.Client, c.Endpoint = srv.Client(), srv.URL+"/v1/"
	return c
}

// The vault failing at sign-in, or at the end of its consent leg, is
// server_error (try again), not access_denied: Google failing isn't
// Google refusing. A grant only the user can repair stays access_denied.
func TestCustodyOutageIsServerError(t *testing.T) {
	for name, tc := range map[string]struct {
		method string
		status int
	}{
		"retrieve 503": {"retrieve", http.StatusServiceUnavailable},
		"retrieve 429": {"retrieve", http.StatusTooManyRequests},
		"finalize 500": {"finalize", http.StatusInternalServerError},
	} {
		e := newTestEnv(t, false, nil)
		e.as.cfg.Custody = outageVault(t, e, tc.method, tc.status)
		loc, page := newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
		if loc == nil || loc.Query().Get("error") != "server_error" {
			t.Errorf("vault %s: redirect %v page %q, want server_error", name, loc, page)
		}
	}

	// Google's tokeninfo failing behind the vault is Google failing too.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	e := newTestEnv(t, false, func(c *Config) { c.Google.TokenInfoURL = down.URL })
	e.vault.grants[alice.Sub] = alice
	loc, page := newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
	if loc == nil || loc.Query().Get("error") != "server_error" {
		t.Errorf("tokeninfo 503: redirect %v page %q, want server_error", loc, page)
	}

	e = newTestEnv(t, false, nil)
	e.vault.consentAs = bob // alice signs in, bob consents
	loc, page = newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
	if loc == nil || loc.Query().Get("error") != "access_denied" {
		t.Errorf("another account's grant: redirect %v page %q, want access_denied", loc, page)
	}

	// Sealed custody's refusals are the user's to repair too.
	for _, gt := range []*googleTokens{
		{AccessToken: "ya29.x", Scope: scopeOpenID + " " + scopeEmail, RefreshToken: "1//x"},
		{AccessToken: "ya29.x", Scope: scopeCloudPlatform},
	} {
		if _, _, err := (SealedCustody{}).begin(t.Context(), nil, alice, gt, ""); custodyError(err) != "access_denied" {
			t.Errorf("sealed begin with %+v: %v is %s, want access_denied", gt, err, custodyError(err))
		}
	}
}

// Google's error value at the callback is anyone's to write: the client
// hears a fixed description, not the value.
func TestGoogleErrorNotEchoed(t *testing.T) {
	e := newTestEnv(t, true, nil)
	b := newBrowser(t)
	toGoogle, page := approve(t, b, e.srv.URL, consentForm(t, b, e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil)))
	g, err := url.Parse(toGoogle)
	if err != nil || g.Query().Get("state") == "" {
		t.Fatalf("approve: %q page %q", toGoogle, page)
	}
	cb := e.srv.URL + "/callback?" + url.Values{"state": {g.Query().Get("state")}, "error": {`"><script>x</script>`}}.Encode()
	resp, err := noFollow(b).Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil || loc.Query().Get("error") != "access_denied" || strings.Contains(loc.Query().Get("error_description"), "script") {
		t.Errorf("Google error at the callback: %v, want access_denied with a fixed description", loc)
	}
}

// The allow-list refusal doesn't name the account: the redirect lands
// in the client and its logs.
func TestRefusalDoesNotNameTheAccount(t *testing.T) {
	e := newTestEnv(t, false, nil)
	e.google.signInAs = mallory
	loc, page := newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
	if loc == nil || loc.Query().Get("error") != "access_denied" || strings.Contains(loc.String(), "mallory") {
		t.Errorf("refusal: redirect %v page %q, want access_denied without the account", loc, page)
	}
}

// inDescCharset reports whether s keeps to RFC 6749's error_description
// characters: %x20-21 / %x23-5B / %x5D-7E.
func inDescCharset(s string) bool {
	for _, c := range []byte(s) {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// Every error_description keeps to RFC 6749's character set, whatever
// it quotes: in a redirect, in a /token error and in a registration
// error.
func TestErrorDescriptionCharset(t *testing.T) {
	const raw = "a \"b\" \\c\n\x7fé"
	if got := cleanDesc(raw); got != "a 'b' ?c????" {
		t.Errorf("cleanDesc = %q", got)
	}

	w := httptest.NewRecorder()
	redirectError(w, httptest.NewRequest(http.MethodGet, "/callback", nil), clientRedirect, "st", "https://kg.example", &authError{"server_error", raw})
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if d := loc.Query().Get("error_description"); d == "" || !inDescCharset(d) {
		t.Errorf("redirect error_description %q", d)
	}

	w = httptest.NewRecorder()
	writeTokenError(w, &tokenError{Code: "invalid_client", Desc: raw})
	var te tokenError
	if err := json.Unmarshal(w.Body.Bytes(), &te); err != nil || te.Desc == "" || !inDescCharset(te.Desc) {
		t.Errorf("/token error_description %q (%v)", te.Desc, err)
	}

	e := newTestEnv(t, false, nil)
	resp, err := http.Post(e.srv.URL+"/register", "application/json", strings.NewReader(`{"redirect_uris":["ftp://x.example/cb"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m struct {
		Error string `json:"error"`
		Desc  string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || m.Error != "invalid_redirect_uri" || !inDescCharset(m.Desc) {
		t.Errorf("registration error %+v (%v)", m, err)
	}
}

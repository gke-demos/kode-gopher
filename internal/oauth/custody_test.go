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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The custody tests below are ported from go-steer/steward#20
// (authserver/custody_test.go at da13930).

// scriptedVault answers retrieve and finalize from a script, one answer
// per call, and Google's tokeninfo for the tokens it hands out.
type scriptedVault struct {
	mu      sync.Mutex
	answers []string // JSON bodies (or "!503" for an outage, "!400 <message>" for a refused call)
	calls   []string
}

func (v *scriptedVault) handler(t *testing.T, info map[string]string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.calls = append(v.calls, r.URL.Path[strings.LastIndex(r.URL.Path, ":")+1:])
		if len(v.answers) == 0 {
			t.Errorf("unscripted vault call %s", r.URL.Path)
			http.Error(w, "unscripted", http.StatusInternalServerError)
			return
		}
		a := v.answers[0]
		v.answers = v.answers[1:]
		if a == "!503" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"status":"UNAVAILABLE"}}`))
			return
		}
		if msg, ok := strings.CutPrefix(a, "!400 "); ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"status":"INVALID_ARGUMENT","message":"` + msg + `"}}`))
			return
		}
		_, _ = w.Write([]byte(a))
	})
	mux.HandleFunc("POST /tokeninfo", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		body, ok := info[r.PostForm.Get("access_token")]
		if !ok {
			http.Error(w, `{"error":"invalid_token","error_description":"Invalid Value"}`, http.StatusBadRequest)
			return
		}
		if rest, ok := strings.CutPrefix(body, "!400 "); ok {
			http.Error(w, rest, http.StatusBadRequest)
			return
		}
		if code, err := strconv.Atoi(strings.TrimPrefix(body, "!")); err == nil {
			http.Error(w, "<html>not Google</html>", code)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	return mux
}

func newScripted(t *testing.T, info map[string]string, answers ...string) (*VaultCustody, *google, *scriptedVault) {
	v := &scriptedVault{answers: answers}
	srv := httptest.NewServer(v.handler(t, info))
	t.Cleanup(srv.Close)
	g := &google{client: &GoogleClient{TokenInfoURL: srv.URL + "/tokeninfo"}, hc: srv.Client()}
	return &VaultCustody{AuthProvider: "projects/p/locations/l/authProviders/kg", Client: srv.Client(), Endpoint: srv.URL + "/v1/"}, g, v
}

const (
	consentAnswer = `{"uriConsentRequired":{"authorizationUri":"https://consent.example/x","consentNonce":"n1","uid":"u1"}}`
	tokA          = `{"success":{"token":"tokA"}}`
	tokShort      = `{"success":{"token":"tokShort"}}`
)

var vaultInfo = map[string]string{
	"tokA":      `{"sub":"111","scope":"openid https://www.googleapis.com/auth/cloud-platform","expires_in":"3600"}`,
	"tokShort":  `{"sub":"111","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"30"}`,
	"tokShort2": `{"sub":"111","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"40"}`,
	"tokMid":    `{"sub":"111","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"240"}`,
	"tokEdge":   `{"sub":"111","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"170"}`,
	"tokBob":    `{"sub":"222","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"3600"}`,
	"tokNoCP":   `{"sub":"111","scope":"openid","expires_in":"3600"}`,
	"tokNoSub":  `{"scope":"openid","expires_in":"3600"}`,
	"tokBadExp": `{"sub":"111","scope":"openid","expires_in":"soon"}`,
	"tokProxy":  "!403", // tokeninfo answers 403: an egress proxy, not Google
	"tokProxy4": "!400", // 400 without Google's invalid_token body
	"tokEmpty4": "!400 ",
	"tokOther4": `!400 {"error":"invalid_request"}`,
	"tokMoved":  "!404",
	"tokDown":   "!502",
}

// scriptedTokenRE matches the scripted vault's token names.
var scriptedTokenRE = regexp.MustCompile(`tok[A-Z]`)

func TestVaultCustodyEdges(t *testing.T) {
	ctx := t.Context()
	q := func(kv ...string) url.Values {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	c1 := Consent{Nonce: "n1", UID: "u1"}

	// begin: consent detour, incomplete consent, a token straight away.
	v, g, _ := newScripted(t, vaultInfo, consentAnswer)
	if gr, c, err := v.begin(ctx, g, alice, nil, "https://kg.example/continue"); err != nil || gr != nil || c == nil || c.URL != "https://consent.example/x" {
		t.Errorf("begin with consent: %v %+v %v", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"uriConsentRequired":{"authorizationUri":""}}`)
	if _, _, err := v.begin(ctx, g, alice, nil, ""); err == nil {
		t.Error("begin accepted an incomplete consent request")
	}
	v, g, _ = newScripted(t, vaultInfo, tokA)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || c != nil || gr.AccessToken != "tokA" {
		t.Errorf("begin with a stored grant: %+v %v %v", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, "!503")
	if _, _, err := v.begin(ctx, g, alice, nil, ""); err == nil || errors.Is(err, errReauth) || !strings.Contains(err.Error(), "UNAVAILABLE") {
		t.Errorf("begin during an outage: %v", err)
	}

	// finish: callback checks, then finalize and retrieve.
	v, g, _ = newScripted(t, vaultInfo)
	if _, err := v.finish(ctx, g, alice, c1, q("uuid", "other", "user_id_validation_state", "s"), ""); err == nil {
		t.Error("finish accepted another flow's callback")
	}
	if _, err := v.finish(ctx, g, alice, c1, q("uuid", "u1"), ""); err == nil {
		t.Error("finish accepted a callback without a validation state")
	}
	v, g, sv := newScripted(t, vaultInfo, "", tokA)
	if gr, err := v.finish(ctx, g, alice, c1, q("uuid", "u1", "user_id_validation_state", "s"), ""); err != nil || gr.AccessToken != "tokA" {
		t.Errorf("finish: %+v %v", gr, err)
	}
	if strings.Join(sv.calls, ",") != "finalize,retrieve" {
		t.Errorf("finish called %v", sv.calls)
	}
	v, g, _ = newScripted(t, vaultInfo, "!503")
	if _, err := v.finish(ctx, g, alice, c1, q("user_id_validation_state", "s"), ""); err == nil {
		t.Error("finish ignored a finalize failure")
	}
	v, g, _ = newScripted(t, vaultInfo, "", "!503")
	if _, err := v.finish(ctx, g, alice, c1, q("user_id_validation_state", "s"), ""); err == nil {
		t.Error("finish ignored a retrieve failure")
	}
	v, g, _ = newScripted(t, vaultInfo, "", consentAnswer)
	if _, err := v.finish(ctx, g, alice, c1, q("user_id_validation_state", "s"), ""); err == nil {
		t.Error("finish accepted a vault still asking for consent")
	}

	// refresh and grant: every way a stored grant is refused (reauth:
	// invalid_grant, sign in again) or fails (a retry may work).
	for name, tc := range map[string]struct {
		answers []string
		reauth  bool
	}{
		"needs consent again": {[]string{consentAnswer}, true},
		"declined":            {[]string{`{"consentRejected":{}}`}, true},
		"another account":     {[]string{`{"success":{"token":"tokBob"}}`}, true},
		"no cloud-platform":   {[]string{`{"success":{"token":"tokNoCP"}}`}, true},
		"no sub":              {[]string{`{"success":{"token":"tokNoSub"}}`}, false},
		"bad expiry":          {[]string{`{"success":{"token":"tokBadExp"}}`}, false},
		"outage":              {[]string{"!503"}, false},
		"malformed":           {[]string{`{`}, false},
		"short, still short":  {[]string{tokShort, `{"success":{"token":"tokShort2"}}`}, false},
		"short, same token":   {[]string{tokShort, tokShort}, false},
		"short, then outage":  {[]string{tokShort, "!503"}, false},
		"short, then refused": {[]string{tokShort, `{"success":{"token":"tokGone"}}`}, false},
		// The vault busy is not the grant gone.
		"pending":             {[]string{`{"pending":{}}`}, false},
		"empty":               {[]string{`{}`}, false},
		"no token":            {[]string{`{"success":{}}`}, false},
		"short, then pending": {[]string{tokShort, `{"pending":{}}`}, false},
		// Google refusing the vault's token (tokeninfo 400) forces one
		// refresh; consent then, or Google refusing the new token too, is
		// a refusal. The same token back is the vault failing.
		"refused, then consent": {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer}, true},
		"refused twice":         {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokGone2"}}`}, true},
		"refused, same token":   {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokGone"}}`}, false},
		"refused, then outage":  {[]string{`{"success":{"token":"tokGone"}}`, "!503"}, false},
		"refused, then pending": {[]string{`{"success":{"token":"tokGone"}}`, `{"pending":{}}`}, false},
		"refused, then short":   {[]string{`{"success":{"token":"tokGone"}}`, tokShort}, false},
		"refused, then another": {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokBob"}}`}, true},
		"refused, then 502":     {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokDown"}}`}, false},
		// Anything else from tokeninfo is Google, or something in front
		// of it, failing.
		"tokeninfo 403":             {[]string{`{"success":{"token":"tokProxy"}}`}, false},
		"tokeninfo 400, not Google": {[]string{`{"success":{"token":"tokProxy4"}}`}, false},
		"tokeninfo 400, empty":      {[]string{`{"success":{"token":"tokEmpty4"}}`}, false},
		"tokeninfo 400, other":      {[]string{`{"success":{"token":"tokOther4"}}`}, false},
		"tokeninfo 404":             {[]string{`{"success":{"token":"tokMoved"}}`}, false},
		"tokeninfo 502":             {[]string{`{"success":{"token":"tokDown"}}`}, false},
	} {
		v, g, sv := newScripted(t, vaultInfo, tc.answers...)
		gr, err := v.refresh(ctx, g, alice, "", "")
		if len(sv.calls) != len(tc.answers) {
			t.Errorf("%s: %d vault calls, want every scripted answer used (%d)", name, len(sv.calls), len(tc.answers))
		}
		if err == nil {
			t.Errorf("%s: refresh accepted %+v", name, gr)
			continue
		}
		if errors.Is(err, errReauth) != tc.reauth {
			t.Errorf("%s: %v, reauth=%v want %v", name, err, errors.Is(err, errReauth), tc.reauth)
		}
		if scriptedTokenRE.MatchString(err.Error()) {
			t.Errorf("%s: the error %q quotes a token", name, err)
		}
	}
	v, g, sv = newScripted(t, vaultInfo, tokShort, tokA)
	if gr, err := v.refresh(ctx, g, alice, "", ""); err != nil || gr.AccessToken != "tokA" || len(sv.calls) != 2 {
		t.Errorf("short token force-refreshed: %+v %v %v", gr, err, sv.calls)
	}
	// The vault won't force-refresh a token that is still valid, and says
	// so by asking for consent: not the grant gone. A forced refresh that
	// brings no better token, short of a refusal, leaves the valid one in
	// use if it has minLiveTokenLife left (tokMid, 4 min), and is a plain
	// error if not (tokShort, 30 s; tokEdge, 170 s, just under the floor).
	for name, second := range map[string]string{
		"consent":       consentAnswer,
		"same token":    `{"success":{"token":"tokMid"}}`,
		"pending":       `{"pending":{}}`,
		"empty":         `{}`,
		"outage":        "!503",
		"refused":       `{"success":{"token":"tokGone"}}`,
		"still short":   tokShort,
		"tokeninfo 502": `{"success":{"token":"tokDown"}}`,
	} {
		v, g, sv := newScripted(t, vaultInfo, `{"success":{"token":"tokMid"}}`, second)
		if gr, err := v.refresh(ctx, g, alice, "", ""); err != nil || gr.AccessToken != "tokMid" || len(sv.calls) != 2 {
			t.Errorf("4 min token, then %s: %+v %v %v, want tokMid", name, gr, err, sv.calls)
		}
		for _, short := range []string{"tokShort", "tokEdge"} {
			v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"`+short+`"}}`, second)
			if gr, err := v.refresh(ctx, g, alice, "", ""); err == nil || errors.Is(err, errReauth) {
				t.Errorf("%s, then %s: %+v %v, want a plain error", short, name, gr, err)
			}
		}
	}
	for name, second := range map[string]string{
		"declined":        `{"consentRejected":{}}`,
		"another account": `{"success":{"token":"tokBob"}}`,
	} {
		v, g, _ := newScripted(t, vaultInfo, `{"success":{"token":"tokMid"}}`, second)
		if gr, err := v.refresh(ctx, g, alice, "", ""); !errors.Is(err, errReauth) {
			t.Errorf("4 min token, then %s: %+v %v, want reauth: a refusal wins", name, gr, err)
		}
	}
	v, g, sv = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, tokA)
	if gr, err := v.refresh(ctx, g, alice, "", ""); err != nil || gr.AccessToken != "tokA" || len(sv.calls) != 2 {
		t.Errorf("token Google refused force-refreshed: %+v %v %v", gr, err, sv.calls)
	}
	// At sign-in the user is in the browser: a grant gone behind a
	// refused token detours through consent; refused twice is
	// access_denied.
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, consentAnswer)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || gr != nil || c == nil || c.URL != "https://consent.example/x" {
		t.Errorf("begin, Google refuses the vault's token, then consent: %+v %+v %v, want a consent detour", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, `{"uriConsentRequired":{"authorizationUri":""}}`)
	if _, c, err := v.begin(ctx, g, alice, nil, ""); err == nil || c != nil || errors.Is(err, errReauth) {
		t.Errorf("begin, refused then an incomplete consent request: %+v %v", c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokGone2"}}`)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); !errors.Is(err, errReauth) || custodyError(err) != "access_denied" {
		t.Errorf("begin, Google refuses the vault's token twice: %+v %+v %v, want access_denied", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, "", `{"success":{"token":"tokGone"}}`, consentAnswer)
	if _, err := v.finish(ctx, g, alice, c1, q("user_id_validation_state", "s"), ""); !errors.Is(err, errReauth) {
		t.Errorf("finish, Google refuses the fresh grant's token, then consent: %v, want reauth", err)
	}
	// A vault error that quotes the forced token back keeps it out of the
	// error, and so out of the log.
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, "!400 forceRefreshToken tokGone is not the latest token")
	if _, err := v.refresh(ctx, g, alice, "", ""); err == nil || strings.Contains(err.Error(), "tokGone") || !strings.Contains(err.Error(), "INVALID_ARGUMENT") {
		t.Errorf("forced refresh refused, quoting the token: %v", err)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"pending":{}}`)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err == nil || custodyError(err) != "server_error" {
		t.Errorf("begin, vault pending: %+v %+v %v, want server_error", gr, c, err)
	}
}

// vaultSignIn signs alice in through the vault's consent and exchanges
// the code, returning the client ID and the first tokens.
func vaultSignIn(e *testEnv) (string, tokenResp) {
	e.t.Helper()
	id := e.register()
	v := rand.Text() + rand.Text()
	first := e.token(exchangeForm(id, e.code(id, v), v), "", "")
	if first.status != http.StatusOK {
		e.t.Fatalf("exchange: %+v", first)
	}
	return id, first
}

// scriptVault makes e's vault answer its next retrieves with answers.
func scriptVault(e *testEnv, answers ...string) {
	e.vault.mu.Lock()
	defer e.vault.mu.Unlock()
	e.vault.script = answers
}

// A nearly spent vault token whose forced refresh the vault declines (by
// asking for consent) at sign-in: with 4 minutes left it's used, and the
// code exchanges for an access token; with 30 seconds left the sign-in
// fails at the authorize redirect, before a code that could never be
// exchanged is issued. Neither is access_denied.
func TestVaultNearlySpentTokenAtSignIn(t *testing.T) {
	for _, c := range []struct {
		life int
		ok   bool
	}{{240, true}, {30, false}} {
		e := newTestEnv(t, false, nil)
		e.google.mu.Lock()
		tok := e.google.mint(alice, scopeOpenID+" "+scopeEmail+" "+scopeCloudPlatform, c.life)
		e.google.mu.Unlock()
		scriptVault(e, `{"success":{"token":"`+tok+`"}}`, consentAnswer)
		id, v := e.register(), rand.Text()+rand.Text()
		loc, page := newBrowser(t).visit(e.authorizeURL(id, v, nil))
		if !c.ok {
			if loc == nil || loc.Query().Get("error") != "server_error" {
				t.Errorf("%d s left, refresh declined: %v %q, want server_error at the redirect", c.life, loc, page)
			}
			continue
		}
		if loc == nil || loc.Query().Get("code") == "" {
			t.Errorf("%d s left, refresh declined: %v %q, want a code", c.life, loc, page)
			continue
		}
		if r := e.token(exchangeForm(id, loc.Query().Get("code"), v), "", ""); r.status != http.StatusOK || r.Expires > int64(c.life-60) {
			t.Errorf("%d s left: exchange %+v, want an access token that stops a minute before the Google token", c.life, r)
		}
	}
}

// A busy vault ("pending") at refresh is the vault failing, not refusing:
// server_error, and the refresh token survives for the retry, rather than
// invalid_grant, which signs the user out. At sign-in it's server_error,
// not access_denied. An answer with nothing usable in it is the same.
func TestVaultPendingIsNotASignOut(t *testing.T) {
	for _, answer := range []string{`{"pending":{}}`, `{}`} {
		e := newTestEnv(t, false, nil)
		id, first := vaultSignIn(e)
		scriptVault(e, answer)
		if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "server_error" || r.status != http.StatusInternalServerError {
			t.Errorf("refresh while the vault answers %s: %+v, want server_error", answer, r)
		}
		if r := e.token(refreshForm(id, first.Refresh), "", ""); r.status != http.StatusOK {
			t.Errorf("retry once the vault answers: %+v", r)
		}

		e = newTestEnv(t, false, nil)
		scriptVault(e, answer)
		loc, page := newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
		if loc == nil || loc.Query().Get("error") != "server_error" {
			t.Errorf("sign-in while the vault answers %s: %v %q, want server_error", answer, loc, page)
		}
	}
}

// Google's tokeninfo refusing the vault's token (400 invalid_token) is a
// refusal once a forced refresh hasn't cured it: access_denied at
// sign-in and invalid_grant at refresh, not server_error. A forced
// refresh that asks for consent sends a signing-in user through
// consent; one that cures it carries on. The refused tokens reach
// neither the log nor the client.
func TestVaultTokenGoogleRefuses(t *testing.T) {
	logs := captureLog(t)
	const dead, dead2 = `{"success":{"token":"ya29.revoked"}}`, `{"success":{"token":"ya29.revoked-too"}}`
	e := newTestEnv(t, false, nil)
	scriptVault(e, dead, dead2)
	loc, page := newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
	if loc == nil || loc.Query().Get("error") != "access_denied" {
		t.Errorf("sign-in, Google refuses the vault's token: %v %q, want access_denied", loc, page)
	}
	if loc != nil && strings.Contains(loc.String(), "ya29.revoked") {
		t.Errorf("the refused token reached the client: %v", loc)
	}

	// The vault refuses the forced refresh, quoting the token back: a
	// plain error, logged with the token replaced.
	e = newTestEnv(t, false, nil)
	scriptVault(e, dead, "!400 forceRefreshToken $forced is not the latest token")
	loc, page = newBrowser(t).visit(e.authorizeURL(e.register(), rand.Text()+rand.Text(), nil))
	if loc == nil || loc.Query().Get("error") != "server_error" {
		t.Errorf("sign-in, the forced refresh refused: %v %q, want server_error", loc, page)
	}
	if !strings.Contains(logs.String(), "forceRefreshToken [token] is not") {
		t.Errorf("the vault's refusal wasn't logged with the token replaced:\n%s", logs)
	}

	// No grant stored for alice: the forced retrieve reaches the fake
	// vault, which asks for consent; sign-in goes through it and
	// succeeds.
	e = newTestEnv(t, false, nil)
	scriptVault(e, dead)
	vaultSignIn(e)
	e.vault.mu.Lock()
	_, ok := e.vault.grants[alice.Sub]
	e.vault.mu.Unlock()
	if !ok {
		t.Error("sign-in after a refused token didn't go through consent")
	}

	e = newTestEnv(t, false, nil)
	id, first := vaultSignIn(e)
	scriptVault(e, dead, dead2)
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("refresh, Google refuses the vault's token: %+v, want invalid_grant", r)
	}
	if !strings.Contains(logs.String(), "needs a new sign-in: vault: Google refuses the force-refreshed token") {
		t.Errorf("the sign-out wasn't logged:\n%s", logs)
	}

	e = newTestEnv(t, false, nil)
	id, first = vaultSignIn(e)
	scriptVault(e, dead) // the forced retrieve reaches the fake vault
	r := e.token(refreshForm(id, first.Refresh), "", "")
	e.vault.mu.Lock()
	forced := e.vault.forced
	e.vault.mu.Unlock()
	if r.status != http.StatusOK || forced != 1 {
		t.Errorf("refresh, a token Google refuses then a forced one: %+v, forced %d", r, forced)
	}

	if strings.Contains(logs.String(), "ya29.revoked") {
		t.Errorf("a refused token reached the log:\n%s", logs)
	}
}

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
	"crypto/rand"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

// The custody tests below are ported from go-steer/steward#20
// (authserver/custody_test.go at da13930) and go-steer/steward#23
// (at 68397b2).

// scriptedVault answers retrieve and finalize from a script, one answer
// per call, and Google's tokeninfo for the tokens it hands out.
type scriptedVault struct {
	mu      sync.Mutex
	answers []string // JSON bodies (or "!503" for an outage, "!400 <message>" for a refused call)
	calls   []string
	forced  []string          // forceRefreshToken of each call ("" if none)
	waits   []time.Duration   // nearly spent tokens waited out
	info    map[string]string // tokeninfo answers, by token
	expire  bool              // a wait expires the tokens it outlasts
}

// sleep stands in for waiting out a nearly spent token: it records the
// wait and, if v.expire, expires every token with no more life than that.
func (v *scriptedVault) sleep(_ context.Context, d time.Duration) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.waits = append(v.waits, d)
	for tok, body := range v.info {
		var ti struct {
			ExpiresIn string `json:"expires_in"`
		}
		if json.Unmarshal([]byte(body), &ti) != nil {
			continue
		}
		if secs, err := strconv.Atoi(ti.ExpiresIn); err == nil && v.expire && time.Duration(secs)*time.Second <= d {
			delete(v.info, tok)
		}
	}
	return nil
}

func (v *scriptedVault) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ForceRefreshToken string `json:"forceRefreshToken"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		v.mu.Lock()
		defer v.mu.Unlock()
		v.calls = append(v.calls, r.URL.Path[strings.LastIndex(r.URL.Path, ":")+1:])
		v.forced = append(v.forced, body.ForceRefreshToken)
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
		v.mu.Lock()
		body, ok := v.info[r.PostForm.Get("access_token")]
		v.mu.Unlock()
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

// newScripted is a VaultCustody in front of a scripted vault whose
// tokeninfo answers from a copy of info. Waiting out a nearly spent token
// takes no time and, as it would, expires the tokens it outlasts.
func newScripted(t *testing.T, info map[string]string, answers ...string) (*VaultCustody, *google, *scriptedVault) {
	v := &scriptedVault{answers: answers, info: maps.Clone(info), expire: true}
	srv := httptest.NewServer(v.handler(t))
	t.Cleanup(srv.Close)
	g := &google{client: &GoogleClient{TokenInfoURL: srv.URL + "/tokeninfo"}, hc: srv.Client()}
	return &VaultCustody{AuthProvider: "projects/p/locations/l/authProviders/kg", Client: srv.Client(), Endpoint: srv.URL + "/v1/", sleeper: &sleeper{v.sleep}}, g, v
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
	"tok100":    `{"sub":"111","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"100"}`,
	"tok80":     `{"sub":"111","scope":"https://www.googleapis.com/auth/cloud-platform","expires_in":"80"}`,
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
var scriptedTokenRE = regexp.MustCompile(`tok[A-Z0-9]`)

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

	testVaultRefusals(t)
	testVaultRenewal(t)
	testVaultConsentRecheck(t)
	testVaultSignIn(t)
	testVaultWaitedConsent(t)

	v, g, sv = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, tokA)
	if gr, err := v.refresh(ctx, g, alice, "", ""); err != nil || gr.AccessToken != "tokA" || len(sv.calls) != 2 {
		t.Errorf("token Google refused force-refreshed: %+v %v %v", gr, err, sv.calls)
	}
	// At sign-in the user is in the browser: a grant gone behind a
	// refused token detours through consent (checked once more first);
	// refused twice is access_denied.
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, consentAnswer, consentAnswer)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || gr != nil || c == nil || c.URL != "https://consent.example/x" {
		t.Errorf("begin, Google refuses the vault's token, then consent: %+v %+v %v, want a consent detour", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, `{"uriConsentRequired":{"authorizationUri":""}}`, `{"uriConsentRequired":{"authorizationUri":""}}`)
	if _, c, err := v.begin(ctx, g, alice, nil, ""); err == nil || c != nil || errors.Is(err, errReauth) {
		t.Errorf("begin, refused then an incomplete consent request: %+v %v", c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokGone2"}}`)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); !errors.Is(err, errReauth) || custodyError(err) != "access_denied" {
		t.Errorf("begin, Google refuses the vault's token twice: %+v %+v %v, want access_denied", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, "", `{"success":{"token":"tokGone"}}`, consentAnswer, consentAnswer)
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

// testVaultRefusals: refresh and grant, every way a stored grant is
// refused (reauth: invalid_grant, sign in again) or fails (a retry may
// work).
func testVaultRefusals(t *testing.T) {
	ctx := t.Context()
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
		// A nearly spent token is waited out, then the vault asked again
		// (plainly): another short token, an outage or the vault busy is
		// a failure; consent, the grant gone. The expired token back is
		// force-refreshed, as is any token Google refuses.
		"short, still short":           {[]string{tokShort, `{"success":{"token":"tokShort2"}}`}, false},
		"short, then outage":           {[]string{tokShort, "!503"}, false},
		"short, then consent":          {[]string{tokShort, consentAnswer}, true},
		"short, then declined":         {[]string{tokShort, `{"consentRejected":{}}`}, true},
		"short, then refused, consent": {[]string{tokShort, `{"success":{"token":"tokGone"}}`, consentAnswer, consentAnswer}, true},
		"short, then refused, outage":  {[]string{tokShort, `{"success":{"token":"tokGone"}}`, "!503"}, false},
		// Consent to renew the token kode-gopher itself waited out isn't
		// yet read as the grant gone (a vault judging expiry by its own
		// expireTime would answer so for a live grant).
		"short, expired back, consent":     {[]string{tokShort, tokShort, consentAnswer, tokShort}, false},
		"short, expired back, same token":  {[]string{tokShort, tokShort, tokShort}, false},
		"short, expired back, pending":     {[]string{tokShort, tokShort, `{"pending":{}}`}, false},
		"short, expired back, still short": {[]string{tokShort, tokShort, `{"success":{"token":"tokShort2"}}`}, false},
		// The vault busy is not the grant gone.
		"pending":             {[]string{`{"pending":{}}`}, false},
		"empty":               {[]string{`{}`}, false},
		"no token":            {[]string{`{"success":{}}`}, false},
		"short, then pending": {[]string{tokShort, `{"pending":{}}`}, false},
		// Google refusing the vault's token (tokeninfo 400) forces one
		// refresh; consent then (checked once more), or Google refusing
		// the new token too, is a refusal. The same token back is the
		// vault failing.
		"refused, then consent": {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, `{"success":{"token":"tokGone"}}`}, true},
		// Checked again: consent there too, the vault busy, or a token
		// Google refuses or that is short keep the verdict; an outage, or
		// tokeninfo failing, is a failure.
		"refused, consent, consent again": {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, consentAnswer}, true},
		"refused, consent, pending":       {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, `{"pending":{}}`}, true},
		"refused, consent, another gone":  {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, `{"success":{"token":"tokGone2"}}`}, true},
		"refused, consent, a short token": {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, tokShort}, true},
		"refused, consent, tokeninfo 502": {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, `{"success":{"token":"tokDown"}}`}, false},
		"refused, consent, outage":        {[]string{`{"success":{"token":"tokGone"}}`, consentAnswer, "!503"}, false},
		"refused twice":                   {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokGone2"}}`}, true},
		"refused, same token":             {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokGone"}}`}, false},
		"refused, then outage":            {[]string{`{"success":{"token":"tokGone"}}`, "!503"}, false},
		"refused, then pending":           {[]string{`{"success":{"token":"tokGone"}}`, `{"pending":{}}`}, false},
		"refused, then short":             {[]string{`{"success":{"token":"tokGone"}}`, tokShort}, false},
		"refused, then 400":               {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokProxy4"}}`}, false},
		"refused, then another":           {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokBob"}}`}, true},
		"refused, then 502":               {[]string{`{"success":{"token":"tokGone"}}`, `{"success":{"token":"tokDown"}}`}, false},
		// Anything else from tokeninfo is Google, or something in front
		// of it, failing. A 400 is Google refusing only with error
		// invalid_token in it.
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
}

// testVaultRenewal: a token Google still accepts is never
// force-refreshed (the vault answers that with consent). At refresh one
// with minRefreshTokenLife (75 s) left is used as it is; one with less is
// waited out, and the vault asked again, plainly.
func testVaultRenewal(t *testing.T) {
	ctx := t.Context()
	for name, tc := range map[string]struct {
		answers []string
		want    string
		forced  []string
		waits   int
	}{
		"4 min token":                      {[]string{`{"success":{"token":"tokMid"}}`}, "tokMid", []string{""}, 0},
		"100 s token":                      {[]string{`{"success":{"token":"tok100"}}`}, "tok100", []string{""}, 0},
		"80 s token":                       {[]string{`{"success":{"token":"tok80"}}`}, "tok80", []string{""}, 0},
		"30 s token, then a fresh one":     {[]string{tokShort, tokA}, "tokA", []string{"", ""}, 1},
		"30 s token back expired, forced":  {[]string{tokShort, tokShort, tokA}, "tokA", []string{"", "", "tokShort"}, 1},
		"30 s token, then refused, forced": {[]string{tokShort, `{"success":{"token":"tokGone"}}`, tokA}, "tokA", []string{"", "", "tokGone"}, 1},
	} {
		v, g, sv := newScripted(t, vaultInfo, tc.answers...)
		gr, err := v.refresh(ctx, g, alice, "", "")
		if err != nil || gr.AccessToken != tc.want {
			t.Errorf("%s: %+v %v, want %s", name, gr, err, tc.want)
		}
		if !slices.Equal(sv.forced, tc.forced) || len(sv.waits) != tc.waits {
			t.Errorf("%s: forced %q, waits %v; want forced %q, %d waits", name, sv.forced, sv.waits, tc.forced, tc.waits)
		}
		// The wait is the token's life (30 s) and expirySkew.
		for _, d := range sv.waits {
			if d < 29*time.Second || d > 30*time.Second+expirySkew {
				t.Errorf("%s: waited %v for a 30 s token", name, d)
			}
		}
	}
	// Waited out and still valid (Google and the vault disagree on the
	// time): a failure, and one wait only.
	v, g, sv := newScripted(t, vaultInfo, tokShort, tokShort)
	sv.expire = false
	if gr, err := v.refresh(ctx, g, alice, "", ""); err == nil || errors.Is(err, errReauth) || len(sv.waits) != 1 || !slices.Equal(sv.forced, []string{"", ""}) {
		t.Errorf("30 s token, still valid after the wait: %+v %v, waits %v forced %q", gr, err, sv.waits, sv.forced)
	}
	// The request ending during the (real) wait ends it: a failure, not a
	// sign-out, and no second retrieve.
	v, g, sv = newScripted(t, vaultInfo, tokShort)
	v.sleeper = nil
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	if gr, err := v.refresh(cctx, g, alice, "", ""); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errReauth) || len(sv.calls) != 1 || time.Since(start) > 10*time.Second {
		t.Errorf("30 s token, request ended during the wait: %+v %v %v after %v", gr, err, sv.calls, time.Since(start))
	}
}

// testVaultConsentRecheck: consent to a forced refresh, then a valid
// token on the plain retrieve (another client of the user forced the same
// expired token first): that token is used, at refresh and at sign-in.
func testVaultConsentRecheck(t *testing.T) {
	ctx := t.Context()
	v, g, sv := newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, consentAnswer, tokA)
	if gr, err := v.refresh(ctx, g, alice, "", ""); err != nil || gr.AccessToken != "tokA" || !slices.Equal(sv.forced, []string{"", "tokGone", ""}) {
		t.Errorf("refused, consent to the force, then a fresh token: %+v %v, forced %q", gr, err, sv.forced)
	}
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, consentAnswer, tokA)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || c != nil || gr == nil || gr.AccessToken != "tokA" {
		t.Errorf("begin, refused, consent to the force, then a fresh token: %+v %+v %v", gr, c, err)
	}
	// At sign-in a recheck that brings a token Google refuses, or a short
	// one, leaves the consent detour.
	for _, third := range []string{`{"success":{"token":"tokGone2"}}`, tokShort} {
		v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, consentAnswer, third)
		if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || gr != nil || c == nil {
			t.Errorf("begin, refused, consent to the force, then %s: %+v %+v %v, want the consent detour", third, gr, c, err)
		}
	}
	v, g, _ = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, consentAnswer, `{"success":{"token":"tokBob"}}`)
	if _, err := v.refresh(ctx, g, alice, "", ""); !errors.Is(err, errReauth) {
		t.Errorf("refused, consent to the force, then another account's token: %v, want reauth: the token is checked", err)
	}
}

// testVaultSignIn: at sign-in the code is exchanged a moment later, so a
// token needs minSignInTokenLife (90 s); one with less is waited out (up
// to 93 s).
func testVaultSignIn(t *testing.T) {
	ctx := t.Context()
	for _, tok := range []string{"tokMid", "tok100"} {
		v, g, sv := newScripted(t, vaultInfo, `{"success":{"token":"`+tok+`"}}`)
		if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || c != nil || gr.AccessToken != tok || len(sv.calls) != 1 {
			t.Errorf("begin, %s: %+v %+v %v %v", tok, gr, c, err, sv.calls)
		}
	}
	v, g, sv := newScripted(t, vaultInfo, `{"success":{"token":"tok80"}}`, tokA)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || c != nil || gr.AccessToken != "tokA" || len(sv.waits) != 1 || sv.waits[0] > 80*time.Second+expirySkew {
		t.Errorf("begin, tok80: %+v %+v %v, waits %v; want it waited out", gr, c, err, sv.waits)
	}
	// Longer than that is a failure at once; only a token handed back
	// short by a forced refresh reaches it at sign-in, as the sign-in
	// wait covers need.
	v, g, sv = newScripted(t, vaultInfo, `{"success":{"token":"tokGone"}}`, `{"success":{"token":"tok80"}}`)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err == nil || custodyError(err) != "server_error" || len(sv.waits) != 0 {
		t.Errorf("begin, refused, then forced to tok80: %+v %+v %v, waits %v; want server_error at once", gr, c, err, sv.waits)
	}
	v, g, sv = newScripted(t, vaultInfo, tokShort, tokA)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || c != nil || gr.AccessToken != "tokA" || len(sv.waits) != 1 || !slices.Equal(sv.forced, []string{"", ""}) {
		t.Errorf("begin, 30 s token: %+v %+v %v, waits %v forced %q", gr, c, err, sv.waits, sv.forced)
	}
	// The user is in the browser: a grant that can't renew once the
	// token expired is a consent detour.
	v, g, _ = newScripted(t, vaultInfo, tokShort, consentAnswer)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || gr != nil || c == nil || c.URL != "https://consent.example/x" {
		t.Errorf("begin, 30 s token, then consent: %+v %+v %v, want a consent detour", gr, c, err)
	}
}

// testVaultWaitedConsent: consent to renew the token that was waited out
// is a consent detour at sign-in, a plain error at finish, and at
// refresh server_error once; the next refresh doesn't wait, forces the
// expired token, and consent then is the grant gone.
func testVaultWaitedConsent(t *testing.T) {
	ctx := t.Context()
	v, g, _ := newScripted(t, vaultInfo, tokShort, tokShort, consentAnswer, consentAnswer)
	if gr, c, err := v.begin(ctx, g, alice, nil, ""); err != nil || gr != nil || c == nil || c.URL != "https://consent.example/x" {
		t.Errorf("begin, 30 s token back expired, then consent: %+v %+v %v, want a consent detour", gr, c, err)
	}
	v, g, _ = newScripted(t, vaultInfo, "", tokShort, tokShort, consentAnswer, consentAnswer)
	if _, err := v.finish(ctx, g, alice, Consent{Nonce: "n1", UID: "u1"}, url.Values{"user_id_validation_state": {"s"}}, ""); err == nil || errors.Is(err, errReauth) {
		t.Errorf("finish, 30 s token back expired, then consent: %v, want a plain error", err)
	}
	v, g, sv := newScripted(t, vaultInfo, tokShort, tokShort, consentAnswer, tokShort)
	if _, err := v.refresh(ctx, g, alice, "", ""); err == nil || errors.Is(err, errReauth) || custodyError(err) != "server_error" {
		t.Errorf("refresh, 30 s token back expired, then consent: %v, want server_error", err)
	}
	sv.mu.Lock()
	sv.answers = []string{tokShort, consentAnswer, tokShort}
	sv.mu.Unlock()
	if _, err := v.refresh(ctx, g, alice, "", ""); !errors.Is(err, errReauth) {
		t.Errorf("the next refresh, expired token forced, then consent: %v, want reauth", err)
	}
	sv.mu.Lock()
	if len(sv.waits) != 1 || !slices.Equal(sv.forced, []string{"", "", "tokShort", "", "", "tokShort", ""}) {
		t.Errorf("waits %v, forced %q; want one wait, and the expired token forced by both refreshes", sv.waits, sv.forced)
	}
	sv.mu.Unlock()
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

// mintFor mints a Google token for alice at e's fake Google, with life
// seconds left, and returns the vault's answer carrying it.
func mintFor(e *testEnv, life int) (tok, answer string) {
	e.google.mu.Lock()
	tok = e.google.mint(alice, scopeOpenID+" "+scopeEmail+" "+scopeCloudPlatform, life)
	e.google.mu.Unlock()
	return tok, `{"success":{"token":"` + tok + `"}}`
}

// googleTokenIn is the Google token sealed in a kode-gopher access token.
func googleTokenIn(t *testing.T, e *testEnv, access string) string {
	t.Helper()
	ti, err := e.as.Verifier()(t.Context(), access, nil)
	if err != nil {
		t.Fatalf("access token refused: %v", err)
	}
	u, _ := ti.Extra[creds.TokenInfoKey].(*creds.OAuthUser)
	if u == nil {
		t.Fatal("access token carries no Google token")
	}
	return u.Token
}

// A nearly spent vault token at sign-in is never force-refreshed (the
// real vault, like the fake, would ask for consent). With 4 minutes left
// it's used, and the access token stops a minute before it; with 100
// seconds left too. With 80 seconds left (under minSignInTokenLife) it's
// waited out like any shorter one. With 30 seconds left it's waited out,
// and the vault's next token, a fresh one, goes in the code. The nearly
// spent token reaches no log.
func TestVaultNearlySpentTokenAtSignIn(t *testing.T) {
	logs := captureLog(t)
	for _, c := range []struct {
		life  int
		fresh bool // the code carries a fresh token, not the short one
		waits int
	}{{240, false, 0}, {100, false, 0}, {80, true, 1}, {30, true, 1}} {
		e := newTestEnv(t, false, nil)
		vaultSignIn(e) // the vault holds alice's grant
		tok, answer := mintFor(e, c.life)
		scriptVault(e, answer)
		id, v := e.register(), rand.Text()+rand.Text()
		loc, page := newBrowser(t).visit(e.authorizeURL(id, v, nil))
		e.vault.mu.Lock()
		forced, waits := e.vault.forced, len(e.vault.waits)
		e.vault.mu.Unlock()
		if forced != 0 || waits != c.waits {
			t.Errorf("%d s left: %d forced refreshes, %d waits; want none forced, %d waits", c.life, forced, waits, c.waits)
		}
		if strings.Contains(logs.String(), tok) {
			t.Errorf("%d s left: the token reached the log", c.life)
		}
		if loc == nil || loc.Query().Get("code") == "" {
			t.Errorf("%d s left: %v %q, want a code", c.life, loc, page)
			continue
		}
		r := e.token(exchangeForm(id, loc.Query().Get("code"), v), "", "")
		switch {
		case r.status != http.StatusOK:
			t.Errorf("%d s left: exchange %+v", c.life, r)
		case !c.fresh && r.Expires > int64(c.life-60):
			t.Errorf("%d s left: expires_in %d, want an access token that stops a minute before the Google token", c.life, r.Expires)
		case c.fresh && r.Expires < int64((accessTTL-time.Minute).Seconds()):
			t.Errorf("%d s left: expires_in %d, want a fresh Google token's", c.life, r.Expires)
		}
	}
}

// An active client's refresh lands when the Google token has a minute
// left: its access token stopped then. The vault would answer a forced
// refresh with consent, so the token is waited out (about a minute, here
// no time at all) and the vault asked again, plainly, which renews it:
// the refresh succeeds with a fresh hour, and nothing is forced. A
// refresh with 4 minutes left uses the token as it is, forcing nothing.
// Neither signs the user out, and neither token reaches the log.
func TestVaultRefreshAcrossGoogleExpiry(t *testing.T) {
	logs := captureLog(t)
	e := newTestEnv(t, false, nil)
	id, first := vaultSignIn(e)
	mid, midAnswer := mintFor(e, 240)
	last, lastAnswer := mintFor(e, 60)

	scriptVault(e, midAnswer)
	second := e.token(refreshForm(id, first.Refresh), "", "")
	if second.status != http.StatusOK || second.Expires > 180 || second.Expires < 170 {
		t.Fatalf("refresh with 4 minutes left: %+v, want an access token of about 3 minutes", second)
	}

	scriptVault(e, lastAnswer)
	third := e.token(refreshForm(id, second.Refresh), "", "")
	if third.status != http.StatusOK || third.Expires < int64((accessTTL-time.Minute).Seconds()) {
		t.Fatalf("refresh with a minute left: %+v, want a fresh Google token's access token", third)
	}
	e.vault.mu.Lock()
	forced, waits := e.vault.forced, e.vault.waits
	e.vault.mu.Unlock()
	if forced != 0 || len(waits) != 1 || waits[0] < 59*time.Second || waits[0] > time.Minute+expirySkew {
		t.Errorf("forced %d, waits %v; want none forced and one wait of about a minute", forced, waits)
	}
	if got := googleTokenIn(t, e, third.Access); got == last || got == mid {
		t.Error("the renewed access token carries a nearly spent Google token")
	}
	if l := logs.String(); strings.Contains(l, mid) || strings.Contains(l, last) {
		t.Error("a vault token reached the log")
	}
}

// countingReplay counts rotations and give-backs.
type countingReplay struct {
	*replayCache
	mu                 sync.Mutex
	rotates, unrotates int
}

func (c *countingReplay) rotate(fam string, gen uint64, exp time.Time, rot string) bool {
	c.mu.Lock()
	c.rotates++
	c.mu.Unlock()
	return c.replayCache.rotate(fam, gen, exp, rot)
}

func (c *countingReplay) unrotate(fam string, gen uint64, rot string) {
	c.mu.Lock()
	c.unrotates++
	c.mu.Unlock()
	c.replayCache.unrotate(fam, gen, rot)
}

func (c *countingReplay) counts() (rotates, unrotates int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rotates, c.unrotates
}

// The vault's wait runs before the refresh token's family is rotated. A
// client that gives up during it (its timeout, or a front end's, is
// shorter) leaves nothing rotated and nothing to give back, and its retry
// with the same refresh token works.
func TestVaultClientGivesUpDuringTheWait(t *testing.T) {
	logs := captureLog(t)
	e := newTestEnv(t, false, nil)
	used := &countingReplay{replayCache: e.as.used.(*replayCache)}
	e.as.used = used
	id, first := vaultSignIn(e)
	_, answer := mintFor(e, 60)
	scriptVault(e, answer)
	vc := e.as.cfg.Custody.(*VaultCustody)
	waiting := make(chan struct{})
	vc.sleeper = &sleeper{func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}}
	rotated, unrotated := used.counts()
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/token", strings.NewReader(refreshForm(id, first.Refresh).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-waiting:
	case err := <-done:
		cancel()
		t.Fatalf("the refresh answered (%v) without waiting out the nearly spent token", err)
	}
	if r, u := used.counts(); r != rotated || u != unrotated {
		t.Errorf("during the wait: %d rotations, %d give-backs; want none", r-rotated, u-unrotated)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the abandoned refresh answered")
	}
	// The server notices the client went away and ends the refresh.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), "waiting out a nearly spent token") {
		if time.Now().After(deadline) {
			t.Fatal("the server never ended the abandoned refresh")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, u := used.counts(); r != rotated || u != unrotated {
		t.Errorf("after the client went away: %d rotations, %d give-backs; want none", r-rotated, u-unrotated)
	}
	vc.sleeper = &sleeper{e.vault.sleep}
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.status != http.StatusOK {
		t.Errorf("retry after giving up during the wait: %+v, want the refresh token still usable", r)
	}
}

// Two processes of one client refresh with the same token while the
// vault's token is nearly spent. The first waits it out; the second,
// arriving during that wait, finds the family not yet rotated and
// refreshes (here the vault already has a fresh token for it). The first
// then loses the rotation race and, as any concurrent refresh with one
// token does (kode-gopher has no reuse grace), revokes the family.
func TestVaultConcurrentRefreshDuringTheWait(t *testing.T) {
	e := newTestEnv(t, false, nil)
	id, first := vaultSignIn(e)
	_, answer := mintFor(e, 60)
	scriptVault(e, answer)
	vc := e.as.cfg.Custody.(*VaultCustody)
	waiting, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	vc.sleeper = &sleeper{func(ctx context.Context, d time.Duration) error {
		close(waiting)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return e.vault.sleep(ctx, d)
	}}
	firstDone := make(chan tokenResp, 1)
	go func() { firstDone <- e.token(refreshForm(id, first.Refresh), "", "") }()
	select {
	case <-waiting:
	case r := <-firstDone:
		t.Fatalf("the first refresh answered (%+v) without waiting out the nearly spent token", r)
	}
	second := e.token(refreshForm(id, first.Refresh), "", "")
	releaseOnce.Do(func() { close(release) })
	r1 := <-firstDone
	if second.status != http.StatusOK {
		t.Fatalf("a refresh during another's wait: %+v, want it to refresh: the family is not rotated during the wait", second)
	}
	if r1.Error != "invalid_grant" {
		t.Errorf("the waiting refresh, after the other rotated: %+v, want invalid_grant", r1)
	}
	if r := e.token(refreshForm(id, second.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Errorf("the second refresh's token after the reuse: %+v, want invalid_grant (family revoked)", r)
	}
}

// A server WriteTimeout shorter than the wait doesn't lose the answer:
// the wait moves the response's write deadline past it, and the token
// endpoint moves it again before rotating. Over HTTP/1.1 a write timeout
// doesn't cancel the request, so a lost answer to a refresh that went on
// to rotate its family would make the client's retry a replay; over
// HTTP/2 it resets the stream, cancelling the wait, so every waiting
// refresh would fail. Each case really waits, about 4 seconds.
func TestVaultWaitOutlastsWriteTimeout(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		e := newTestEnv(t, false, nil)
		id, first := vaultSignIn(e)
		_, answer := mintFor(e, 1)
		scriptVault(e, answer)
		e.as.cfg.Custody.(*VaultCustody).sleeper = nil // a real wait
		ts := httptest.NewUnstartedServer(e.srv.Config.Handler)
		ts.Config.WriteTimeout = 500 * time.Millisecond
		if h2 {
			ts.EnableHTTP2 = true
			ts.StartTLS()
		} else {
			ts.Start()
		}
		t.Cleanup(ts.Close)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/token", strings.NewReader(refreshForm(id, first.Refresh).Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("HTTP/2 %v: refresh behind a 500 ms WriteTimeout: %v, want the answer", h2, err)
		}
		var r tokenResp
		_ = json.NewDecoder(resp.Body).Decode(&r)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || r.Refresh == "" || (h2 && resp.ProtoMajor != 2) {
			t.Fatalf("HTTP/2 %v: refresh behind a 500 ms WriteTimeout: %s %d %+v", h2, resp.Proto, resp.StatusCode, r)
		}
		if r2 := e.token(refreshForm(id, r.Refresh), "", ""); r2.status != http.StatusOK {
			t.Errorf("HTTP/2 %v: the answered refresh token: %+v", h2, r2)
		}
	}
}

// At sign-in only the wait's own extension stands between a WriteTimeout
// and the redirect, at the callback (a stored grant's token nearly spent)
// and at the consent return (the token after consent nearly spent). Each
// case really waits, about 4 seconds.
func TestVaultSignInWaitOutlastsWriteTimeout(t *testing.T) {
	for _, consent := range []bool{false, true} {
		e := newTestEnvServer(t, false, nil, func(s *http.Server) { s.WriteTimeout = 500 * time.Millisecond })
		if !consent {
			vaultSignIn(e) // the vault holds alice's grant
		}
		_, answer := mintFor(e, 1)
		if consent {
			scriptVault(e, "-", answer) // the callback's retrieve asks for consent
		} else {
			scriptVault(e, answer)
		}
		e.as.cfg.Custody.(*VaultCustody).sleeper = nil // a real wait
		id, v := e.register(), rand.Text()+rand.Text()
		b := newBrowser(t)
		// A fresh connection per request: the client retries a GET that
		// fails on a reused one, which would hide a lost answer.
		b.hc.Transport = &http.Transport{DisableKeepAlives: true}
		loc, page := b.visit(e.authorizeURL(id, v, nil))
		if loc == nil || loc.Query().Get("code") == "" {
			t.Fatalf("consent %v: sign-in behind a 500 ms WriteTimeout: %v %q, want a code", consent, loc, page)
		}
		if r := e.token(exchangeForm(id, loc.Query().Get("code"), v), "", ""); r.status != http.StatusOK || r.Expires < int64((accessTTL-time.Minute).Seconds()) {
			t.Errorf("consent %v: exchange %+v, want a fresh Google token's access token", consent, r)
		}
	}
}

// A refusal found in prepare is answered only after the allow-list check:
// if that check fails, the answer is server_error and the rotation is
// given back, so the retry, with the vault answering properly, works.
func TestVaultRefusalWaitsForTheAllowList(t *testing.T) {
	e := newTestEnv(t, false, nil)
	e.google.signInAs, e.vault.consentAs = bob, bob // admitted by group
	id, first := vaultSignIn(e)
	e.google.mu.Lock()
	other := e.google.mint(mallory, scopeOpenID+" "+scopeEmail+" "+scopeCloudPlatform, 3600)
	e.google.mu.Unlock()
	scriptVault(e, `{"success":{"token":"`+other+`"}}`)
	e.as.cfg.Allow.cache = nil
	e.groups.mu.Lock()
	e.groups.failNext = 1
	e.groups.mu.Unlock()
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "server_error" {
		t.Fatalf("refresh, the vault refusing and the allow-list failing: %+v, want server_error", r)
	}
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.status != http.StatusOK {
		t.Errorf("retry: %+v, want the rotation given back", r)
	}
}

// A refusal found before the rotation (here the vault holding another
// account's grant) is still answered after it: the refresh token is
// spent, and presenting it again is a replay that revokes its family.
func TestVaultRefusalAtRefreshKeepsTheTokenSpent(t *testing.T) {
	logs := captureLog(t)
	e := newTestEnv(t, false, nil)
	id, first := vaultSignIn(e)
	e.google.mu.Lock()
	other := e.google.mint(mallory, scopeOpenID+" "+scopeEmail+" "+scopeCloudPlatform, 3600)
	e.google.mu.Unlock()
	scriptVault(e, `{"success":{"token":"`+other+`"}}`)
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "invalid_grant" {
		t.Fatalf("refresh, the vault holds another account's grant: %+v, want invalid_grant", r)
	}
	// The vault answers properly again; the spent token is still a replay.
	if r := e.token(refreshForm(id, first.Refresh), "", ""); r.Error != "invalid_grant" || !strings.Contains(logs.String(), "reused refresh token") {
		t.Errorf("the refused refresh token presented again: %+v, want invalid_grant and the family revoked", r)
	}
}

// deadlineWriter records SetWriteDeadline calls, as
// http.ResponseController makes them.
type deadlineWriter struct {
	http.ResponseWriter
	set []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.set = append(d.set, t)
	return nil
}

// The wait's write-deadline extension only ever moves a server's
// WriteTimeout deadline later: it sets none where the server has none,
// and never shortens a longer one.
func TestExtendWriteDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		srv    *http.Server
		extend bool
	}{
		"no server":              {nil, false},
		"no WriteTimeout":        {&http.Server{}, false},
		"a longer WriteTimeout":  {&http.Server{WriteTimeout: 10 * time.Minute}, false},
		"a shorter WriteTimeout": {&http.Server{WriteTimeout: time.Second}, true},
		"no response in the ctx": {&http.Server{WriteTimeout: time.Second}, false},
	} {
		ctx := t.Context()
		if tc.srv != nil {
			ctx = context.WithValue(ctx, http.ServerContextKey, tc.srv)
		}
		w := &deadlineWriter{}
		if name != "no response in the ctx" {
			ctx = withResponse(ctx, w)
		}
		extendWriteDeadline(ctx, time.Minute)
		if got := len(w.set) == 1; got != tc.extend {
			t.Errorf("%s: deadlines set %v, want extended=%v", name, w.set, tc.extend)
			continue
		}
		if tc.extend && time.Until(w.set[0]) < 59*time.Second {
			t.Errorf("%s: deadline %v, want a minute from now", name, w.set[0])
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

// The wait itself moves the write deadline past the wait and
// postWaitBudget. The sign-in callback and the consent return rely on
// this alone; the token endpoint extends again before rotating, so
// TestVaultWaitOutlastsWriteTimeout passes without it.
func TestVaultWaitExtendsWriteDeadline(t *testing.T) {
	ctx := context.WithValue(t.Context(), http.ServerContextKey, &http.Server{WriteTimeout: time.Second})
	w := &deadlineWriter{}
	ctx = withResponse(ctx, w)
	v := &VaultCustody{sleeper: &sleeper{func(context.Context, time.Duration) error { return nil }}}
	if err := v.wait(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(w.set) != 1 || time.Until(w.set[0]) < time.Minute+postWaitBudget-time.Second {
		t.Errorf("deadlines set %v, want one past the wait and postWaitBudget", w.set)
	}
}

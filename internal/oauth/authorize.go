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
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// authRequest is a client's validated /authorize request. It rides
// through the consent page, Google sign-in and any vault consent inside
// sealed envelopes.
type authRequest struct {
	ClientID    string `json:"c"`
	ClientName  string `json:"cn,omitempty"`
	RedirectURI string `json:"r"`
	Challenge   string `json:"pc"`
	Resource    string `json:"aud"`
	Scope       string `json:"s"`
	State       string `json:"st,omitempty"`
}

// browserBound is an envelope tied to the browser that started the flow
// by a cookie, so a link or form from someone else's flow fails.
type browserBound struct {
	Req     authRequest `json:"q"`
	Browser string      `json:"b"`
	Exp     int64       `json:"e"`
	// Verifier is the PKCE verifier for Google sign-in (state only).
	Verifier string `json:"v,omitempty"`
}

// pendingConsent waits in a cookie while the browser is at the vault's
// consent leg.
type pendingConsent struct {
	Req     authRequest `json:"q"`
	User    User        `json:"u"`
	Consent Consent     `json:"c"`
	Exp     int64       `json:"e"`
}

const (
	cookieBrowser = "kg_auth"
	cookieConsent = "kg_consent"
)

var challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// authError is an /authorize failure. Before the redirect URI is
// trusted it's shown to the user; after, it goes back to the client.
type authError struct {
	code, desc string
}

// handleAuthorize validates the request and, for a pre-registered
// client, goes straight to Google sign-in. Any other client gets a
// consent page first: the MCP spec requires a proxy with one Google
// client to get the user's consent for each dynamically registered
// client, or a malicious one could ride a user's existing Google
// session.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.resolveClient(r.Context(), q.Get("client_id"))
	if err != nil {
		authPage(w, http.StatusBadRequest, "Unknown client", "This sign-in link names a client this server doesn't recognize: "+err.Error())
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(c.RedirectURIs) == 1 {
		redirectURI = c.RedirectURIs[0]
	}
	if !redirectAllowed(c, redirectURI) {
		authPage(w, http.StatusBadRequest, "Bad redirect", "The redirect URI isn't registered for this client.")
		return
	}
	req, aerr := s.validateAuthRequest(q, c, redirectURI)
	if aerr != nil {
		redirectError(w, r, redirectURI, q.Get("state"), s.cfg.Issuer, aerr)
		return
	}
	browser := s.bindBrowser(w, r)
	if _, ok := s.static[c.ID]; ok {
		s.toGoogle(w, r, req, browser)
		return
	}
	form, err := s.keys.seal(purposeConsent, browserBound{Req: *req, Browser: browser, Exp: time.Now().Add(authRequestTTL).Unix()})
	if err != nil {
		authPage(w, http.StatusInternalServerError, "Error", "Couldn't start sign-in.")
		return
	}
	consentPage(w, c, redirectURI, form)
}

func (s *Server) validateAuthRequest(q url.Values, c *client, redirectURI string) (*authRequest, *authError) {
	if q.Get("response_type") != "code" {
		return nil, &authError{"unsupported_response_type", "only response_type=code is supported"}
	}
	if q.Get("code_challenge_method") != "S256" || !challengeRE.MatchString(q.Get("code_challenge")) {
		return nil, &authError{"invalid_request", "PKCE with code_challenge_method=S256 is required"}
	}
	resource := q.Get("resource")
	if resource == "" {
		resource = s.resource
	}
	if resource != s.resource {
		return nil, &authError{"invalid_target", "resource must be " + s.resource}
	}
	// One scope exists. Others are dropped rather than refused, as RFC
	// 6749 §3.3 allows; the token response says what was granted.
	return &authRequest{
		ClientID:    c.ID,
		ClientName:  c.Name,
		RedirectURI: redirectURI,
		Challenge:   q.Get("code_challenge"),
		Resource:    resource,
		Scope:       ScopeExecute,
		State:       q.Get("state"),
	}, nil
}

// handleApprove is the consent page's form: Continue goes on to Google
// sign-in, Cancel returns access_denied to the client.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var b browserBound
	if !s.openBound(w, r, purposeConsent, r.PostFormValue("request"), &b) {
		return
	}
	// Renew the binding cookie: Google sign-in gets a fresh 10 minutes
	// in the state, and the cookie must last as long.
	if ck, err := r.Cookie(s.cookieName(cookieBrowser)); err == nil {
		s.setCookie(w, cookieBrowser, ck.Value)
	}
	if r.PostFormValue("action") != "approve" {
		redirectError(w, r, b.Req.RedirectURI, b.Req.State, s.cfg.Issuer, &authError{"access_denied", "the user declined"})
		return
	}
	s.toGoogle(w, r, &b.Req, b.Browser)
}

// toGoogle sends the browser to Google sign-in, carrying req in the
// sealed state.
func (s *Server) toGoogle(w http.ResponseWriter, r *http.Request, req *authRequest, browser string) {
	verifier := rand.Text() + rand.Text()
	state, err := s.keys.seal(purposeState, browserBound{Req: *req, Browser: browser, Exp: time.Now().Add(authRequestTTL).Unix(), Verifier: verifier})
	if err != nil {
		authPage(w, http.StatusInternalServerError, "Error", "Couldn't start sign-in.")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	u := s.cfg.Google.authCodeURL(s.callbackURI(), state, base64.RawURLEncoding.EncodeToString(sum[:]), s.cfg.Custody.signInScopes(), s.cfg.Custody.signInOffline())
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// handleCallback is Google's redirect after sign-in.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var b browserBound
	if !s.openBound(w, r, purposeState, q.Get("state"), &b) {
		return
	}
	// The binding cookie stays: another sign-in started in this browser
	// may still need it. It expires with the flow.
	req := &b.Req
	fail := func(code, desc string) {
		redirectError(w, r, req.RedirectURI, req.State, s.cfg.Issuer, &authError{code, desc})
	}
	if e := q.Get("error"); e != "" {
		fail("access_denied", "Google sign-in: "+e)
		return
	}
	ctx := r.Context()
	t, err := s.cfg.Google.exchange(ctx, s.google.hc, q.Get("code"), s.callbackURI(), b.Verifier)
	if err != nil {
		log.Printf("oauth: Google code exchange: %v", err)
		fail("server_error", "Google sign-in failed")
		return
	}
	u, err := s.cfg.Google.verifyIDToken(t.IDToken, time.Now())
	if err != nil {
		log.Printf("oauth: Google ID token: %v", err)
		fail("server_error", "Google sign-in failed")
		return
	}
	if err := s.cfg.Allow.admit(ctx, u); err != nil {
		log.Printf("oauth: refused %s: %v", u.Email, err)
		if errors.Is(err, errNotAllowed) {
			fail("access_denied", u.Email+" isn't allowed to use this server")
		} else {
			fail("server_error", "couldn't check the allow-list")
		}
		return
	}
	grant, consent, err := s.cfg.Custody.begin(ctx, s.google, u, t, s.continueURI())
	if err != nil {
		log.Printf("oauth: %s custody for %s: %v", s.cfg.Custody.Name(), u.Email, err)
		fail("access_denied", grantFailed)
		return
	}
	if consent != nil {
		s.toConsent(w, r, req, u, consent)
		return
	}
	s.finishAuthorize(w, r, req, u, grant)
}

// grantFailed is what the client hears when custody fails; the reason
// (which may quote Google's response) goes only to the log.
const grantFailed = "couldn't get a Google Cloud grant for this account (the server log has details)"

// toConsent parks the flow in a cookie and sends the browser to the
// vault's consent leg, which returns to /consent/continue.
func (s *Server) toConsent(w http.ResponseWriter, r *http.Request, req *authRequest, u User, c *Consent) {
	p, err := s.keys.seal(purposePending, pendingConsent{Req: *req, User: u, Consent: *c, Exp: time.Now().Add(authRequestTTL).Unix()})
	if err != nil {
		redirectError(w, r, req.RedirectURI, req.State, s.cfg.Issuer, &authError{"server_error", "couldn't start consent"})
		return
	}
	s.setCookie(w, cookieConsent, p)
	http.Redirect(w, r, c.URL, http.StatusSeeOther)
}

// handleConsentContinue is where the vault's consent leg returns.
func (s *Server) handleConsentContinue(w http.ResponseWriter, r *http.Request) {
	ck, err := r.Cookie(s.cookieName(cookieConsent))
	var p pendingConsent
	var why string
	switch {
	case err != nil:
		why = whyNoCookie
	case s.keys.open(purposePending, ck.Value, &p) != nil:
		why = whyUnreadable
	case time.Now().Unix() > p.Exp:
		why = whyExpired
	}
	if why != "" {
		signInFailed(w, purposePending, why)
		return
	}
	s.clearCookie(w, cookieConsent)
	grant, err := s.cfg.Custody.finish(r.Context(), s.google, p.User, p.Consent, r.URL.Query(), s.continueURI())
	if err != nil {
		log.Printf("oauth: %s consent for %s: %v", s.cfg.Custody.Name(), p.User.Email, err)
		redirectError(w, r, p.Req.RedirectURI, p.Req.State, s.cfg.Issuer, &authError{"access_denied", grantFailed})
		return
	}
	s.finishAuthorize(w, r, &p.Req, p.User, grant)
}

// finishAuthorize issues the authorization code and returns the browser
// to the client.
func (s *Server) finishAuthorize(w http.ResponseWriter, r *http.Request, req *authRequest, u User, g *Grant) {
	code, err := s.keys.seal(purposeCode, codePayload{
		Req: *req, User: u,
		Token: g.AccessToken, TokenExp: g.Expiry.Unix(), Secret: g.Secret,
		JTI: rand.Text(), Exp: time.Now().Add(codeTTL).Unix(),
	})
	if err != nil {
		redirectError(w, r, req.RedirectURI, req.State, s.cfg.Issuer, &authError{"server_error", "couldn't issue a code"})
		return
	}
	log.Printf("oauth: signed in %s (client %s)", u.Email, clientLabel(req))
	redirectTo(w, r, req.RedirectURI, url.Values{"code": {code}, "state": {req.State}, "iss": {s.cfg.Issuer}})
}

func clientLabel(req *authRequest) string {
	if req.ClientName != "" {
		return req.ClientName
	}
	if isSealed(req.ClientID) {
		return "dynamically registered"
	}
	return req.ClientID
}

// bindBrowser returns the hash of the browser's binding cookie, which
// the sealed flow state carries. It keeps a cookie the browser already
// has, so a second sign-in started in the same browser (a retry, a
// copied link, a prefetch) doesn't break the first, and renews its
// lifetime.
func (s *Server) bindBrowser(w http.ResponseWriter, r *http.Request) string {
	v := rand.Text()
	if ck, err := r.Cookie(s.cookieName(cookieBrowser)); err == nil && browserCookieRE.MatchString(ck.Value) {
		v = ck.Value
	}
	s.setCookie(w, cookieBrowser, v)
	return hashCookie(v)
}

// browserCookieRE matches what rand.Text makes.
var browserCookieRE = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// Why a flow can't continue in this browser. Each is shown to the user
// and logged.
const (
	whyUnreadable = "The sign-in link is damaged, or was issued by a different kode-gopher server."
	whyExpired    = "This sign-in was started more than 10 minutes ago."
	whyNoCookie   = "This browser has no record of starting this sign-in. It was probably started in another browser or browser profile (signing in to a different Google account can move you to another Chrome profile), or this browser blocks cookies for this site."
	whyOtherFlow  = "This browser started a different sign-in. This one was started in another browser or browser profile, or after this browser's sign-in record expired."
)

// openBound opens a browser-bound envelope and checks it belongs to
// this browser. If not, it shows why and returns false.
func (s *Server) openBound(w http.ResponseWriter, r *http.Request, purpose, sealed string, b *browserBound) bool {
	why := ""
	ck, ckErr := r.Cookie(s.cookieName(cookieBrowser))
	switch {
	case s.keys.open(purpose, sealed, b) != nil:
		why = whyUnreadable
	case time.Now().Unix() > b.Exp:
		why = whyExpired
	case ckErr != nil:
		why = whyNoCookie
	case subtle.ConstantTimeCompare([]byte(hashCookie(ck.Value)), []byte(b.Browser)) != 1:
		why = whyOtherFlow
	default:
		return true
	}
	signInFailed(w, purpose, why)
	return false
}

// signInFailed logs why at step (a fixed name, not the request's path)
// and shows it.
func signInFailed(w http.ResponseWriter, step, why string) {
	log.Printf("oauth: sign-in stopped at %s: %s", step, why)
	authPage(w, http.StatusBadRequest, "Sign-in can't continue", why+" Start again from your MCP client.")
}

func hashCookie(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.cfg.Issuer, "https://") }

// cookieName adds the __Host- prefix on https. Browsers only accept
// such a cookie from this exact host over https, so a network attacker
// (over http) or a sibling domain can't plant a binding cookie whose
// value they know, which bindBrowser would otherwise reuse.
func (s *Server) cookieName(name string) string {
	if s.secureCookies() {
		return "__Host-" + name
	}
	return name
}

// setCookie sets a flow cookie. SameSite=Lax: it must ride the
// top-level redirects back from Google.
func (s *Server) setCookie(w http.ResponseWriter, name, value string) {
	//nolint:gosec // Secure whenever the issuer is https; http only on loopback for testing
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(name), Value: value, Path: "/",
		MaxAge:   int(authRequestTTL.Seconds()),
		HttpOnly: true, Secure: s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	//nolint:gosec // as setCookie
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(name), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, issuer string, e *authError) {
	v := url.Values{"error": {e.code}, "error_description": {e.desc}, "iss": {issuer}}
	if state != "" {
		v.Set("state", state)
	}
	redirectTo(w, r, redirectURI, v)
}

func redirectTo(w http.ResponseWriter, r *http.Request, base string, params url.Values) {
	u, err := url.Parse(base)
	if err != nil {
		authPage(w, http.StatusBadRequest, "Bad redirect", "The redirect URI is malformed.")
		return
	}
	q := u.Query()
	for k, vs := range params {
		if len(vs) > 0 && vs[0] != "" {
			q.Set(k, vs[0])
		}
	}
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // base is a redirect URI checked against the client's registration
}

func pageHeaders(w http.ResponseWriter, code int) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>kode-gopher: {{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:34em;margin:4em auto;padding:0 1em;line-height:1.5}
.box{border:1px solid #ccc;border-radius:8px;padding:1em 1.5em}button{font-size:1em;padding:.5em 1.2em;margin-right:.5em}
code{background:#f3f3f3;padding:0 .3em}</style></head>
<body><div class="box"><h2>{{.Title}}</h2>
{{if .Form}}<p><b>{{.ClientName}}</b> wants to sign you in to kode-gopher.</p>
<p>It will be able to <b>run Go code in a sandbox as you</b>, with your Google Cloud access.</p>
<p>After sign-in you'll be sent back to <code>{{.RedirectHost}}</code>.</p>
<p>Only continue if you started this from your own MCP client.</p>
<form method="post" action="authorize"><input type="hidden" name="request" value="{{.Form}}">
<button name="action" value="approve">Continue with Google</button><button name="action" value="deny">Cancel</button></form>
{{else}}<p>{{.Message}}</p>{{end}}</div></body></html>
`))

func authPage(w http.ResponseWriter, code int, title, msg string) {
	pageHeaders(w, code)
	_ = pageTmpl.Execute(w, map[string]string{"Title": title, "Message": msg})
}

func consentPage(w http.ResponseWriter, c *client, redirectURI, form string) {
	name := c.Name
	if name == "" {
		name = "An unnamed MCP client"
	}
	if strings.HasPrefix(c.ID, "https://") {
		name += " (" + c.ID + ")"
	}
	host := redirectURI
	if u, err := url.Parse(redirectURI); err == nil && u.Host != "" {
		host = u.Scheme + "://" + u.Host
	}
	pageHeaders(w, http.StatusOK)
	_ = pageTmpl.Execute(w, map[string]string{"Title": "Sign in", "Form": form, "ClientName": name, "RedirectHost": host})
}

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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

// codePayload is a sealed authorization code. It carries the Google
// access token already minted at sign-in, so /token needn't call Google.
type codePayload struct {
	Req      authRequest `json:"q"`
	User     User        `json:"u"`
	Token    string      `json:"t"`
	TokenExp int64       `json:"te"`
	Secret   string      `json:"g,omitempty"`
	JTI      string      `json:"j"`
	Exp      int64       `json:"e"`
}

// accessPayload is a sealed kode-gopher access token.
type accessPayload struct {
	Sub      string `json:"sub"`
	Email    string `json:"em"`
	ClientID string `json:"c"`
	Aud      string `json:"aud"`
	Scope    string `json:"s"`
	Token    string `json:"t"`
	TokenExp int64  `json:"te"`
	Exp      int64  `json:"e"`
	// Mode is modeService for a client-credentials token, else empty.
	Mode string `json:"m,omitempty"`
}

// refreshPayload is a sealed kode-gopher refresh token. Secret is the
// Google refresh token under sealed custody, empty under the vault.
type refreshPayload struct {
	User     User   `json:"u"`
	ClientID string `json:"c"`
	Aud      string `json:"aud"`
	Scope    string `json:"s"`
	Secret   string `json:"g,omitempty"`
	JTI      string `json:"j"`
	Exp      int64  `json:"e"`
}

// tokenError is an RFC 6749 §5.2 error response.
type tokenError struct {
	Code string `json:"error"`
	Desc string `json:"error_description,omitempty"`
	// basic: the client tried HTTP Basic, so a 401 names that scheme.
	basic bool
}

func (e *tokenError) Error() string { return e.Code + ": " + e.Desc }

func invalidGrant(desc string) *tokenError { return &tokenError{Code: "invalid_grant", Desc: desc} }

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, &tokenError{Code: "invalid_request", Desc: "malformed form body"})
		return
	}
	c, terr := s.authenticateClient(r)
	if terr != nil {
		writeTokenError(w, terr)
		return
	}
	var resp map[string]any
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		resp, terr = s.exchangeCode(r, c)
	case "refresh_token":
		resp, terr = s.refreshTokens(r.Context(), r, c)
	case "client_credentials":
		resp, terr = s.clientCredentials(r, c)
	default:
		terr = &tokenError{Code: "unsupported_grant_type", Desc: "authorization_code, refresh_token and client_credentials only"}
	}
	if terr != nil {
		writeTokenError(w, terr)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// authenticateClient resolves the client and, for a confidential one,
// checks its secret (client_secret_basic or client_secret_post).
func (s *Server) authenticateClient(r *http.Request) (*client, *tokenError) {
	id, secret, basic := r.BasicAuth()
	if basic {
		// RFC 6749 §2.3.1: Basic credentials are form-urlencoded.
		id, secret = formUnescape(id), formUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == "" {
		return nil, &tokenError{Code: "invalid_client", Desc: "client_id is required", basic: basic}
	}
	c, err := s.resolveClient(r.Context(), id)
	if err != nil {
		return nil, &tokenError{Code: "invalid_client", Desc: err.Error(), basic: basic}
	}
	if c.Secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(c.Secret)) != 1 {
		return nil, &tokenError{Code: "invalid_client", Desc: "client authentication failed", basic: basic}
	}
	return c, nil
}

func formUnescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

func (s *Server) exchangeCode(r *http.Request, c *client) (map[string]any, *tokenError) {
	f := r.PostForm
	var p codePayload
	if s.keys.open(purposeCode, f.Get("code"), &p) != nil {
		return nil, invalidGrant("invalid authorization code")
	}
	now := time.Now()
	switch {
	case now.Unix() > p.Exp:
		return nil, invalidGrant("authorization code expired")
	case p.Req.ClientID != c.ID:
		return nil, invalidGrant("authorization code was issued to another client")
	case f.Get("redirect_uri") != "" && f.Get("redirect_uri") != p.Req.RedirectURI:
		return nil, invalidGrant("redirect_uri doesn't match the authorization request")
	case !pkceMatches(f.Get("code_verifier"), p.Req.Challenge):
		return nil, invalidGrant("PKCE verification failed")
	case f.Get("resource") != "" && f.Get("resource") != p.Req.Resource:
		return nil, &tokenError{Code: "invalid_target", Desc: "resource doesn't match the authorization request"}
	case !s.used.use("code:"+p.JTI, time.Unix(p.Exp, 0)):
		return nil, invalidGrant("authorization code already used")
	}
	g := &Grant{AccessToken: p.Token, Expiry: time.Unix(p.TokenExp, 0), Secret: p.Secret}
	return s.issueTokens(p.User, c.ID, p.Req.Resource, p.Req.Scope, g)
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// refreshTokens rotates a refresh token: the old one is spent, the
// allow-list is checked again, and custody mints a fresh Google token.
func (s *Server) refreshTokens(ctx context.Context, r *http.Request, c *client) (map[string]any, *tokenError) {
	f := r.PostForm
	var p refreshPayload
	if s.keys.open(purposeRefresh, f.Get("refresh_token"), &p) != nil {
		return nil, invalidGrant("invalid refresh token")
	}
	switch {
	case time.Now().Unix() > p.Exp:
		return nil, invalidGrant("refresh token expired")
	case p.ClientID != c.ID:
		return nil, invalidGrant("refresh token was issued to another client")
	case f.Get("resource") != "" && f.Get("resource") != p.Aud:
		return nil, &tokenError{Code: "invalid_target", Desc: "resource doesn't match the original grant"}
	case !s.used.use("refresh:"+p.JTI, time.Unix(p.Exp, 0)):
		log.Printf("oauth: reused refresh token for %s (client %s)", p.User.Email, p.ClientID)
		return nil, invalidGrant("refresh token already used")
	}
	// The token is claimed above, so two concurrent refreshes can't both
	// use it. A transient failure below (server_error) gives the claim
	// back, or the client's retry would be refused as a replay and its
	// user signed out; a final refusal (invalid_grant) keeps it spent.
	if err := s.cfg.Allow.admit(ctx, p.User); err != nil {
		log.Printf("oauth: refresh refused for %s: %v", p.User.Email, err)
		if errors.Is(err, errNotAllowed) {
			return nil, invalidGrant(err.Error())
		}
		s.used.release("refresh:" + p.JTI)
		return nil, &tokenError{Code: "server_error", Desc: "couldn't check the allow-list"}
	}
	g, err := s.cfg.Custody.refresh(ctx, s.google, p.User, p.Secret, s.continueURI())
	if errors.Is(err, errReauth) {
		return nil, invalidGrant(err.Error())
	}
	if err != nil {
		log.Printf("oauth: %s refresh for %s: %v", s.cfg.Custody.Name(), p.User.Email, err)
		s.used.release("refresh:" + p.JTI)
		return nil, &tokenError{Code: "server_error", Desc: "couldn't refresh the Google grant"}
	}
	return s.issueTokens(p.User, c.ID, p.Aud, p.Scope, g)
}

// issueTokens seals a new access and refresh token pair. The access
// token never outlives the Google token inside it.
func (s *Server) issueTokens(u User, clientID, aud, scope string, g *Grant) (map[string]any, *tokenError) {
	now := time.Now()
	at, exp, terr := s.sealAccess(accessPayload{
		Sub: u.Sub, Email: u.Email, ClientID: clientID, Aud: aud, Scope: scope,
		Token: g.AccessToken, TokenExp: g.Expiry.Unix(),
	}, now)
	if terr != nil {
		return nil, terr
	}
	rt, err := s.keys.seal(purposeRefresh, refreshPayload{
		User: u, ClientID: clientID, Aud: aud, Scope: scope, Secret: g.Secret,
		JTI: rand.Text(), Exp: now.Add(refreshTTL).Unix(),
	})
	if err != nil {
		return nil, &tokenError{Code: "server_error", Desc: "couldn't seal the refresh token"}
	}
	return map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"expires_in":    int64(exp.Sub(now).Seconds()),
		"refresh_token": rt,
		"scope":         scope,
	}, nil
}

// sealAccess sets p's expiry, accessTTL from now but never past the
// Google token inside it, and seals it.
func (s *Server) sealAccess(p accessPayload, now time.Time) (string, time.Time, *tokenError) {
	exp := now.Add(accessTTL)
	if limit := time.Unix(p.TokenExp, 0).Add(-time.Minute); limit.Before(exp) {
		exp = limit
	}
	if !exp.After(now) {
		return "", time.Time{}, &tokenError{Code: "server_error", Desc: "the Google access token is already spent"}
	}
	p.Exp = exp.Unix()
	at, err := s.keys.seal(purposeAccess, p)
	if err != nil {
		return "", time.Time{}, &tokenError{Code: "server_error", Desc: "couldn't seal the access token"}
	}
	return at, exp, nil
}

func writeTokenError(w http.ResponseWriter, e *tokenError) {
	code := http.StatusBadRequest
	if e.Code == "invalid_client" {
		code = http.StatusUnauthorized
		if e.basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="kode-gopher"`)
		}
	}
	if e.Code == "server_error" {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, e)
}

// handleRegister is open Dynamic Client Registration (RFC 7591) for
// public clients. The client_id it returns is the sealed registration,
// so nothing is stored.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var m struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || json.Unmarshal(b, &m) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata", "error_description": "malformed JSON"})
		return
	}
	if desc := checkRegistration(m.RedirectURIs, m.GrantTypes, m.ResponseTypes); desc != "" {
		code := "invalid_client_metadata"
		if strings.HasPrefix(desc, "redirect") {
			code = "invalid_redirect_uri"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
		return
	}
	if len(m.ClientName) > 100 {
		m.ClientName = m.ClientName[:100]
	}
	now := time.Now()
	id, err := s.keys.seal(purposeClient, dcrClient{RedirectURIs: m.RedirectURIs, Name: m.ClientName, IssuedAt: now.Unix()})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	// Every DCR client is public: token_endpoint_auth_method is none,
	// whatever was asked for (RFC 7591 §3.2.1 lets us say so).
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        now.Unix(),
		"client_name":                m.ClientName,
		"redirect_uris":              m.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

func checkRegistration(redirects, grants, responses []string) string {
	if len(redirects) == 0 || len(redirects) > 10 {
		return "redirect_uris needs 1 to 10 entries"
	}
	for _, u := range redirects {
		if !validRedirectURI(u) {
			return fmt.Sprintf("redirect URI %q isn't allowed (https, loopback http, or a private-use scheme)", u)
		}
	}
	for _, g := range grants {
		if g != "authorization_code" && g != "refresh_token" {
			return fmt.Sprintf("grant type %q isn't supported", g)
		}
	}
	for _, rt := range responses {
		if rt != "code" {
			return fmt.Sprintf("response type %q isn't supported", rt)
		}
	}
	return ""
}

// Verifier checks kode-gopher access tokens at the MCP endpoint. The
// TokenInfo's UserID is the Google sub, or "client:<id>" for a
// client-credentials token (sessions bind to it), and its Extra carries
// the request's creds.OAuthUser.
func (s *Server) Verifier() auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		var p accessPayload
		if s.keys.open(purposeAccess, token, &p) != nil {
			return nil, auth.ErrInvalidToken
		}
		exp := time.Unix(p.Exp, 0)
		if time.Now().After(exp) || p.Aud != s.resource {
			return nil, auth.ErrInvalidToken
		}
		service := p.Mode == modeService
		if service && !s.serviceClientCurrent(&p) {
			return nil, auth.ErrInvalidToken
		}
		project := s.cfg.Project
		if service && project == "" {
			project = creds.ServiceAccountProject(p.Email)
		}
		return &auth.TokenInfo{
			Scopes:     strings.Fields(p.Scope),
			Expiration: exp,
			UserID:     p.Sub,
			Extra: map[string]any{creds.TokenInfoKey: &creds.OAuthUser{
				Token:          p.Token,
				Expiry:         time.Unix(p.TokenExp, 0),
				Email:          p.Email,
				Project:        project,
				QuotaProject:   s.cfg.QuotaProject,
				ServiceAccount: service,
			}},
		}, nil
	}
}

// Protect guards the MCP endpoint: a missing or bad token gets 401 and a
// pointer to the resource metadata; a token without ScopeExecute gets
// 403 insufficient_scope, which starts the client's step-up flow.
func (s *Server) Protect(h http.Handler) http.Handler {
	verify := s.Verifier()
	inner := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.ResourceMetadataURL(),
		Scopes:              []string{ScopeExecute},
	})(h)
	challenge := fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q, resource_metadata=%q`, ScopeExecute, s.ResourceMetadataURL())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The SDK's own 403 challenge lacks error="insufficient_scope",
		// and clients only step up when it's there, so answer that case
		// here.
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if ti, err := verify(r.Context(), strings.TrimSpace(tok), r); err == nil && !slices.Contains(ti.Scopes, ScopeExecute) {
				w.Header().Set("WWW-Authenticate", challenge)
				http.Error(w, "insufficient scope", http.StatusForbidden)
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
}

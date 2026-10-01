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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Google's canonical scope names. The vault stores grants under these
// exact spellings ("email" doesn't match), so they're used everywhere.
const (
	scopeOpenID        = "openid"
	scopeEmail         = "https://www.googleapis.com/auth/userinfo.email"
	scopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
)

// GoogleClient is kode-gopher's own confidential OAuth client at Google,
// and Google's endpoints. Zero endpoint fields take Google's defaults;
// tests point them at fakes.
type GoogleClient struct {
	ClientID     string
	ClientSecret string

	AuthURL      string
	TokenURL     string
	TokenInfoURL string
}

func (g *GoogleClient) applyDefaults() {
	if g.AuthURL == "" {
		g.AuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	}
	if g.TokenURL == "" {
		g.TokenURL = "https://oauth2.googleapis.com/token"
	}
	if g.TokenInfoURL == "" {
		g.TokenInfoURL = "https://oauth2.googleapis.com/tokeninfo"
	}
}

// LoadGoogleClient reads the client ID and secret from the JSON file the
// Cloud console downloads for an OAuth client ({"web": {...}}).
func LoadGoogleClient(path string) (GoogleClient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return GoogleClient{}, fmt.Errorf("read Google client: %w", err)
	}
	var f struct {
		Web, Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return GoogleClient{}, fmt.Errorf("parse Google client %s: %w", path, err)
	}
	c := f.Web
	if c == nil {
		c = f.Installed
	}
	if c == nil || c.ClientID == "" || c.ClientSecret == "" {
		return GoogleClient{}, fmt.Errorf("google client %s has no web client_id and client_secret", path)
	}
	return GoogleClient{ClientID: c.ClientID, ClientSecret: c.ClientSecret}, nil
}

// googleTokens is Google's token endpoint response.
type googleTokens struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`

	expiry time.Time
}

// User is a signed-in Google user, from a verified ID token.
type User struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	// Domain is the ID token's hd claim: the user's Workspace domain,
	// empty for consumer accounts.
	Domain string `json:"hd,omitempty"`
}

// authCodeURL is Google's sign-in URL for one /authorize.
func (g *GoogleClient) authCodeURL(redirectURI, state, challenge string, scopes []string, offline bool) string {
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if offline {
		// prompt=consent: Google returns a refresh token only on consent.
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	} else {
		q.Set("prompt", "select_account")
	}
	return g.AuthURL + "?" + q.Encode()
}

// exchange trades a sign-in code for Google's tokens.
func (g *GoogleClient) exchange(ctx context.Context, hc *http.Client, code, redirectURI, verifier string) (*googleTokens, error) {
	return g.tokenRequest(ctx, hc, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

// refresh mints a fresh Google access token from a refresh token.
func (g *GoogleClient) refresh(ctx context.Context, hc *http.Client, refreshToken string) (*googleTokens, error) {
	return g.tokenRequest(ctx, hc, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// errGoogleGrant is Google refusing a code or refresh token
// (invalid_grant): the grant is gone, and the user must sign in again.
var errGoogleGrant = errors.New("google refused the grant")

func (g *GoogleClient) tokenRequest(ctx context.Context, hc *http.Client, form url.Values) (*googleTokens, error) {
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("google token endpoint: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "invalid_grant" {
			return nil, errGoogleGrant
		}
		return nil, fmt.Errorf("google token endpoint: HTTP %d %s", resp.StatusCode, e.Error)
	}
	var t googleTokens
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("google token endpoint: %w", err)
	}
	if t.AccessToken == "" || t.ExpiresIn <= 0 {
		return nil, errors.New("google token endpoint: no access token")
	}
	t.expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return &t, nil
}

// verifyIDToken checks the ID token from Google's token response. It
// came straight from Google's token endpoint over TLS, so per OIDC Core
// §3.1.3.7 TLS stands in for the signature check; the claims are still
// checked.
func (g *GoogleClient) verifyIDToken(raw string, now time.Time) (User, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return User{}, errors.New("malformed ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return User{}, errors.New("malformed ID token")
	}
	var c struct {
		Iss           string          `json:"iss"`
		Aud           json.RawMessage `json:"aud"`
		Sub           string          `json:"sub"`
		Email         string          `json:"email"`
		EmailVerified bool            `json:"email_verified"`
		HD            string          `json:"hd"`
		Exp           int64           `json:"exp"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return User{}, errors.New("malformed ID token")
	}
	var aud []string
	if json.Unmarshal(c.Aud, &aud) != nil {
		var one string
		_ = json.Unmarshal(c.Aud, &one)
		aud = []string{one}
	}
	switch {
	case c.Iss != "https://accounts.google.com" && c.Iss != "accounts.google.com":
		return User{}, fmt.Errorf("ID token issuer %q isn't Google", c.Iss)
	case !slices.Contains(aud, g.ClientID):
		return User{}, errors.New("ID token isn't for this client")
	case now.Unix() >= c.Exp:
		return User{}, errors.New("ID token expired")
	case c.Sub == "":
		return User{}, errors.New("ID token has no subject")
	case c.Email == "" || !c.EmailVerified:
		return User{}, errors.New("ID token has no verified email")
	}
	return User{Sub: c.Sub, Email: c.Email, Domain: c.HD}, nil
}

// tokenInfo is what Google's tokeninfo endpoint says about an access
// token.
type tokenInfo struct {
	Sub    string
	Email  string
	Scopes []string
	Expiry time.Time
}

// tokenInfo asks Google whose an access token is and when it really
// expires (the vault's expireTime can be wrong).
func (g *GoogleClient) tokenInfo(ctx context.Context, hc *http.Client, accessToken string) (*tokenInfo, error) {
	form := url.Values{"access_token": {accessToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.TokenInfoURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google tokeninfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google tokeninfo: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Sub       string `json:"sub"`
		Email     string `json:"email"`
		Scope     string `json:"scope"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("google tokeninfo: %w", err)
	}
	secs, err := strconv.ParseInt(r.ExpiresIn, 10, 64)
	if err != nil || r.Sub == "" {
		return nil, errors.New("google tokeninfo: incomplete response")
	}
	return &tokenInfo{
		Sub:    r.Sub,
		Email:  r.Email,
		Scopes: strings.Fields(r.Scope),
		Expiry: time.Now().Add(time.Duration(secs) * time.Second),
	}, nil
}

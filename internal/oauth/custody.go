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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// minGoogleTokenLife is the least life a Google access token should
// have left to be sealed into a kode-gopher access token. A vault token
// closer to expiry than this is force-refreshed, and used as it is only
// if the vault doesn't bring a better one (see minLiveTokenLife).
const minGoogleTokenLife = 5 * time.Minute

// minLiveTokenLife is the least life a nearly spent vault token must
// have left to be used when the vault won't refresh it: an access token
// stops a minute before the Google token inside it, a sign-in's code may
// wait codeTTL to be exchanged, and a minute's margin.
const minLiveTokenLife = time.Minute + codeTTL + time.Minute

// Grant is a Google access token for a user, plus what the custody
// backend needs to mint the next one: the Google refresh token for
// sealed custody, nothing for the vault (Google holds the grant).
type Grant struct {
	AccessToken string
	Expiry      time.Time
	Secret      string
}

// Consent is a detour the browser takes between sign-in and the end of
// /authorize: the vault's own Google consent leg.
type Consent struct {
	URL   string `json:"-"`
	Nonce string `json:"n"`
	UID   string `json:"u,omitempty"`
}

// Custody holds users' Google grants and mints access tokens from them
// (docs/design-in-cluster.md, "Token storage" and "Alternative custody").
// Implementations: *VaultCustody (the default) and SealedCustody.
type Custody interface {
	// Name is "vault" or "sealed".
	Name() string
	// signInScopes and signInOffline shape the Google sign-in request.
	signInScopes() []string
	signInOffline() bool
	// begin runs at /callback once the user has signed in. It returns
	// either a grant, or a consent detour that comes back to
	// continueURI and then finish.
	begin(ctx context.Context, g *google, u User, signIn *googleTokens, continueURI string) (*Grant, *Consent, error)
	finish(ctx context.Context, g *google, u User, c Consent, q url.Values, continueURI string) (*Grant, error)
	// refresh mints a fresh grant at /token. errReauth means the user
	// must sign in again.
	refresh(ctx context.Context, g *google, u User, secret, continueURI string) (*Grant, error)
}

// google is Google's endpoints plus the client for calling them.
type google struct {
	client *GoogleClient
	hc     *http.Client
}

// errReauth is a grant that can't be renewed without the user.
var errReauth = errors.New("the Google grant is gone; sign in again")

// reauthError is a specific reason the user must sign in again; it
// matches errReauth, so a refresh answers invalid_grant (sign in) rather
// than server_error (retry).
type reauthError string

func (e reauthError) Error() string        { return string(e) }
func (e reauthError) Is(target error) bool { return target == errReauth }

// SealedCustody is the fallback: the Google refresh token travels inside
// kode-gopher's own sealed refresh token, held by the MCP client.
type SealedCustody struct{}

// Name implements Custody.
func (SealedCustody) Name() string { return "sealed" }

func (SealedCustody) signInScopes() []string {
	return []string{scopeOpenID, scopeEmail, scopeCloudPlatform}
}

func (SealedCustody) signInOffline() bool { return true }

func (SealedCustody) begin(_ context.Context, _ *google, _ User, t *googleTokens, _ string) (*Grant, *Consent, error) {
	if !slices.Contains(strings.Fields(t.Scope), scopeCloudPlatform) {
		return nil, nil, reauthError("the Google Cloud scope wasn't granted at consent")
	}
	if t.RefreshToken == "" {
		return nil, nil, reauthError("google returned no refresh token")
	}
	return &Grant{AccessToken: t.AccessToken, Expiry: t.expiry, Secret: t.RefreshToken}, nil, nil
}

func (SealedCustody) finish(context.Context, *google, User, Consent, url.Values, string) (*Grant, error) {
	return nil, errors.New("sealed custody has no consent detour")
}

func (SealedCustody) refresh(ctx context.Context, g *google, u User, secret, _ string) (*Grant, error) {
	if secret == "" {
		return nil, errReauth
	}
	t, err := g.client.refresh(ctx, g.hc, secret)
	if errors.Is(err, errGoogleGrant) {
		return nil, errReauth
	}
	if err != nil {
		return nil, err
	}
	// Google includes a fresh ID token when it can. If the account's
	// email or domain has changed since sign-in, the identity (and the
	// allow-list decision) sealed in the refresh token is stale.
	if t.IDToken != "" {
		if now, err := g.client.verifyIDToken(t.IDToken, time.Now()); err == nil && (now.Sub != u.Sub || now.Email != u.Email || now.Domain != u.Domain) {
			return nil, reauthError("the Google account's identity changed since sign-in")
		}
	}
	next := secret
	if t.RefreshToken != "" {
		next = t.RefreshToken
	}
	return &Grant{AccessToken: t.AccessToken, Expiry: t.expiry, Secret: next}, nil
}

// VaultCustody keeps users' Google grants in the Agent Identity
// credential vault (agentidentitycredentials.googleapis.com). The vault
// trusts whatever user ID it's given, and stores whichever account
// consented, so every token it returns is checked against the
// signed-in user's sub before use. The vault's expireTime is ignored in
// favour of tokeninfo's, and a token with under 5 minutes left, or one
// tokeninfo refuses, is force-refreshed once.
type VaultCustody struct {
	// AuthProvider is the full resource name,
	// projects/<p>/locations/<l>/authProviders/<name>.
	AuthProvider string
	// Client calls the vault as kode-gopher's own identity (Workload
	// Identity, with roles/agentidentity.user).
	Client *http.Client
	// Endpoint defaults to https://agentidentitycredentials.googleapis.com/v1/.
	Endpoint string
}

// Name implements Custody.
func (*VaultCustody) Name() string { return "vault" }

// The vault runs its own consent leg for cloud-platform, so sign-in
// only needs to identify the user.
func (*VaultCustody) signInScopes() []string { return []string{scopeOpenID, scopeEmail} }

func (*VaultCustody) signInOffline() bool { return false }

func (v *VaultCustody) begin(ctx context.Context, g *google, u User, _ *googleTokens, continueURI string) (*Grant, *Consent, error) {
	r, err := v.retrieve(ctx, u.Sub, continueURI, "")
	if err != nil {
		return nil, nil, err
	}
	c := consentOf(r)
	if c == nil {
		// A consent request from grant comes from forcing a refresh of a
		// token Google refused: the user is in the browser, so they're
		// sent to it as well.
		var gr *Grant
		if gr, c, err = v.grant(ctx, g, u, r, continueURI); err != nil || c == nil {
			return gr, nil, err
		}
	}
	if c.URL == "" || c.Nonce == "" {
		return nil, nil, errors.New("vault: incomplete consent request")
	}
	return nil, c, nil
}

// consentOf is the consent leg the vault asks for in r, or nil.
func consentOf(r *vaultRetrieveResponse) *Consent {
	c := r.UriConsentRequired
	if c == nil {
		return nil
	}
	return &Consent{URL: c.AuthorizationURI, Nonce: c.ConsentNonce, UID: c.UID}
}

func (v *VaultCustody) finish(ctx context.Context, g *google, u User, c Consent, q url.Values, continueURI string) (*Grant, error) {
	if uid := q.Get("uuid"); c.UID != "" && uid != "" && uid != c.UID {
		return nil, errors.New("vault: consent callback is for another flow")
	}
	state := q.Get("user_id_validation_state")
	if state == "" {
		return nil, errors.New("vault: consent callback has no user_id_validation_state")
	}
	body := map[string]string{"userId": u.Sub, "consentNonce": c.Nonce, "userIdValidationState": state}
	if err := v.call(ctx, "finalize", body, nil); err != nil {
		return nil, err
	}
	r, err := v.retrieve(ctx, u.Sub, continueURI, "")
	if err != nil {
		return nil, err
	}
	if r.UriConsentRequired != nil {
		return nil, errors.New("vault: still asks for consent after finalize")
	}
	gr, again, err := v.grant(ctx, g, u, r, continueURI)
	if again != nil {
		// Consent just ran, and the grant it stored is already gone.
		return nil, errReauth
	}
	return gr, err
}

func (v *VaultCustody) refresh(ctx context.Context, g *google, u User, _, continueURI string) (*Grant, error) {
	r, err := v.retrieve(ctx, u.Sub, continueURI, "")
	if err != nil {
		return nil, err
	}
	if r.UriConsentRequired != nil {
		return nil, errReauth
	}
	gr, c, err := v.grant(ctx, g, u, r, continueURI)
	if c != nil {
		return nil, errReauth
	}
	return gr, err
}

// grant turns a retrieve result into a Grant. It checks the token is
// the signed-in user's and carries cloud-platform, takes the expiry from
// tokeninfo (the vault's expireTime can be wrong), and force-refreshes
// once a token that's nearly spent or that Google refuses.
//
// Only the vault or Google refusing is a reauthError (access_denied at
// sign-in, invalid_grant at refresh, which signs the user out). Per the
// vault's API reference, "pending" means "retry the RetrieveCredentials
// request after some time": the vault busy, like an answer with nothing
// usable in it, is a plain error, server_error, and a retry may work.
//
// A forced refresh "fetches a new token or initiates a new consent flow".
// For a token Google refuses, a consent request is the grant gone: it's
// returned, for a sign-in to detour through and a refresh to refuse. For
// a token Google still accepts it says nothing about the grant: the vault
// asks for consent rather than force-refresh a valid token (the phase B
// spike, docs/design-in-cluster.md). So when a forced refresh of a valid
// token brings no better one, short of a refusal, that token is used if
// it still has minLiveTokenLife left.
func (v *VaultCustody) grant(ctx context.Context, g *google, u User, r *vaultRetrieveResponse, continueURI string) (*Grant, *Consent, error) {
	var (
		forced string // the token whose refresh was forced, if any
		live   *Grant // that token, if Google still accepted it
	)
	fallback := func(err error) (*Grant, *Consent, error) {
		if live != nil && time.Until(live.Expiry) >= minLiveTokenLife {
			return live, nil, nil
		}
		return nil, nil, err
	}
	for {
		switch {
		case r.UriConsentRequired != nil && live != nil:
			return fallback(errors.New("vault: would not force-refresh a nearly spent token that is still valid; try again"))
		case r.UriConsentRequired != nil:
			return nil, consentOf(r), nil
		case r.ConsentRejected != nil:
			return nil, nil, reauthError("vault: the user declined consent")
		case r.Pending != nil:
			return fallback(errors.New("vault: the grant is pending; try again"))
		case r.Success == nil || r.Success.Token == "":
			return fallback(errors.New("vault: answered with neither a token nor a consent request"))
		case forced != "" && r.Success.Token == forced:
			return fallback(errors.New("vault: a forced refresh returned the same token"))
		}
		tok := r.Success.Token
		ti, err := g.client.tokenInfo(ctx, g.hc, tok)
		switch {
		case errors.Is(err, errGoogleToken) && forced == "":
			// Expired early (the vault's expireTime can be wrong) or
			// revoked: force a new one.
		case errors.Is(err, errGoogleToken) && live == nil:
			// Refused before the forced refresh and after it.
			return nil, nil, reauthError("vault: Google refuses the force-refreshed token")
		case err != nil:
			return fallback(err)
		case ti.Sub != u.Sub:
			// Someone other than the signed-in user consented. The vault
			// has no delete API, so this only refuses.
			return nil, nil, reauthError("vault: the stored Google grant belongs to a different account than the one signed in")
		case !slices.Contains(ti.Scopes, scopeCloudPlatform):
			return nil, nil, reauthError("vault: token lacks the Google Cloud scope")
		case ti.Email != "" && !strings.EqualFold(ti.Email, u.Email):
			// Same account, new address: the identity (and the allow-list
			// decision) sealed at sign-in is stale. tokeninfo reports the
			// email because retrieve always asks for userinfo.email.
			return nil, nil, reauthError("vault: the Google account's email changed since sign-in")
		case time.Until(ti.Expiry) >= minGoogleTokenLife:
			return &Grant{AccessToken: tok, Expiry: ti.Expiry}, nil, nil
		case forced == "":
			live = &Grant{AccessToken: tok, Expiry: ti.Expiry}
		}
		if forced != "" {
			// The forced token is nearly spent too.
			return fallback(errors.New("vault: force-refreshed token is still nearly expired"))
		}
		forced = tok
		if r, err = v.retrieve(ctx, u.Sub, continueURI, tok); err != nil {
			return fallback(err)
		}
	}
}

type vaultRetrieveResponse struct {
	UriConsentRequired *struct {
		AuthorizationURI string `json:"authorizationUri"`
		ConsentNonce     string `json:"consentNonce"`
		UID              string `json:"uid"`
	} `json:"uriConsentRequired"`
	Success *struct {
		Token string `json:"token"`
	} `json:"success"`
	ConsentRejected *struct{} `json:"consentRejected"`
	Pending         *struct{} `json:"pending"`
}

// retrieve asks the vault for userID's token. forceRefresh, if set, is
// the spent token to replace.
func (v *VaultCustody) retrieve(ctx context.Context, userID, continueURI, forceRefresh string) (*vaultRetrieveResponse, error) {
	body := map[string]any{
		"userId":      userID,
		"scopes":      []string{scopeOpenID, scopeEmail, scopeCloudPlatform},
		"continueUri": continueURI,
	}
	if forceRefresh != "" {
		body["forceRefreshToken"] = forceRefresh
	}
	var r vaultRetrieveResponse
	if err := v.call(ctx, "retrieve", body, &r); err != nil {
		if forceRefresh != "" && strings.Contains(err.Error(), forceRefresh) {
			// The vault's error message quotes the token back: keep it
			// out of the error, which is logged.
			return nil, errors.New(strings.ReplaceAll(err.Error(), forceRefresh, "[token]"))
		}
		return nil, err
	}
	return &r, nil
}

func (v *VaultCustody) call(ctx context.Context, method string, body, out any) error {
	ep := v.Endpoint
	if ep == "" {
		ep = "https://agentidentitycredentials.googleapis.com/v1/"
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep+v.AuthProvider+"/credentials:"+method, bytes.NewReader(b)) //nolint:gosec // operator-configured vault endpoint
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.Client.Do(req) //nolint:gosec // as above
	if err != nil {
		return fmt.Errorf("vault %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("vault %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rb, &e)
		return fmt.Errorf("vault %s: HTTP %d %s %s", method, resp.StatusCode, e.Error.Status, e.Error.Message)
	}
	if out == nil || len(bytes.TrimSpace(rb)) == 0 {
		return nil
	}
	if err := json.Unmarshal(rb, out); err != nil {
		return fmt.Errorf("vault %s: %w", method, err)
	}
	return nil
}

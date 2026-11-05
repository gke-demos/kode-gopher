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

	"github.com/gke-demos/kode-gopher/internal/creds"
)

// The vault renews a Google token only once it has expired or Google
// refuses it: its API reference says to set forceRefreshToken "only if
// the previous token was expired or invalid", and asked to force one
// that was still valid it asked for consent instead (the phase B spike,
// docs/design-in-cluster.md). So a token Google still accepts is used for
// as long as it lasts, and one too close to expiry to use is waited out,
// then retrieved again.

// accessMargin is sealAccess's: an access token stops a minute before
// the Google token inside it expires.
const accessMargin = time.Minute

// minAccessLife is the shortest access token worth issuing.
const minAccessLife = 15 * time.Second

// minRefreshTokenLife is the least life a vault token needs at refresh:
// an access token of at least minAccessLife, a minute before it expires.
const minRefreshTokenLife = accessMargin + minAccessLife

// exchangeAllowance is how long after sign-in an authorization code
// carrying a vault token can be exchanged for an access token of at
// least minAccessLife. Clients exchange within a second or two; one that
// waits longer (codeTTL allows a minute) may get server_error at the
// exchange and start again, as it would at the redirect for a token held
// back.
const exchangeAllowance = 15 * time.Second

// minSignInTokenLife is the least life a vault token needs at sign-in.
const minSignInTokenLife = minRefreshTokenLife + exchangeAllowance

// maxExpiryWait is the longest a token too close to expiry to use is
// waited out, or need if that's longer (at sign-in, minSignInTokenLife:
// 93 seconds with expirySkew). It covers minRefreshTokenLife, so a
// refresh doesn't fail just because the token is nearly spent: an active
// client's refresh lands when the Google token has a minute left (its
// access token stopped then), and waiting that minute is the only way to
// a fresh token that doesn't need the user. (It still fails if the
// vault's next token is short too.)
const maxExpiryWait = minRefreshTokenLife

// expirySkew is how long past tokeninfo's expiry a waited-out token is
// retrieved again: tokeninfo rounds expires_in down to the second.
const expirySkew = 3 * time.Second

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
	readOnly() bool
	// signInScopes and signInOffline shape the Google sign-in request.
	signInScopes() []string
	signInOffline() bool
	// begin runs at /callback once the user has signed in. It returns
	// either a grant, or a consent detour that comes back to
	// continueURI and then finish.
	begin(ctx context.Context, g *google, u User, signIn *googleTokens, continueURI string) (*Grant, *Consent, error)
	finish(ctx context.Context, g *google, u User, c Consent, q url.Values, continueURI string) (*Grant, error)
	// prepare runs at /token before the refresh token's family is
	// rotated, so custody work that can block for long (the vault waiting
	// out a nearly spent token) never holds the family rotated. It
	// returns the grant to issue, or nil for refresh to mint one once the
	// family is rotated. errReauth means the user must sign in again (the
	// family is still rotated first, and stays so); any other error is a
	// failure, answered before anything is rotated.
	prepare(ctx context.Context, g *google, u User, secret, continueURI string) (*Grant, error)
	// refresh mints a fresh grant at /token, after the rotation, when
	// prepare returned none. errReauth means the user must sign in again.
	refresh(ctx context.Context, g *google, u User, secret, continueURI string) (*Grant, error)
}

// google is Google's endpoints plus the client for calling them.
type google struct {
	client *GoogleClient
	hc     *http.Client
}

// errWaitedConsent is the vault asking for consent to renew a token that
// expired while kode-gopher waited it out: not yet the grant gone (see
// grant).
var errWaitedConsent = errors.New("vault: asked for consent to renew a token that expired while it was waited out; try again")

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
type SealedCustody struct {
	// ReadOnly requests creds.ReadOnlyScopes instead of cloud-platform.
	ReadOnly bool
}

// Name implements Custody.
func (SealedCustody) Name() string { return "sealed" }

func (c SealedCustody) readOnly() bool { return c.ReadOnly }

func (c SealedCustody) signInScopes() []string {
	if c.ReadOnly {
		return googleUserScopes(true)
	}
	return googleUserScopes(false)
}

func googleUserScopes(readOnly bool) []string {
	if readOnly {
		return append([]string{scopeOpenID, scopeEmail}, creds.ReadOnlyScopes...)
	}
	return []string{scopeOpenID, scopeEmail, scopeCloudPlatform}
}

func isAllowedReadOnlyGoogleScope(sc string) bool {
	switch sc {
	case scopeOpenID, scopeEmail, "email", "profile", "https://www.googleapis.com/auth/userinfo.profile":
		return true
	}
	return slices.Contains(creds.ReadOnlyScopes, sc)
}

// checkReadOnlyGoogleScopes enforces an allowlist on Google scopes in
// read-only mode so neither a tampered browser consent URL nor a
// pre-existing service-specific write grant (e.g. compute, bigquery,
// devstorage.full_control) can slip past a simple cloud-platform check.
func checkReadOnlyGoogleScopes(scopes []string, where string) error {
	if !slices.Contains(scopes, creds.CloudPlatformReadOnlyScope) {
		return reauthError(where + ": the read-only Google Cloud scope wasn't granted")
	}
	for _, sc := range scopes {
		if !isAllowedReadOnlyGoogleScope(sc) {
			return reauthError(fmt.Sprintf("%s: scope %q isn't permitted on a read-only server", where, sc))
		}
	}
	return nil
}

func (SealedCustody) signInOffline() bool { return true }

func (c SealedCustody) begin(_ context.Context, _ *google, _ User, t *googleTokens, _ string) (*Grant, *Consent, error) {
	granted := strings.Fields(t.Scope)
	if c.ReadOnly {
		if err := checkReadOnlyGoogleScopes(granted, "consent"); err != nil {
			return nil, nil, err
		}
	} else if !slices.Contains(granted, scopeCloudPlatform) {
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

// prepare returns no grant: the Google refresh is quick, and runs after
// the rotation, so a replayed refresh token is refused before Google is
// asked (and a refresh token Google rotates is never lost to a refresh
// that is then refused).
func (SealedCustody) prepare(context.Context, *google, User, string, string) (*Grant, error) {
	return nil, nil
}

func (c SealedCustody) refresh(ctx context.Context, g *google, u User, secret, _ string) (*Grant, error) {
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
	if c.ReadOnly && t.Scope != "" {
		if err := checkReadOnlyGoogleScopes(strings.Fields(t.Scope), "refreshed Google token"); err != nil {
			return nil, err
		}
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
// favour of tokeninfo's. A token tokeninfo refuses is force-refreshed
// once; one Google still accepts never is, and one too close to expiry to
// use is waited out (up to 78 seconds at refresh and 93 at sign-in, plus
// the vault and tokeninfo calls, during the token request, the sign-in
// callback or the consent return) and retrieved again. At refresh the
// wait runs before the refresh token's family is rotated (prepare), so it
// never holds the family rotated. It moves the server's WriteTimeout
// deadline past itself, unless the ResponseWriter is wrapped without
// Unwrap. Give the front end a timeout of at least 2 minutes on those
// paths; a shorter one turns that request into a retry.
type VaultCustody struct {
	// AuthProvider is the full resource name,
	// projects/<p>/locations/<l>/authProviders/<name>.
	AuthProvider string
	// Client calls the vault as kode-gopher's own identity (Workload
	// Identity, with roles/agentidentity.user).
	Client *http.Client
	// Endpoint defaults to https://agentidentitycredentials.googleapis.com/v1/.
	Endpoint string
	// ReadOnly requests creds.ReadOnlyScopes instead of cloud-platform.
	ReadOnly bool

	// sleeper replaces waiting out a nearly spent token (tests). A
	// pointer, so VaultCustody stays comparable.
	sleeper *sleeper
}

// sleeper waits out a nearly spent vault token in tests.
type sleeper struct {
	sleep func(ctx context.Context, d time.Duration) error
}

// Name implements Custody.
func (*VaultCustody) Name() string { return "vault" }

func (v *VaultCustody) readOnly() bool { return v.ReadOnly }

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
		// token Google refused, or retrieving one again once it expired:
		// the user is in the browser, so they're sent to it as well.
		var gr *Grant
		if gr, c, err = v.grant(ctx, g, u, r, continueURI, minSignInTokenLife); (err != nil && !errors.Is(err, errWaitedConsent)) || c == nil {
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
	gr, again, err := v.grant(ctx, g, u, r, continueURI, minSignInTokenLife)
	if err != nil {
		return nil, err
	}
	if again != nil {
		// Consent just ran, and the grant it stored is already gone.
		return nil, errReauth
	}
	return gr, nil
}

// prepare does all of a refresh's vault work, waiting out a nearly spent
// token included, before the family is rotated; the grant it returns,
// with every check (account, scope, email, life) done, is the one issued.
func (v *VaultCustody) prepare(ctx context.Context, g *google, u User, secret, continueURI string) (*Grant, error) {
	return v.refresh(ctx, g, u, secret, continueURI)
}

func (v *VaultCustody) refresh(ctx context.Context, g *google, u User, _, continueURI string) (*Grant, error) {
	r, err := v.retrieve(ctx, u.Sub, continueURI, "")
	if err != nil {
		return nil, err
	}
	if r.UriConsentRequired != nil {
		return nil, errReauth
	}
	gr, c, err := v.grant(ctx, g, u, r, continueURI, minRefreshTokenLife)
	if err != nil {
		return nil, err
	}
	if c != nil {
		return nil, errReauth
	}
	return gr, nil
}

// grant turns a retrieve result into a Grant. It checks the token is
// the signed-in user's and carries cloud-platform, and takes the expiry
// from tokeninfo (the vault's expireTime can be wrong). need is the least
// life the token must have left to be used: minSignInTokenLife or
// minRefreshTokenLife.
//
// Only the vault or Google refusing is a reauthError (access_denied at
// sign-in, invalid_grant at refresh, which signs the user out). Per the
// vault's API reference, "pending" means "retry the RetrieveCredentials
// request after some time": the vault busy, like an answer with nothing
// usable in it, is a plain error, server_error, and a retry may work.
//
// A refresh is forced only for a token Google's tokeninfo refuses (400
// invalid_token: expired, on time or early, or revoked), the one case the
// vault's API reference allows: "Set this field only if the previous
// token was expired or invalid". A forced refresh "fetches a new token or
// initiates a new consent flow", so a consent request then is the grant
// gone: it's returned, for a sign-in to detour through and a refresh to
// refuse, once a plain retrieve hasn't brought another valid token (some
// other caller may have forced the same expired token first).
//
// A token Google still accepts is never forced: asked to, the vault asks
// for consent instead (the phase B spike, docs/design-in-cluster.md). One
// with need left is used, and the access token stops a minute before it
// (sealAccess). One with less is waited out if it expires within need,
// or maxExpiryWait if longer, and the vault asked again: after a real
// expiry a plain retrieve brought a fresh token in the spike, and if the
// vault hands back the expired one, Google refuses it and it's forced.
// Consent then (errWaitedConsent) isn't yet read as the grant gone: the
// vault may lag tokeninfo by a few seconds in counting the token expired.
// A refresh gets server_error, and the next request, which doesn't wait,
// forces the token again and reads consent as the grant gone; a sign-in,
// the user being in the browser, detours through consent as for any
// other. A short token waited out once, or one a forced refresh brought,
// is server_error.
func (v *VaultCustody) grant(ctx context.Context, g *google, u User, r *vaultRetrieveResponse, continueURI string, need time.Duration) (*Grant, *Consent, error) {
	var (
		forced    string // the token whose refresh was forced, if any
		waited    string // the token too short to use that was waited out
		rechecked bool   // consent to a forced refresh was checked again
	)
	for {
		if r.UriConsentRequired != nil && forced != "" && !rechecked {
			rechecked = true
			again, err := v.recheck(ctx, g, u, continueURI, forced, need)
			if err != nil {
				return nil, nil, err
			}
			if again != nil {
				r = again
				continue
			}
		}
		switch {
		case r.UriConsentRequired != nil && forced != "" && forced == waited:
			return nil, consentOf(r), errWaitedConsent
		case r.UriConsentRequired != nil:
			return nil, consentOf(r), nil
		case r.ConsentRejected != nil:
			return nil, nil, reauthError("vault: the user declined consent")
		case r.Pending != nil:
			return nil, nil, errors.New("vault: the grant is pending; try again")
		case r.Success == nil || r.Success.Token == "":
			return nil, nil, errors.New("vault: answered with neither a token nor a consent request")
		case forced != "" && r.Success.Token == forced:
			return nil, nil, errors.New("vault: a forced refresh returned the same token")
		}
		tok := r.Success.Token
		ti, err := g.client.tokenInfo(ctx, g.hc, tok)
		switch {
		case errors.Is(err, errGoogleToken) && forced == "":
			// Expired (on time, early, or while it was waited out) or
			// revoked: force a new one.
			forced = tok
			if r, err = v.retrieve(ctx, u.Sub, continueURI, tok); err != nil {
				return nil, nil, err
			}
			continue
		case errors.Is(err, errGoogleToken):
			// Refused before the forced refresh and after it.
			return nil, nil, reauthError("vault: Google refuses the force-refreshed token")
		case err != nil:
			return nil, nil, err
		}
		if err := checkVaultToken(ti, u, v.ReadOnly); err != nil {
			return nil, nil, err
		}
		left := time.Until(ti.Expiry)
		if left >= need {
			return &Grant{AccessToken: tok, Expiry: ti.Expiry}, nil, nil
		}
		if waited != "" || forced != "" || left > max(need, maxExpiryWait) {
			// A fresh token is due only once this one expires, and
			// forcing it would ask for consent.
			return nil, nil, fmt.Errorf("vault: the Google token has %ds left, too little to use, and the vault renews it only once it expires; try again", int(left.Seconds()))
		}
		waited = tok
		if err := v.wait(ctx, left+expirySkew); err != nil {
			return nil, nil, fmt.Errorf("vault: waiting out a nearly spent token: %w", err)
		}
		if r, err = v.retrieve(ctx, u.Sub, continueURI, ""); err != nil {
			return nil, nil, err
		}
	}
}

// recheck asks the vault once more, plainly, before consent to a forced
// refresh of forced is a verdict: another caller (another client of the
// same user, forcing the same expired token) may just have renewed it,
// and the vault then refuses a stale force. It returns the answer to
// carry on with if it brings a token other than the forced one that
// Google accepts with need left, which then goes on to the full checks
// (account, scope, email); nil, leaving the verdict as it was, for
// anything else. Failing to ask is failing.
func (v *VaultCustody) recheck(ctx context.Context, g *google, u User, continueURI, forced string, need time.Duration) (*vaultRetrieveResponse, error) {
	again, err := v.retrieve(ctx, u.Sub, continueURI, "")
	if err != nil {
		return nil, err
	}
	if again.Success == nil || again.Success.Token == "" || again.Success.Token == forced {
		return nil, nil
	}
	ti, err := g.client.tokenInfo(ctx, g.hc, again.Success.Token)
	if err != nil && !errors.Is(err, errGoogleToken) {
		return nil, err
	}
	if err == nil && time.Until(ti.Expiry) >= need {
		return again, nil
	}
	return nil, nil
}

// checkVaultToken refuses a vault token that isn't u's to use.
func checkVaultToken(ti *tokenInfo, u User, readOnly bool) error {
	if ti.Sub != u.Sub {
		// Someone other than the signed-in user consented. The vault
		// has no delete API, so this only refuses.
		return reauthError("vault: the stored Google grant belongs to a different account than the one signed in")
	}
	if readOnly {
		if err := checkReadOnlyGoogleScopes(ti.Scopes, "vault"); err != nil {
			return err
		}
	} else if !slices.Contains(ti.Scopes, scopeCloudPlatform) {
		return reauthError("vault: token lacks the Google Cloud scope")
	}
	if ti.Email != "" && !strings.EqualFold(ti.Email, u.Email) {
		// Same account, new address: the identity (and the allow-list
		// decision) sealed at sign-in is stale. tokeninfo reports the
		// email because retrieve always asks for userinfo.email.
		return reauthError("vault: the Google account's email changed since sign-in")
	}
	return nil
}

// postWaitBudget is how long a request may still take after waiting out
// a nearly spent token: a vault retrieve or two, tokeninfo, the rotation,
// the allow-list and the response.
const postWaitBudget = 30 * time.Second

// wait sleeps for d, or until ctx is done. It first moves the response's
// write deadline past the wait (see extendWriteDeadline), so a server
// WriteTimeout shorter than the wait doesn't drop the answer to a
// request that went on to rotate a refresh token's family.
func (v *VaultCustody) wait(ctx context.Context, d time.Duration) error {
	extendWriteDeadline(ctx, d+postWaitBudget)
	if v.sleeper != nil {
		return v.sleeper.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// responseKey carries the response whose write deadline a long custody
// wait extends.
type responseKey struct{}

type responseDeadline struct {
	w     http.ResponseWriter
	limit time.Time // the server's own deadline (WriteTimeout from the start); zero if none
}

// withResponse lets work under ctx extend w's write deadline. It records
// the deadline the server set from its WriteTimeout, so an extension
// never shortens it, and none is set where the server has none. Call it
// first thing in the handler, as near the server's deadline as it gets.
func withResponse(ctx context.Context, w http.ResponseWriter) context.Context {
	rd := responseDeadline{w: w}
	if srv, ok := ctx.Value(http.ServerContextKey).(*http.Server); ok && srv.WriteTimeout > 0 {
		rd.limit = time.Now().Add(srv.WriteTimeout)
	}
	return context.WithValue(ctx, responseKey{}, rd)
}

// extendWriteDeadline moves the write deadline of the response in ctx,
// if any, to at least d from now. http.Server.WriteTimeout is a deadline
// set when the request is read, and over HTTP/1.1 it doesn't cancel the
// request: the handler runs on and its answer is lost, which after a
// refresh token's family was rotated makes the client's retry a replay.
// Without a WriteTimeout there's no deadline to move (kode-gopher's own
// server sets none). A writer that doesn't support it (a wrapper without
// Unwrap) is left as it is.
func extendWriteDeadline(ctx context.Context, d time.Duration) {
	rd, ok := ctx.Value(responseKey{}).(responseDeadline)
	if !ok || rd.limit.IsZero() {
		return
	}
	if t := time.Now().Add(d); t.After(rd.limit) {
		_ = http.NewResponseController(rd.w).SetWriteDeadline(t)
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
		"scopes":      googleUserScopes(v.ReadOnly),
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

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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

const (
	testGoogleID     = "kg-test.apps.googleusercontent.com"
	testGoogleSecret = "google-secret"
)

var (
	alice   = User{Sub: "111", Email: "alice@example.com", Domain: "example.com"}
	bob     = User{Sub: "222", Email: "bob@other.org", Domain: "other.org"}
	mallory = User{Sub: "333", Email: "mallory@gmail.com"}
)

// fakeGoogle is Google's sign-in, token and tokeninfo endpoints.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	signInAs User                 // who signs in at /auth next
	codes    map[string]fakeCode  // sign-in codes
	tokens   map[string]fakeToken // access tokens, for tokeninfo
	refresh  map[string]User      // refresh tokens
	revoked  map[string]bool      // revoked users, by sub
	lastAuth url.Values
}

type fakeCode struct {
	user      User
	challenge string
	redirect  string
	scope     string
	offline   bool
}

type fakeToken struct {
	user  User
	scope string
	life  int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{t: t, signInAs: alice, codes: map[string]fakeCode{}, tokens: map[string]fakeToken{}, refresh: map[string]User{}, revoked: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", g.auth)
	mux.HandleFunc("POST /token", g.token)
	mux.HandleFunc("POST /tokeninfo", g.tokeninfo)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) client() GoogleClient {
	return GoogleClient{ClientID: testGoogleID, ClientSecret: testGoogleSecret,
		AuthURL: g.srv.URL + "/auth", TokenURL: g.srv.URL + "/token", TokenInfoURL: g.srv.URL + "/tokeninfo"}
}

func (g *fakeGoogle) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastAuth = q
	if q.Get("client_id") != testGoogleID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad sign-in request", http.StatusBadRequest)
		return
	}
	code := "gcode-" + rand.Text()
	g.codes[code] = fakeCode{user: g.signInAs, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"),
		scope: q.Get("scope"), offline: q.Get("access_type") == "offline"}
	http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
}

// mint issues an access token for u (call with g.mu held).
func (g *fakeGoogle) mint(u User, scope string, life int) string {
	tok := "ya29.fake-" + rand.Text()
	g.tokens[tok] = fakeToken{user: u, scope: scope, life: life}
	return tok
}

func (g *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.PostForm
	g.mu.Lock()
	defer g.mu.Unlock()
	if f.Get("client_id") != testGoogleID || f.Get("client_secret") != testGoogleSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		c, ok := g.codes[f.Get("code")]
		delete(g.codes, f.Get("code"))
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if !ok || c.redirect != f.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		scope := c.scope
		resp := map[string]any{"access_token": g.mint(c.user, scope, 3600), "expires_in": 3599, "scope": scope,
			"id_token": idToken(c.user, testGoogleID)}
		if c.offline {
			rt := "1//fake-" + rand.Text()
			g.refresh[rt] = c.user
			resp["refresh_token"] = rt
		}
		writeJSON(w, http.StatusOK, resp)
	case "refresh_token":
		u, ok := g.refresh[f.Get("refresh_token")]
		if !ok || g.revoked[u.Sub] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		scope := scopeOpenID + " " + scopeEmail + " " + scopeCloudPlatform
		writeJSON(w, http.StatusOK, map[string]any{"access_token": g.mint(u, scope, 3600), "expires_in": 3599, "scope": scope})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (g *fakeGoogle) tokeninfo(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	g.mu.Lock()
	t, ok := g.tokens[r.PostForm.Get("access_token")]
	g.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"sub": t.user.Sub, "email": t.user.Email, "scope": t.scope, "expires_in": fmt.Sprint(t.life)})
}

// idToken is an unsigned ID token; kode-gopher trusts TLS to Google's
// token endpoint instead of the signature.
func idToken(u User, aud string) string {
	claims := map[string]any{"iss": "https://accounts.google.com", "aud": aud, "sub": u.Sub, "email": u.Email,
		"email_verified": true, "exp": 4102444800}
	if u.Domain != "" {
		claims["hd"] = u.Domain
	}
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(b) + ".sig"
}

// fakeVault is the Agent Identity credential vault, with its consent
// leg standing in for Google consent.
type fakeVault struct {
	g   *fakeGoogle
	srv *httptest.Server

	mu        sync.Mutex
	consentAs User              // the account that consents at /consent
	pending   map[string]User   // consent nonce -> consenting account
	states    map[string]string // validation state -> nonce
	grants    map[string]User   // userId -> account whose grant is stored
	shortLife bool              // retrieve hands out nearly spent tokens
	forced    int
}

func newFakeVault(t *testing.T, g *fakeGoogle) *fakeVault {
	v := &fakeVault{g: g, consentAs: alice, pending: map[string]User{}, states: map[string]string{}, grants: map[string]User{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/", v.api)
	mux.HandleFunc("GET /consent", v.consent)
	v.srv = httptest.NewServer(mux)
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fakeVault) custody() *VaultCustody {
	return &VaultCustody{AuthProvider: "projects/p/locations/l/authProviders/kg", Client: v.srv.Client(), Endpoint: v.srv.URL + "/v1/"}
}

func (v *fakeVault) api(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID                string   `json:"userId"`
		Scopes                []string `json:"scopes"`
		ContinueURI           string   `json:"continueUri"`
		ForceRefreshToken     string   `json:"forceRefreshToken"`
		ConsentNonce          string   `json:"consentNonce"`
		UserIDValidationState string   `json:"userIdValidationState"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/authProviders/kg/credentials:retrieve"):
		if strings.Join(body.Scopes, " ") != scopeOpenID+" "+scopeEmail+" "+scopeCloudPlatform {
			http.Error(w, "scopes don't match the stored grant", http.StatusBadRequest)
			return
		}
		if u, ok := v.grants[body.UserID]; ok {
			life := 3600
			if v.shortLife && body.ForceRefreshToken == "" {
				life = 60
			}
			if body.ForceRefreshToken != "" {
				v.forced++
			}
			v.g.mu.Lock()
			tok := v.g.mint(u, strings.Join(body.Scopes, " "), life)
			v.g.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"success": map[string]any{"token": tok, "expireTime": "2099-01-01T00:00:00Z"}})
			return
		}
		nonce, uid := rand.Text(), rand.Text()
		v.pending[nonce] = User{}
		u := v.srv.URL + "/consent?" + url.Values{"nonce": {nonce}, "uid": {uid}, "continue": {body.ContinueURI}}.Encode()
		writeJSON(w, http.StatusOK, map[string]any{"uriConsentRequired": map[string]string{"authorizationUri": u, "consentNonce": nonce, "uid": uid}})
	case strings.HasSuffix(r.URL.Path, "/authProviders/kg/credentials:finalize"):
		if v.states[body.UserIDValidationState] != body.ConsentNonce {
			http.Error(w, "bad finalize", http.StatusBadRequest)
			return
		}
		// Like the real vault: whoever consented, stored under the
		// user ID it's given.
		v.grants[body.UserID] = v.pending[body.ConsentNonce]
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (v *fakeVault) consent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v.mu.Lock()
	state := rand.Text()
	v.pending[q.Get("nonce")] = v.consentAs
	v.states[state] = q.Get("nonce")
	v.mu.Unlock()
	http.Redirect(w, r, q.Get("continue")+"?"+url.Values{"user_id_validation_state": {state}, "uuid": {q.Get("uid")}, "auth_provider_name": {"kg"}}.Encode(), http.StatusFound)
}

// fakeGroups is a GroupChecker over a fixed membership table.
type fakeGroups struct {
	mu       sync.Mutex
	members  map[string]map[string]bool
	calls    int
	failNext int // fail this many calls (a directory outage)
}

func (f *fakeGroups) IsMember(_ context.Context, group, member string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failNext > 0 {
		f.failNext--
		return false, errors.New("directory unavailable")
	}
	return f.members[group][member], nil
}

// testEnv is a kode-gopher authorization server in front of a tiny MCP
// server whose one tool reports the caller's credentials.
type testEnv struct {
	t      *testing.T
	google *fakeGoogle
	vault  *fakeVault
	groups *fakeGroups
	as     *Server
	srv    *httptest.Server
}

func newTestEnv(t *testing.T, sealed bool, mutate func(*Config)) *testEnv {
	t.Helper()
	e := &testEnv{t: t, google: newFakeGoogle(t), groups: &fakeGroups{members: map[string]map[string]bool{
		"kg-users@example.com": {"bob@other.org": true},
	}}}
	e.vault = newFakeVault(t, e.google)
	mux := http.NewServeMux()
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	keys, err := NewKeyring(KeyringKey{ID: "k1", Secret: []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	var custody Custody = e.vault.custody()
	if sealed {
		custody = SealedCustody{}
	}
	cfg := Config{
		Issuer:           e.srv.URL,
		Google:           e.google.client(),
		Custody:          custody,
		Allow:            &AllowList{Domains: []string{"Example.com"}, Groups: []string{"kg-users@example.com"}, GroupChecker: e.groups},
		Keys:             keys,
		OpenRegistration: true,
		Project:          "proj-1",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	e.as, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "0"}, nil)
	sdk.AddTool(mcpSrv, &sdk.Tool{Name: "whoami"}, func(ctx context.Context, req *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
		src, _ := req.Extra.TokenInfo.Extra[creds.TokenInfoKey].(*creds.OAuthUser)
		tok, err := src.AccessToken(ctx)
		if err != nil {
			return nil, nil, err
		}
		text := fmt.Sprintf("%s %s %s %s", req.Extra.TokenInfo.UserID, tok.Email, tok.Project, tok.Token)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}}, nil, nil
	})
	mux.Handle("/mcp", e.as.Protect(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcpSrv }, nil)))
	mux.Handle("/", e.as.Handler())
	return e
}

// browser follows redirects like a user's browser, approving
// kode-gopher's consent page, until it reaches the client's redirect
// URI (127.0.0.1:1, never dialed).
type browser struct {
	t       *testing.T
	hc      *http.Client
	deny    bool
	sawPage bool
}

const clientRedirect = "http://127.0.0.1:1/callback"

var hiddenRequestRE = regexp.MustCompile(`name="request" value="([^"]+)"`)

func newBrowser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t}
	b.hc = &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == "127.0.0.1:1" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return b
}

// visit opens u and returns the final redirect to the client, or the
// page it stopped on.
func (b *browser) visit(u string) (*url.URL, string) {
	resp, err := b.hc.Get(u)
	if err != nil {
		b.t.Fatalf("browser GET: %v", err)
	}
	for {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther {
			loc, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				b.t.Fatal(err)
			}
			return loc, ""
		}
		m := hiddenRequestRE.FindSubmatch(body)
		if m == nil {
			return nil, string(body)
		}
		b.sawPage = true
		action := "approve"
		if b.deny {
			action = "deny"
		}
		resp, err = b.hc.PostForm(resp.Request.URL.String(), url.Values{"request": {html.UnescapeString(string(m[1]))}, "action": {action}})
		if err != nil {
			b.t.Fatalf("browser POST: %v", err)
		}
	}
}

// fetcher is the go-sdk client's AuthorizationCodeFetcher.
func (b *browser) fetcher(_ context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	loc, page := b.visit(args.URL)
	if loc == nil {
		return nil, fmt.Errorf("sign-in stopped on a page: %s", page)
	}
	q := loc.Query()
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("authorization error %s: %s", e, q.Get("error_description"))
	}
	return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state")}, nil
}

// connect runs the go-sdk client's whole sign-in against env and calls
// whoami.
func (e *testEnv) connect(b *browser, cfg *auth.AuthorizationCodeHandlerConfig) (string, error) {
	cfg.RedirectURL = clientRedirect
	cfg.AuthorizationCodeFetcher = b.fetcher
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	c := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: e.srv.URL + "/mcp", OAuthHandler: h, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "whoami"})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("whoami: %v", res.Content[0].(*sdk.TextContent).Text)
	}
	return res.Content[0].(*sdk.TextContent).Text, nil
}

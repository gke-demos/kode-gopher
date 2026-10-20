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

// Package oauth is kode-gopher's OAuth 2.1 authorization server, fronting
// Google sign-in (docs/design-in-cluster.md, section 3). MCP clients
// register (pre-registered, DCR or CIMD), sign in through /authorize,
// and get kode-gopher's own sealed tokens; each access token carries
// the user's Google access token, which a tool call hands to the
// sandbox's metadata emulator. Nothing is stored server-side except a
// replay cache for single-use codes and refresh-token families.
package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// ScopeExecute is required for kode-gopher's tools.
const ScopeExecute = "kode-gopher:execute"

// Lifetimes.
const (
	authRequestTTL = 10 * time.Minute // consent page to callback
	codeTTL        = time.Minute
	accessTTL      = 15 * time.Minute
	refreshTTL     = 30 * 24 * time.Hour // sliding: every refresh renews it
)

// Envelope purposes, bound into each seal.
const (
	purposeClient  = "client"
	purposeConsent = "consent"
	purposeState   = "state"
	purposePending = "pending"
	purposeCode    = "code"
	purposeAccess  = "access"
	purposeRefresh = "refresh"
)

// Config is one deployment's authorization server. Nothing about the
// Google side is hard-coded.
type Config struct {
	// Issuer is the server's public base URL, e.g. https://kg.example.com
	// (no trailing slash). The MCP endpoint is Issuer+"/mcp", and that's
	// the resource every token is bound to.
	Issuer string
	// Google is kode-gopher's own OAuth client at Google.
	Google GoogleClient
	// Custody holds users' Google grants.
	Custody Custody
	// Allow admits users. Required.
	Allow *AllowList
	// Keys seals every token, code and DCR client ID.
	Keys *Keyring
	// Clients are pre-registered. They skip kode-gopher's consent page.
	Clients []StaticClient
	// ServiceTokens mints tokens for client-credentials clients' service
	// accounts. Required if any client has a ServiceAccount.
	ServiceTokens ServiceTokens
	// OpenRegistration accepts DCR and CIMD clients. Off: only Clients.
	OpenRegistration bool
	// Project and QuotaProject are reported to snippets as
	// GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_QUOTA_PROJECT.
	Project      string
	QuotaProject string
	// HTTPClient calls Google's token and tokeninfo endpoints. Default:
	// a client with a 30 s timeout.
	HTTPClient *http.Client
	// CIMDClient fetches Client ID Metadata Documents. Default:
	// SSRFSafeClient. Tests on loopback override it.
	CIMDClient *http.Client
}

// Server is the authorization server. Mount Handler at the root of
// Issuer, and wrap the MCP endpoint with Protect.
type Server struct {
	cfg      Config
	resource string
	keys     *Keyring
	google   *google
	static   map[string]StaticClient
	// serviceClients: some client may use the client_credentials grant.
	serviceClients bool
	cimd           *cimdFetcher
	used           *replayCache
}

// New validates cfg and builds a Server.
func New(cfg Config) (*Server, error) {
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	switch {
	case !strings.HasPrefix(cfg.Issuer, "https://") && !strings.HasPrefix(cfg.Issuer, "http://localhost") && !strings.HasPrefix(cfg.Issuer, "http://127.0.0.1"):
		return nil, fmt.Errorf("oauth: issuer %q must be https (or http on loopback, for testing)", cfg.Issuer)
	case cfg.Google.ClientID == "" || cfg.Google.ClientSecret == "":
		return nil, errors.New("oauth: Google client ID and secret are required")
	case cfg.Custody == nil:
		return nil, errors.New("oauth: a custody backend is required")
	case cfg.Allow == nil || len(cfg.Allow.Domains)+len(cfg.Allow.Groups) == 0:
		return nil, errors.New("oauth: the allow-list is empty, so no one could sign in")
	case len(cfg.Allow.Groups) > 0 && cfg.Allow.GroupChecker == nil:
		return nil, errors.New("oauth: group allow-list needs a group checker")
	case cfg.Keys == nil:
		return nil, errors.New("oauth: a sealing keyring is required")
	}
	if err := checkGoogleEndpoints(cfg); err != nil {
		return nil, err
	}
	for i, d := range cfg.Allow.Domains {
		cfg.Allow.Domains[i] = strings.ToLower(d)
	}
	cfg.Google.applyDefaults()
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	s := &Server{
		cfg:      cfg,
		resource: cfg.Issuer + "/mcp",
		keys:     cfg.Keys,
		google:   &google{client: &cfg.Google, hc: cfg.HTTPClient},
		static:   map[string]StaticClient{},
		cimd:     newCIMDFetcher(cfg.CIMDClient),
		used:     &replayCache{seen: map[string]time.Time{}},
	}
	for _, c := range cfg.Clients {
		if _, dup := s.static[c.ID]; dup {
			return nil, fmt.Errorf("oauth: duplicate client %q", c.ID)
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("oauth: %w", err)
		}
		// A copy, so changing the caller's slice can't change a client
		// after New.
		c.RedirectURIs = slices.Clone(c.RedirectURIs)
		s.static[c.ID] = c
		s.serviceClients = s.serviceClients || c.ServiceAccount != ""
	}
	if s.serviceClients && cfg.ServiceTokens == nil {
		return nil, errors.New("oauth: client-credentials clients need a service token minter")
	}
	return s, nil
}

// checkGoogleEndpoints requires overridden Google and vault endpoints to
// be https (http only on loopback, for tests): the ID token is trusted
// for coming straight from Google's token endpoint over TLS, and the
// vault hands out users' Google tokens.
func checkGoogleEndpoints(cfg Config) error {
	endpoints := [][2]string{
		{"Google.AuthURL", cfg.Google.AuthURL},
		{"Google.TokenURL", cfg.Google.TokenURL},
		{"Google.TokenInfoURL", cfg.Google.TokenInfoURL},
	}
	if v, ok := cfg.Custody.(*VaultCustody); ok && v != nil {
		endpoints = append(endpoints, [2]string{"VaultCustody.Endpoint", v.Endpoint})
	}
	for _, e := range endpoints {
		if e[1] == "" {
			continue // Google's default
		}
		u, err := url.Parse(e[1])
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname()))) {
			return fmt.Errorf("oauth: %s %q must be an https URL (or http on loopback, for testing)", e[0], e[1])
		}
	}
	return nil
}

// Resource is the MCP endpoint URL tokens are bound to.
func (s *Server) Resource() string { return s.resource }

// ResourceMetadataURL is where the protected-resource metadata lives;
// 401s point clients at it.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.Issuer + "/.well-known/oauth-protected-resource/mcp"
}

func (s *Server) callbackURI() string { return s.cfg.Issuer + "/callback" }
func (s *Server) continueURI() string { return s.cfg.Issuer + "/consent/continue" }

// Handler serves the metadata and authorization-server endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	prm := cors(http.HandlerFunc(s.handleResourceMetadata))
	mux.Handle("/.well-known/oauth-protected-resource", prm)
	mux.Handle("/.well-known/oauth-protected-resource/mcp", prm)
	asm := cors(http.HandlerFunc(s.handleServerMetadata))
	mux.Handle("/.well-known/oauth-authorization-server", asm)
	mux.Handle("/.well-known/openid-configuration", asm)
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("POST /authorize", s.handleApprove)
	mux.HandleFunc("GET /callback", s.handleCallback)
	mux.HandleFunc("GET /consent/continue", s.handleConsentContinue)
	mux.Handle("/token", cors(http.HandlerFunc(s.handleToken)))
	if s.cfg.OpenRegistration {
		mux.Handle("/register", cors(http.HandlerFunc(s.handleRegister)))
	}
	return mux
}

func (s *Server) handleResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resource,
		"authorization_servers":    []string{s.cfg.Issuer},
		"scopes_supported":         []string{ScopeExecute},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "kode-gopher",
	})
}

func (s *Server) handleServerMetadata(w http.ResponseWriter, _ *http.Request) {
	m := map[string]any{
		"issuer":                                         s.cfg.Issuer,
		"authorization_endpoint":                         s.cfg.Issuer + "/authorize",
		"token_endpoint":                                 s.cfg.Issuer + "/token",
		"scopes_supported":                               []string{ScopeExecute},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	}
	if s.serviceClients {
		m["grant_types_supported"] = []string{"authorization_code", "refresh_token", "client_credentials"}
	}
	if s.cfg.OpenRegistration {
		m["registration_endpoint"] = s.cfg.Issuer + "/register"
		m["client_id_metadata_document_supported"] = true
	}
	writeJSON(w, http.StatusOK, m)
}

// cors lets browser-based clients (MCP Inspector) call the metadata,
// token and registration endpoints. None of them use cookies.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// replayCache remembers single-use IDs (authorization codes) until
// they'd have expired anyway, and tracks refresh-token families. It's in
// memory: v1 runs one replica, and a restart forgets it
// (docs/design-in-cluster.md).
//
// Every refresh token belongs to a family, started when its sign-in's
// code was exchanged, and carries a generation number. A refresh moves
// the family from the presented token's generation to the next.
// Presenting a generation the family has already moved past is a replay:
// one of the token's two holders isn't the client it was issued to, so
// the whole family is revoked and the user signs in again (OAuth 2.1
// §4.3.1). Memory grows with families (one per sign-in per client that
// has refreshed), not with refreshes.
type replayCache struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	families map[string]family
}

type family struct {
	gen     uint64
	exp     time.Time
	revoked bool
}

// use records id, valid until exp, and reports whether it was fresh.
func (c *replayCache) use(id string, exp time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.seen) > 1024 {
		for k, e := range c.seen {
			if now.After(e) {
				delete(c.seen, k)
			}
		}
	}
	if e, ok := c.seen[id]; ok && now.Before(e) {
		return false
	}
	c.seen[id] = exp
	return true
}

// release forgets id, for a use that failed for a transient reason, so
// the client's retry isn't refused as a replay.
func (c *replayCache) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, id)
}

// rotate moves family fam from generation gen to gen+1, until exp, and
// reports whether it could. It reports false, and revokes the family, if
// the family is already past gen; false also for a revoked family. An
// unknown or expired family (one never refreshed, or forgotten by a
// restart) starts at gen+1.
func (c *replayCache) rotate(fam string, gen uint64, exp time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.families == nil {
		c.families = map[string]family{}
	}
	if len(c.families) > 1024 {
		for k, f := range c.families {
			if now.After(f.exp) {
				delete(c.families, k)
			}
		}
	}
	if f, ok := c.families[fam]; ok && now.Before(f.exp) {
		if f.revoked {
			return false
		}
		if gen < f.gen {
			f.revoked = true
			c.families[fam] = f
			return false
		}
	}
	c.families[fam] = family{gen: gen + 1, exp: exp}
	return true
}

// unrotate moves family fam back from gen+1 to gen, for a refresh that
// failed for a transient reason, so the client's retry isn't taken for a
// replay. A family that has moved on or been revoked is left alone.
func (c *replayCache) unrotate(fam string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.families[fam]; ok && !f.revoked && f.gen == gen+1 {
		f.gen = gen
		c.families[fam] = f
	}
}

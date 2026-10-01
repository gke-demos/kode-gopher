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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// client is an OAuth client resolved from its client_id.
type client struct {
	ID           string
	Name         string
	RedirectURIs []string
	// Secret is set only for pre-registered confidential clients.
	Secret string
}

// StaticClient is a pre-registered client, from the clients file.
type StaticClient struct {
	ID string `json:"client_id"`
	// Secret makes the client confidential: it must authenticate at
	// /token (client_secret_basic or client_secret_post).
	Secret       string   `json:"client_secret,omitempty"`
	Name         string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
}

// LoadStaticClients reads a JSON array of StaticClient.
func LoadStaticClients(path string) ([]StaticClient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read clients: %w", err)
	}
	var cs []StaticClient
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("parse clients %s: %w", path, err)
	}
	for _, c := range cs {
		if c.ID == "" || isSealed(c.ID) || strings.HasPrefix(c.ID, "https://") || len(c.RedirectURIs) == 0 {
			return nil, fmt.Errorf("clients %s: client %q needs a plain client_id and redirect_uris", path, c.ID)
		}
	}
	return cs, nil
}

// dcrClient is a dynamically registered client, sealed into its own
// client_id so nothing is stored.
type dcrClient struct {
	RedirectURIs []string `json:"r"`
	Name         string   `json:"n,omitempty"`
	IssuedAt     int64    `json:"t"`
}

var errUnknownClient = errors.New("unknown client")

// resolveClient finds the client a client_id names: pre-registered, a
// sealed DCR registration, or a CIMD URL.
func (s *Server) resolveClient(ctx context.Context, id string) (*client, error) {
	if c, ok := s.static[id]; ok {
		return &client{ID: c.ID, Name: c.Name, RedirectURIs: c.RedirectURIs, Secret: c.Secret}, nil
	}
	if !s.cfg.OpenRegistration {
		return nil, errUnknownClient
	}
	if isSealed(id) {
		var d dcrClient
		if s.keys.open(purposeClient, id, &d) != nil {
			return nil, errUnknownClient
		}
		return &client{ID: id, Name: d.Name, RedirectURIs: d.RedirectURIs}, nil
	}
	if strings.HasPrefix(id, "https://") {
		return s.cimd.fetch(ctx, id)
	}
	return nil, errUnknownClient
}

// redirectAllowed reports whether uri is one of c's redirect URIs. The
// match is exact, except that a loopback redirect (RFC 8252 §7.3) may
// use any port.
func redirectAllowed(c *client, uri string) bool {
	if slices.Contains(c.RedirectURIs, uri) {
		return true
	}
	got, err := url.Parse(uri)
	if err != nil || got.Scheme != "http" || !isLoopbackHost(got.Hostname()) {
		return false
	}
	for _, r := range c.RedirectURIs {
		want, err := url.Parse(r)
		if err != nil || want.Scheme != "http" || !isLoopbackHost(want.Hostname()) {
			continue
		}
		if want.Hostname() == got.Hostname() && want.Path == got.Path && want.RawQuery == got.RawQuery {
			return true
		}
	}
	return false
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.IsLoopback()
}

// validRedirectURI is the registration-time check: an absolute URI with
// no fragment, HTTPS unless it's a loopback or a private-use scheme
// (RFC 8252 §7.1, e.g. com.example.app:/cb).
func validRedirectURI(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	case "javascript", "data", "file", "vbscript":
		return false
	}
	return strings.Contains(u.Scheme, ".")
}

// cimdFetcher fetches Client ID Metadata Documents: the client_id is an
// HTTPS URL serving the client's metadata. The URL is client-supplied,
// so the fetch only reaches public IPs, follows no redirects, and is
// size- and time-capped.
type cimdFetcher struct {
	hc *http.Client

	mu    sync.Mutex
	cache map[string]cimdEntry
}

type cimdEntry struct {
	c       *client
	expires time.Time
}

const (
	cimdMaxBytes   = 64 << 10
	cimdDefaultTTL = 5 * time.Minute
	cimdMaxTTL     = time.Hour
)

func newCIMDFetcher(hc *http.Client) *cimdFetcher {
	if hc == nil {
		hc = SSRFSafeClient(5 * time.Second)
	}
	return &cimdFetcher{hc: hc, cache: map[string]cimdEntry{}}
}

func (f *cimdFetcher) fetch(ctx context.Context, id string) (*client, error) {
	u, err := url.Parse(id)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return nil, errors.New("client ID metadata document URL must be an HTTPS URL with a path")
	}
	f.mu.Lock()
	e, ok := f.cache[id]
	f.mu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.c, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil) //nolint:gosec // see the Do below
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.hc.Do(req) //nolint:gosec // client-supplied URL, fetched with SSRFSafeClient
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch client metadata: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	if len(b) > cimdMaxBytes {
		return nil, errors.New("client metadata document is too large")
	}
	var doc struct {
		ClientID                string   `json:"client_id"`
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse client metadata: %w", err)
	}
	switch {
	case doc.ClientID != id:
		return nil, errors.New("client metadata client_id doesn't match its URL")
	case len(doc.RedirectURIs) == 0:
		return nil, errors.New("client metadata has no redirect_uris")
	case doc.TokenEndpointAuthMethod != "" && doc.TokenEndpointAuthMethod != "none":
		return nil, fmt.Errorf("client metadata token_endpoint_auth_method %q isn't supported (only none)", doc.TokenEndpointAuthMethod)
	}
	for _, r := range doc.RedirectURIs {
		if !validRedirectURI(r) {
			return nil, fmt.Errorf("client metadata redirect URI %q isn't allowed", r)
		}
	}
	c := &client{ID: id, Name: doc.ClientName, RedirectURIs: doc.RedirectURIs}
	if ttl := cacheTTL(resp.Header.Get("Cache-Control")); ttl > 0 {
		f.mu.Lock()
		f.cache[id] = cimdEntry{c: c, expires: time.Now().Add(ttl)}
		f.mu.Unlock()
	}
	return c, nil
}

// cacheTTL reads max-age from Cache-Control, capped; no-store and
// no-cache mean don't cache.
func cacheTTL(cc string) time.Duration {
	ttl := cimdDefaultTTL
	for _, d := range strings.Split(cc, ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		switch {
		case d == "no-store" || d == "no-cache":
			return 0
		case strings.HasPrefix(d, "max-age="):
			n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age="))
			if err == nil {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return min(ttl, cimdMaxTTL)
}

// SSRFSafeClient is an HTTP client for client-supplied URLs: it dials
// only public unicast addresses (checked on the resolved IP, so DNS
// can't smuggle in a private one), uses no proxy, and follows no
// redirects.
func SSRFSafeClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !isPublic(ip) {
				return fmt.Errorf("refusing to dial non-public address %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects aren't followed")
		},
	}
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublic(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip)
}

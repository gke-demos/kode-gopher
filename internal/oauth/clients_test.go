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
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The fixes in this file came from go-steer/steward's review of its port
// of this package.

// syncBuffer is a log destination the test can read while servers write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// captureLog sends the standard logger to a buffer for the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return buf
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// get fetches u without following redirects and returns the status and
// body.
func get(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// tokenError posts form to /token and returns the status, error and
// error_description.
func (e *testEnv) tokenError(form url.Values) (int, string, string) {
	e.t.Helper()
	resp, err := http.PostForm(e.srv.URL+"/token", form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		Error string `json:"error"`
		Desc  string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r.Error, r.Desc
}

// TestIsPublicSpecialRanges checks the SSRF filter refuses the
// special-purpose ranges IsGlobalUnicast admits, above all the IPv6
// prefixes that embed an IPv4 address: through a NAT64 gateway,
// 64:ff9b::a9fe:a9fe is the metadata server.
func TestIsPublicSpecialRanges(t *testing.T) {
	for _, a := range []string{
		"64:ff9b::a9fe:a9fe",   // NAT64 (RFC 6052)
		"64:ff9b:1::a9fe:a9fe", // local-use NAT64 (RFC 8215)
		"2002:a9fe:a9fe::1",    // 6to4
		"2001:0:4136:e378::1",  // Teredo
		"2001:2::1",            // IETF protocol assignments, benchmarking
		"2001:db8::1",          // documentation
		"::a9fe:a9fe",          // IPv4-compatible
		"::ffff:0:a9fe:a9fe",   // SIIT
		"0.1.2.3",              // "this network"
		"192.0.0.8",            // IETF protocol assignments
		"192.88.99.1",          // 6to4 relay anycast
		"198.18.0.1",           // benchmarking
		"240.0.0.1",            // reserved
		"192.0.2.1",            // documentation (TEST-NET-1)
		"198.51.100.1",         // documentation (TEST-NET-2)
		"203.0.113.1",          // documentation (TEST-NET-3)
		"3fff::1",              // documentation (RFC 9637)
		"fec0::1",              // deprecated site-local
		// Prefix.Contains is false for a zoned address, so a zone mustn't
		// carry one past the ranges, nor make a public address dialable.
		"64:ff9b::a9fe:a9fe%eth0",
		"2002:a9fe:a9fe::1%lo",
		"2001:db8::1%x",
		"2606:4700::1%x",
	} {
		if isPublic(netip.MustParseAddr(a)) {
			t.Errorf("isPublic(%s) = true", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "2001:4860:4860::8888", "2600:1900::1"} {
		if !isPublic(netip.MustParseAddr(a)) {
			t.Errorf("isPublic(%s) = false", a)
		}
	}
}

// TestSecretMatches covers the digest comparison of client secrets. It
// passes on the old raw comparison too, by design: what changed is the
// timing of a length mismatch, which a unit test can't observe.
func TestSecretMatches(t *testing.T) {
	if !secretMatches("s/3+cret", "s/3+cret") {
		t.Error("the right secret refused")
	}
	for _, got := range []string{"", "s", "s/3+cre", "s/3+cret ", "S/3+CRET", strings.Repeat("x", 1000)} {
		if secretMatches(got, "s/3+cret") {
			t.Errorf("secret %q accepted", got)
		}
	}
}

// TestStaticClientRedirectsValidated checks a pre-registered client's
// redirect URIs pass the same check as a registered one's, from the
// clients file and from Config.
func TestStaticClientRedirectsValidated(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k", Secret: []byte(strings.Repeat("k", 32))})
	for _, bad := range []string{"http://evil.example/cb", "javascript:alert(1)", "https://app.example/cb#frag", "/relative"} {
		c := StaticClient{ID: "cli", RedirectURIs: []string{clientRedirect, bad}}
		cfg := Config{Issuer: "https://kg.example.com", Google: GoogleClient{ClientID: "id", ClientSecret: "s"},
			Custody: SealedCustody{}, Allow: &AllowList{Domains: []string{"example.com"}}, Keys: keys, Clients: []StaticClient{c}}
		if _, err := New(cfg); err == nil {
			t.Errorf("New accepted a static client redirecting to %q", bad)
		}
		path := filepath.Join(t.TempDir(), "clients.json")
		b, _ := json.Marshal([]StaticClient{c})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStaticClients(path); err == nil {
			t.Errorf("LoadStaticClients accepted a client redirecting to %q", bad)
		}
	}
	path := filepath.Join(t.TempDir(), "clients.json")
	if err := os.WriteFile(path, []byte(`[{"client_id":"cli","redirect_uris":["https://app.example/cb","http://127.0.0.1/cb","com.example.app:/cb"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStaticClients(path); err != nil {
		t.Errorf("good redirect URIs refused: %v", err)
	}
}

// TestStaticClientNotAliased checks New copies a pre-registered client's
// redirect URIs, so changing the caller's slice afterwards can't add a
// redirect.
func TestStaticClientNotAliased(t *testing.T) {
	var clients []StaticClient
	e := newTestEnv(t, true, func(c *Config) {
		c.Clients = []StaticClient{{ID: "cli", RedirectURIs: []string{clientRedirect}}}
		clients = c.Clients
	})
	clients[0].RedirectURIs[0] = "https://evil.example/cb"
	c, err := e.as.resolveClient(context.Background(), "cli")
	if err != nil {
		t.Fatal(err)
	}
	if redirectAllowed(c, "https://evil.example/cb") || !redirectAllowed(c, clientRedirect) {
		t.Errorf("client after the caller's slice changed: %v", c.RedirectURIs)
	}
}

// TestCIMDErrorsNotEchoed checks why a client couldn't be resolved
// reaches only the log: the reason can quote the client-supplied
// metadata document (text the attacker chose, shown on kode-gopher's
// page) or a dial error naming an internal address.
func TestCIMDErrorsNotEchoed(t *testing.T) {
	logs := captureLog(t)
	const planted = "Sign in again at https://evil.example/login"
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"client_id": docURL, "redirect_uris": []string{clientRedirect}, "token_endpoint_auth_method": planted})
	}))
	t.Cleanup(doc.Close)
	docURL = doc.URL + "/c.json"
	e := newTestEnv(t, false, func(c *Config) { c.CIMDClient = doc.Client() })
	v := rand.Text() + rand.Text()
	if code, body := get(t, e.authorizeURL(docURL, v, nil)); code != http.StatusBadRequest || strings.Contains(body, "evil.example") {
		t.Errorf("authorize page: HTTP %d, echoes the document: %v", code, strings.Contains(body, "evil.example"))
	}
	if code, errCode, desc := e.tokenError(exchangeForm(docURL, "x", v)); code != http.StatusUnauthorized || errCode != "invalid_client" || strings.Contains(desc, "evil") {
		t.Errorf("token endpoint: HTTP %d %s %q", code, errCode, desc)
	}
	if !strings.Contains(logs.String(), "evil.example") {
		t.Error("the reason didn't reach the log")
	}

	// The default SSRF-safe fetcher refuses a loopback client_id URL;
	// the refusal names the address, so it stays in the log too.
	internal := newTestEnv(t, false, nil)
	u, _ := url.Parse(doc.URL)
	id := "https://" + u.Host + "/c.json"
	if code, body := get(t, internal.authorizeURL(id, v, nil)); code != http.StatusBadRequest || strings.Contains(body, "non-public") {
		t.Errorf("authorize page: HTTP %d, echoes the dial error: %v", code, strings.Contains(body, "non-public"))
	}
	if _, errCode, desc := internal.tokenError(exchangeForm(id, "x", v)); errCode != "invalid_client" || strings.Contains(desc, "non-public") {
		t.Errorf("token endpoint: %s %q", errCode, desc)
	}
	if !strings.Contains(logs.String(), "non-public address 127.0.0.1") {
		t.Error("the dial error didn't reach the log")
	}
}

// TestCIMDHostDown checks a client whose metadata host is failing is
// "try again" (503 on the page, server_error at /token), not "unknown
// client"; a host that answers 404 is still an unknown client.
func TestCIMDHostDown(t *testing.T) {
	captureLog(t)
	status := http.StatusServiceUnavailable
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(doc.Close)
	docURL = doc.URL + "/c.json"
	e := newTestEnv(t, false, func(c *Config) { c.CIMDClient = doc.Client() })
	v := rand.Text() + rand.Text()
	if code, _ := get(t, e.authorizeURL(docURL, v, nil)); code != http.StatusServiceUnavailable {
		t.Errorf("authorize with the client's host down: HTTP %d, want 503", code)
	}
	if code, errCode, _ := e.tokenError(exchangeForm(docURL, "x", v)); code != http.StatusInternalServerError || errCode != "server_error" {
		t.Errorf("token endpoint with the client's host down: HTTP %d %s, want server_error", code, errCode)
	}
	status = http.StatusNotFound
	if code, _ := get(t, e.authorizeURL(docURL, v, nil)); code != http.StatusBadRequest {
		t.Errorf("authorize with no document: HTTP %d, want 400", code)
	}
	if _, errCode, _ := e.tokenError(exchangeForm(docURL, "x", v)); errCode != "invalid_client" {
		t.Errorf("token endpoint with no document: %s, want invalid_client", errCode)
	}
}

// TestCIMDErrorClassification checks which fetch failures are the host
// being unavailable (a retry may work) and which make the client
// unknown.
func TestCIMDErrorClassification(t *testing.T) {
	ctx := context.Background()
	answer := func(code int) *cimdFetcher {
		return newCIMDFetcher(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			rec.WriteHeader(code)
			return rec.Result(), nil
		})})
	}
	fail := func(err error) *cimdFetcher {
		return newCIMDFetcher(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, err })})
	}
	const id = "https://x.example/c.json"
	for name, f := range map[string]*cimdFetcher{
		"429":         answer(http.StatusTooManyRequests),
		"502":         answer(http.StatusBadGateway),
		"DNS timeout": fail(&net.DNSError{Err: "timeout", Name: "x.example", IsTimeout: true}),
		"refused":     fail(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}),
	} {
		if _, err := f.fetch(ctx, id); !errors.Is(err, errCIMDUnavailable) {
			t.Errorf("%s: %v, want unavailable", name, err)
		}
	}
	for name, f := range map[string]*cimdFetcher{
		"404":      answer(http.StatusNotFound),
		"NXDOMAIN": fail(&net.DNSError{Err: "no such host", Name: "x.example", IsNotFound: true}),
	} {
		if _, err := f.fetch(ctx, id); err == nil || errors.Is(err, errCIMDUnavailable) {
			t.Errorf("%s: %v, want an unknown client", name, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if _, err := newCIMDFetcher(nil).fetch(ctx, "https://"+u.Host+"/c.json"); err == nil || errors.Is(err, errCIMDUnavailable) || !errors.Is(err, errNonPublic) {
		t.Errorf("loopback CIMD URL: %v, want refused as non-public and unknown", err)
	}
	badCert := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer badCert.Close()
	if _, err := newCIMDFetcher(&http.Client{Transport: &http.Transport{}}).fetch(ctx, badCert.URL+"/c.json"); err == nil || errors.Is(err, errCIMDUnavailable) {
		t.Errorf("untrusted certificate: %v, want an unknown client", err)
	}
}

// TestGoogleEndpointsHTTPS checks overridden Google and vault endpoints
// must be https, or http on loopback (the tests' fakes): the ID token is
// trusted for coming from Google's token endpoint over TLS.
func TestGoogleEndpointsHTTPS(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k", Secret: []byte(strings.Repeat("k", 32))})
	good := func() Config {
		return Config{Issuer: "https://kg.example.com", Google: GoogleClient{ClientID: "id", ClientSecret: "s"},
			Custody: SealedCustody{}, Allow: &AllowList{Domains: []string{"example.com"}}, Keys: keys}
	}
	for name, edit := range map[string]func(*Config){
		"http token URL":     func(c *Config) { c.Google.TokenURL = "http://oauth2.example.com/token" },
		"http auth URL":      func(c *Config) { c.Google.AuthURL = "http://accounts.example.com/auth" },
		"http tokeninfo URL": func(c *Config) { c.Google.TokenInfoURL = "http://oauth2.example.com/tokeninfo" },
		"relative token URL": func(c *Config) { c.Google.TokenURL = "/token" },
		"loopback lookalike": func(c *Config) { c.Google.TokenURL = "http://localhost.evil.example/token" },
		"http vault": func(c *Config) {
			c.Custody = &VaultCustody{AuthProvider: "projects/p/locations/l/authProviders/kg", Client: http.DefaultClient, Endpoint: "http://vault.example.com/v1/"}
		},
	} {
		c := good()
		edit(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := good()
	c.Google.TokenURL, c.Google.AuthURL = "https://oauth2.example.com/token", "http://127.0.0.1:8080/auth"
	c.Custody = &VaultCustody{AuthProvider: "projects/p/locations/l/authProviders/kg", Client: http.DefaultClient, Endpoint: "http://[::1]:9/v1/"}
	if _, err := New(c); err != nil {
		t.Errorf("https and loopback endpoints refused: %v", err)
	}
}

// TestCIMDRedirectIsUnknownClient checks a metadata host that answers
// with a redirect makes the client unknown, not the host unavailable:
// a retry won't change it.
func TestCIMDRedirectIsUnknownClient(t *testing.T) {
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere.json", http.StatusFound)
	}))
	defer doc.Close()
	hc := doc.Client()
	hc.CheckRedirect = SSRFSafeClient(time.Second).CheckRedirect
	_, err := newCIMDFetcher(hc).fetch(context.Background(), doc.URL+"/c.json")
	if err == nil || errors.Is(err, errCIMDUnavailable) || !errors.Is(err, errRedirect) {
		t.Errorf("redirecting metadata host: %v, want an unknown client", err)
	}
}

// TestCIMDCacheBounded checks the metadata document cache can't grow
// without bound: anyone can name a new client_id URL. When it's full,
// expired entries go first, then everything.
func TestCIMDCacheBounded(t *testing.T) {
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60") // writeJSON says no-store
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": docURL, "redirect_uris": []string{clientRedirect}})
	}))
	defer doc.Close()
	docURL = doc.URL + "/c.json"
	f := newCIMDFetcher(doc.Client())
	fill := func(expired int) {
		clear(f.cache)
		for i := range cimdMaxCached {
			exp := time.Now().Add(time.Hour)
			if i < expired {
				exp = time.Now().Add(-time.Second)
			}
			f.cache["https://c"+strconv.Itoa(i)+".example/c.json"] = cimdEntry{c: &client{}, expires: exp}
		}
	}
	fill(10)
	if _, err := f.fetch(context.Background(), docURL); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.cache[docURL]; !ok || len(f.cache) != cimdMaxCached-10+1 {
		t.Errorf("full cache with expired entries: %d entries, want %d", len(f.cache), cimdMaxCached-10+1)
	}
	fill(0)
	if _, err := f.fetch(context.Background(), docURL); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.cache[docURL]; !ok || len(f.cache) != 1 {
		t.Errorf("full cache of live entries: %d entries, want 1", len(f.cache))
	}
}

// TestClientNamesTruncated checks client names are cut to
// maxClientName bytes on a character boundary: a CIMD client's name, so
// a long one can't push its URL off the consent page or bloat the flow,
// and a DCR client's, which used to be cut mid-character.
func TestClientNamesTruncated(t *testing.T) {
	long := strings.Repeat("é", 300)
	check := func(where, name string) {
		t.Helper()
		if len(name) > maxClientName || len(name) < maxClientName-1 || !utf8.ValidString(name) || strings.ContainsRune(name, utf8.RuneError) {
			t.Errorf("%s: client name is %d bytes, valid UTF-8 %v", where, len(name), utf8.ValidString(name) && !strings.ContainsRune(name, utf8.RuneError))
		}
	}
	var docURL string
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"client_id": docURL, "client_name": "x" + long, "redirect_uris": []string{clientRedirect}})
	}))
	defer doc.Close()
	docURL = doc.URL + "/c.json"
	c, err := newCIMDFetcher(doc.Client()).fetch(context.Background(), docURL)
	if err != nil {
		t.Fatal(err)
	}
	check("CIMD", c.Name)

	e := newTestEnv(t, false, nil)
	body, _ := json.Marshal(map[string]any{"client_name": "x" + long, "redirect_uris": []string{clientRedirect}})
	resp, err := http.Post(e.srv.URL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m struct {
		ClientID   string `json:"client_id"`
		ClientName string `json:"client_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	check("DCR response", m.ClientName)
	dc, err := e.as.resolveClient(context.Background(), m.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	check("DCR registration", dc.Name)
}

// TestIssuerLoopback checks an http issuer must really be on loopback,
// not merely start with "http://localhost".
func TestIssuerLoopback(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k", Secret: []byte(strings.Repeat("k", 32))})
	cfg := func(issuer string) Config {
		return Config{Issuer: issuer, Google: GoogleClient{ClientID: "id", ClientSecret: "s"},
			Custody: SealedCustody{}, Allow: &AllowList{Domains: []string{"example.com"}}, Keys: keys}
	}
	for _, bad := range []string{"http://localhost.evil.example", "http://127.0.0.1.nip.io", "http://localhost@evil.example", "https://", "ftp://kg.example.com"} {
		if _, err := New(cfg(bad)); err == nil {
			t.Errorf("issuer %q accepted", bad)
		}
	}
	for _, good := range []string{"https://kg.example.com", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := New(cfg(good)); err != nil {
			t.Errorf("issuer %q refused: %v", good, err)
		}
	}
}

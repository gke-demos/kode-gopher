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
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeyring(t *testing.T) {
	old := KeyringKey{ID: "old", Secret: []byte(strings.Repeat("o", 32))}
	cur := KeyringKey{ID: "new", Secret: []byte(strings.Repeat("n", 32))}
	k1, _ := NewKeyring(old)
	k2, err := NewKeyring(cur, old)
	if err != nil {
		t.Fatal(err)
	}
	type payload struct{ V string }
	sealedOld, _ := k1.seal(purposeAccess, payload{"a"})
	var p payload
	if err := k2.open(purposeAccess, sealedOld, &p); err != nil || p.V != "a" {
		t.Fatalf("rotated keyring can't open an old envelope: %v", err)
	}
	s, _ := k2.seal(purposeAccess, payload{"b"})
	if !strings.HasPrefix(s, "kg1.new.") {
		t.Errorf("sealed with %q, want the first key", s[:8])
	}
	if k1.open(purposeAccess, s, &p) == nil {
		t.Error("a keyring without the sealing key opened the envelope")
	}
	if k2.open(purposeRefresh, s, &p) == nil {
		t.Error("an access envelope opened as a refresh token")
	}
	b := []byte(s)
	b[len(b)-2] ^= 1
	if k2.open(purposeAccess, string(b), &p) == nil {
		t.Error("a tampered envelope opened")
	}
	for _, bad := range []KeyringKey{{ID: "a.b", Secret: cur.Secret}, {ID: "", Secret: cur.Secret}, {ID: "x", Secret: []byte("short")}} {
		if _, err := NewKeyring(bad); err == nil {
			t.Errorf("NewKeyring(%q, %d bytes) succeeded", bad.ID, len(bad.Secret))
		}
	}
	if _, err := NewKeyring(cur, cur); err == nil {
		t.Error("duplicate key IDs accepted")
	}
}

func TestLoadKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))
	if err := os.WriteFile(path, []byte("# comment\n\nk2 "+key+"\nk1 "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKeyring(path)
	if err != nil || len(k.ids) != 2 || k.ids[0] != "k2" {
		t.Fatalf("LoadKeyring = %v, %v", k, err)
	}
	if err := os.WriteFile(path, []byte("k1 not-base64!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(path); err == nil {
		t.Error("bad base64 accepted")
	}
}

func TestRedirects(t *testing.T) {
	c := &client{RedirectURIs: []string{"http://127.0.0.1/cb", "https://app.example/cb", "com.example.app:/cb"}}
	allowed := map[string]bool{
		"http://127.0.0.1/cb":         true,
		"http://127.0.0.1:5555/cb":    true,
		"https://app.example/cb":      true,
		"com.example.app:/cb":         true,
		"http://127.0.0.1:5555/other": false,
		"http://localhost:5555/cb":    false, // a different loopback host
		"https://app.example:444/cb":  false, // only loopback ports float
		"https://app.example/cb?x=1":  false,
		"https://evil.example/cb":     false,
	}
	for uri, want := range allowed {
		if got := redirectAllowed(c, uri); got != want {
			t.Errorf("redirectAllowed(%q) = %v, want %v", uri, got, want)
		}
	}
	valid := map[string]bool{
		"https://app.example/cb":   true,
		"http://localhost:8/cb":    true,
		"http://[::1]/cb":          true,
		"com.example.app:/cb":      true,
		"http://app.example/cb":    false,
		"https://app.example/#x":   false,
		"javascript:alert(1)":      false,
		"myapp:/cb":                false,
		"/relative":                false,
		"https:///no-host":         false,
		"file:///etc/passwd":       false,
		"data:text/html,<script>":  false,
		"vbscript.x:/cb":           true,
		"http://127.0.0.1.nip.io/": false,
	}
	for uri, want := range valid {
		if got := validRedirectURI(uri); got != want {
			t.Errorf("validRedirectURI(%q) = %v, want %v", uri, got, want)
		}
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2001:4860:4860::8888": true,
		"10.0.0.1": false, "172.16.0.1": false, "192.168.1.1": false, "127.0.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fd00::1": false, "fe80::1": false, "::ffff:10.0.0.1": false, "224.0.0.1": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestSSRFSafeClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	resp, err := SSRFSafeClient(time.Second).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("SSRF-safe client dialed loopback")
	}
}

func TestCacheTTL(t *testing.T) {
	for cc, want := range map[string]time.Duration{
		"":                     cimdDefaultTTL,
		"max-age=60":           time.Minute,
		"public, max-age=9999": cimdMaxTTL,
		"no-store":             0,
		"max-age=60, no-cache": 0,
	} {
		if got := cacheTTL(cc); got != want {
			t.Errorf("cacheTTL(%q) = %v, want %v", cc, got, want)
		}
	}
}

func TestCIMDDocumentChecks(t *testing.T) {
	var docURL string
	doc := map[string]any{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()
	docURL = srv.URL + "/c.json"
	f := newCIMDFetcher(srv.Client())
	cases := []struct {
		name string
		doc  map[string]any
		ok   bool
	}{
		{"good", map[string]any{"client_id": docURL, "redirect_uris": []string{clientRedirect}}, true},
		{"other client_id", map[string]any{"client_id": srv.URL + "/other.json", "redirect_uris": []string{clientRedirect}}, false},
		{"no redirects", map[string]any{"client_id": docURL}, false},
		{"confidential", map[string]any{"client_id": docURL, "redirect_uris": []string{clientRedirect}, "token_endpoint_auth_method": "client_secret_basic"}, false},
	}
	for _, tc := range cases {
		doc = tc.doc
		_, err := f.fetch(context.Background(), docURL)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	for _, id := range []string{"http://x.example/c.json", "https://x.example", "https://x.example/", "https://u@x.example/c", "https://x.example/c#f"} {
		if _, err := f.fetch(context.Background(), id); err == nil {
			t.Errorf("CIMD client_id %q accepted", id)
		}
	}
}

func TestVerifyIDToken(t *testing.T) {
	g := &GoogleClient{ClientID: testGoogleID}
	now := time.Unix(1_800_000_000, 0)
	enc := func(claims map[string]any) string {
		b, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
	}
	base := func() map[string]any {
		return map[string]any{"iss": "https://accounts.google.com", "aud": testGoogleID, "sub": "1",
			"email": "a@example.com", "email_verified": true, "hd": "example.com", "exp": now.Unix() + 60}
	}
	u, err := g.verifyIDToken(enc(base()), now)
	if err != nil || u != (User{Sub: "1", Email: "a@example.com", Domain: "example.com"}) {
		t.Fatalf("verifyIDToken = %+v, %v", u, err)
	}
	multi := base()
	multi["aud"] = []string{"x", testGoogleID}
	if _, err := g.verifyIDToken(enc(multi), now); err != nil {
		t.Errorf("array aud: %v", err)
	}
	for name, edit := range map[string]func(map[string]any){
		"issuer":     func(c map[string]any) { c["iss"] = "https://evil.example" },
		"audience":   func(c map[string]any) { c["aud"] = "other.apps.googleusercontent.com" },
		"expired":    func(c map[string]any) { c["exp"] = now.Unix() },
		"no sub":     func(c map[string]any) { delete(c, "sub") },
		"unverified": func(c map[string]any) { c["email_verified"] = false },
	} {
		c := base()
		edit(c)
		if _, err := g.verifyIDToken(enc(c), now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := g.verifyIDToken("not-a-jwt", now); err == nil {
		t.Error("malformed token accepted")
	}
}

func TestAllowList(t *testing.T) {
	groups := &fakeGroups{members: map[string]map[string]bool{"g@example.com": {"bob@other.org": true}}}
	a := &AllowList{Domains: []string{"example.com"}, Groups: []string{"g@example.com"}, GroupChecker: groups}
	ctx := context.Background()
	if err := a.admit(ctx, alice); err != nil || groups.calls != 0 {
		t.Errorf("domain member: %v, %d group checks", err, groups.calls)
	}
	// The hd claim counts, not the email's domain.
	if err := a.admit(ctx, User{Sub: "9", Email: "eve@example.com"}); err == nil {
		t.Error("consumer account with a matching email suffix admitted")
	}
	if err := a.admit(ctx, bob); err != nil {
		t.Errorf("group member: %v", err)
	}
	calls := groups.calls
	_ = a.admit(ctx, bob)
	if groups.calls != calls {
		t.Error("group decision not cached")
	}
	if err := a.admit(ctx, mallory); err != errNotAllowed {
		t.Errorf("outsider: %v", err)
	}
}

func TestCloudIdentityGroups(t *testing.T) {
	lookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/groups:lookup" && r.URL.Query().Get("groupKey.id") == "g@example.com":
			lookups++
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/abc"})
		case r.URL.Path == "/groups/abc/memberships:checkTransitiveMembership":
			switch r.URL.Query().Get("query") {
			case "member_key_id == 'bob@other.org'":
				writeJSON(w, http.StatusOK, map[string]bool{"hasMembership": true})
			case "member_key_id == 'ghost@nowhere.org'":
				http.Error(w, "not found", http.StatusNotFound)
			default:
				writeJSON(w, http.StatusOK, map[string]bool{})
			}
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	for member, want := range map[string]bool{"bob@other.org": true, "mallory@gmail.com": false, "ghost@nowhere.org": false} {
		got, err := c.IsMember(ctx, "g@example.com", member)
		if err != nil || got != want {
			t.Errorf("IsMember(%s) = %v, %v; want %v", member, got, err, want)
		}
	}
	if lookups != 1 {
		t.Errorf("group looked up %d times, want 1", lookups)
	}
	if _, err := c.IsMember(ctx, "denied@example.com", "bob@other.org"); err == nil {
		t.Error("a failed group lookup reported a decision")
	}
}

func TestNewValidates(t *testing.T) {
	keys, _ := NewKeyring(KeyringKey{ID: "k", Secret: []byte(strings.Repeat("k", 32))})
	good := func() Config {
		return Config{Issuer: "https://kg.example.com/", Google: GoogleClient{ClientID: "id", ClientSecret: "s"},
			Custody: SealedCustody{}, Allow: &AllowList{Domains: []string{"example.com"}}, Keys: keys}
	}
	s, err := New(good())
	if err != nil || s.Resource() != "https://kg.example.com/mcp" {
		t.Fatalf("New = %v, %v", s, err)
	}
	for name, edit := range map[string]func(*Config){
		"http issuer":      func(c *Config) { c.Issuer = "http://kg.example.com" },
		"no google secret": func(c *Config) { c.Google.ClientSecret = "" },
		"no custody":       func(c *Config) { c.Custody = nil },
		"empty allow-list": func(c *Config) { c.Allow = &AllowList{} },
		"groups no check":  func(c *Config) { c.Allow = &AllowList{Groups: []string{"g@example.com"}} },
		"no keys":          func(c *Config) { c.Keys = nil },
		"duplicate client": func(c *Config) {
			c.Clients = []StaticClient{{ID: "a", RedirectURIs: []string{clientRedirect}}, {ID: "a", RedirectURIs: []string{clientRedirect}}}
		},
	} {
		c := good()
		edit(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

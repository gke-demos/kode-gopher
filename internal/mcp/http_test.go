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

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

const testToken = "kg-test-token-0123456789abcdef0123456789"

func TestReserveCap(t *testing.T) {
	s := New(Config{MaxSandboxesPerUser: 2})
	for i := 0; i < 2; i++ {
		if err := s.reserve("alice"); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if err := s.reserve("alice"); err == nil || !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("third reserve: err = %v, want the cap", err)
	}
	if err := s.reserve("bob"); err != nil {
		t.Fatalf("bob is capped by alice's sandboxes: %v", err)
	}
	s.unreserve("alice")
	if err := s.reserve("alice"); err != nil {
		t.Fatalf("reserve after unreserve: %v", err)
	}
	s.unreserve("bob")
	if _, ok := s.open["bob"]; ok {
		t.Error("bob's count wasn't deleted at zero")
	}
}

func TestReserveUncapped(t *testing.T) {
	s := New(Config{})
	for i := 0; i < 10; i++ {
		if err := s.reserve(""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStaticTokenVerifier(t *testing.T) {
	v := StaticTokenVerifier(testToken, "operator")
	ti, err := v(context.Background(), testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ti.UserID != "operator" || ti.Expiration.Before(time.Now()) {
		t.Errorf("TokenInfo = %+v", ti)
	}
	if _, err := v(context.Background(), testToken+"x", nil); err == nil {
		t.Error("accepted a wrong token")
	}
	if _, err := StaticTokenVerifier("", "operator")(context.Background(), "", nil); err == nil {
		t.Error("an empty configured token accepted an empty bearer")
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func newTestHTTP(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	// No reachable cluster: sandbox opens fail fast.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	cfg.KubeContext = "kg-test-missing"
	cfg.OpenTimeout = 10 * time.Second
	s := New(cfg)
	ts := httptest.NewServer(s.httpHandler(HTTPOptions{Verifier: StaticTokenVerifier(testToken, "operator")}))
	t.Cleanup(ts.Close)
	return s, ts
}

func connect(t *testing.T, url, token string) (*sdk.ClientSession, error) {
	t.Helper()
	c := sdk.NewClient(&sdk.Implementation{Name: "kg-test", Version: "0"}, nil)
	return c.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:             url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
}

func TestHTTPAuth(t *testing.T) {
	_, ts := newTestHTTP(t, Config{})

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}

	if _, err := connect(t, ts.URL, "wrong"); err == nil {
		t.Error("connected with a wrong token")
	}

	cs, err := connect(t, ts.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 {
		t.Errorf("tools = %d, want 3", len(tools.Tools))
	}

	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: status %d", resp.StatusCode)
	}
}

// Each MCP session gets its own slot, and ending the session releases it.
func TestHTTPSessionSlots(t *testing.T) {
	s, ts := newTestHTTP(t, Config{})
	ctx := context.Background()
	call := func(cs *sdk.ClientSession) {
		t.Helper()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{
			Name:      "execute_go_code",
			Arguments: map[string]any{"code": "package main\nfunc main() {}\n"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("tool call succeeded without a cluster")
		}
	}
	slots := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.slots)
	}

	a, err := connect(t, ts.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	b, err := connect(t, ts.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	call(a)
	call(a)
	call(b)
	if n := slots(); n != 2 {
		t.Fatalf("slots = %d, want one per MCP session", n)
	}
	if n := len(s.open); n != 0 {
		t.Errorf("failed opens left %d reservations", n)
	}

	a.Close()
	deadline := time.Now().Add(5 * time.Second)
	for slots() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := slots(); n != 1 {
		t.Fatalf("slots after closing one session = %d, want 1", n)
	}
	b.Close()
}

// Under OAuth, the credentials the verifier puts in TokenInfo replace
// Config.Credentials for that request.
func TestHTTPPerRequestCredentials(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	s := New(Config{KubeContext: "kg-test-missing", OpenTimeout: 10 * time.Second})
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token != testToken {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: "111", Expiration: time.Now().Add(time.Hour), Extra: map[string]any{
			creds.TokenInfoKey: &creds.OAuthUser{Token: "ya29.x", Expiry: time.Now().Add(time.Hour), Email: "alice@example.com", Project: "p1"},
		}}, nil
	}
	var routed bool
	ts := httptest.NewServer(s.httpHandler(HTTPOptions{
		Protect: func(h http.Handler) http.Handler { return auth.RequireBearerToken(verifier, nil)(h) },
		Routes:  http.HandlerFunc(func(http.ResponseWriter, *http.Request) { routed = true }),
	}))
	t.Cleanup(ts.Close)

	cs, err := connect(t, ts.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "gcp_auth_status"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	if want := "mode=oauth credential_type=authorized_user email=alice@example.com project_id=p1"; text != want {
		t.Errorf("gcp_auth_status = %q, want %q", text, want)
	}

	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !routed {
		t.Error("non-MCP path didn't reach Routes")
	}
}

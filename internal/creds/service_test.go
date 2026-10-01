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

package creds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIAM serves generateAccessToken for allowed accounts.
func fakeIAM(t *testing.T, allowed string, life time.Duration) (*Impersonator, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		email, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/"), ":generateAccessToken")
		var body struct {
			Scope []string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPost || !ok || len(body.Scope) != 1 || body.Scope[0] != cloudPlatformScope {
			http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		if email != allowed {
			http.Error(w, `{"error":{"message":"Permission 'iam.serviceAccounts.getAccessToken' denied"}}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": fmt.Sprintf("ya29.sa-%d", n),
			"expireTime":  time.Now().Add(life).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return &Impersonator{Client: srv.Client(), Endpoint: srv.URL}, &calls
}

func TestService(t *testing.T) {
	const sa = "runner@proj-1.iam.gserviceaccount.com"
	imp, calls := fakeIAM(t, sa, time.Hour)
	s, err := NewService(imp, sa, "", "quota-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tok, err := s.AccessToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ya29.sa-1" || tok.Email != sa || tok.Project != "proj-1" || tok.QuotaProject != "quota-1" || time.Until(tok.Expiry) < 50*time.Minute {
		t.Errorf("AccessToken = %+v", tok)
	}
	if _, err := s.AccessToken(ctx); err != nil || calls.Load() != 1 {
		t.Errorf("second AccessToken: err %v, %d mints; want the cached token", err, calls.Load())
	}
	id, _ := s.Identity(ctx)
	if id != (Identity{Mode: "service", CredType: "service_account", Email: sa, ProjectID: "proj-1"}) {
		t.Errorf("Identity = %+v", id)
	}
	if _, ok := Source(s).(TokenMinter); !ok {
		t.Error("Service isn't a TokenMinter")
	}
}

func TestImpersonatorRemints(t *testing.T) {
	const sa = "runner@proj-1.iam.gserviceaccount.com"
	imp, calls := fakeIAM(t, sa, 2*time.Minute) // under minTokenLife
	for range 2 {
		if _, err := imp.Token(context.Background(), sa); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("%d mints for a token about to expire, want 2", calls.Load())
	}
}

func TestImpersonatorDenied(t *testing.T) {
	imp, _ := fakeIAM(t, "runner@proj-1.iam.gserviceaccount.com", time.Hour)
	_, err := imp.Token(context.Background(), "other@proj-1.iam.gserviceaccount.com")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "getAccessToken") {
		t.Errorf("err = %v, want the 403 and its message", err)
	}
}

func TestServiceAccountProject(t *testing.T) {
	for email, want := range map[string]string{
		"a@p-1.iam.gserviceaccount.com":             "p-1",
		"123-compute@developer.gserviceaccount.com": "",
		"user@example.com":                          "",
	} {
		if got := ServiceAccountProject(email); got != want {
			t.Errorf("ServiceAccountProject(%q) = %q, want %q", email, got, want)
		}
	}
	if _, err := NewService(nil, "not-an-email", "", ""); err == nil {
		t.Error("NewService accepted a non-email")
	}
}

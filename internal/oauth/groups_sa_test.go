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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

// TestGroupsAsServiceAccount: with --oauth-groups-service-account, the
// group check calls Cloud Identity with the impersonated account's
// token, minted with the read-only groups scope only, once, and reused
// (the composition cmd/kode-gopher's groupsClient builds).
func TestGroupsAsServiceAccount(t *testing.T) {
	const sa = "kode-gopher-groups@proj.iam.gserviceaccount.com"
	var mints atomic.Int32
	const groupsScope = "https://www.googleapis.com/auth/cloud-identity.groups.readonly"
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/"), ":generateAccessToken")
		var body struct {
			Scope []string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Scope) != 1 || body.Scope[0] != groupsScope {
			t.Errorf("minted with scopes %v, want only %s", body.Scope, groupsScope)
		}
		if email != sa {
			// kode-gopher has no token-creator grant on this account.
			http.Error(w, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.getAccessToken' denied"}}`, http.StatusForbidden)
			return
		}
		mints.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{
			"accessToken": "tok-for-" + email,
			"expireTime":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer iam.Close()
	ci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the groups account holds Groups Reader.
		if r.Header.Get("Authorization") != "Bearer tok-for-"+sa {
			http.Error(w, `{"error":{"code":403,"message":"caller lacks Groups Reader"}}`, http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/groups:lookup":
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/abc"})
		case "/groups/abc/memberships:checkTransitiveMembership":
			writeJSON(w, http.StatusOK, map[string]bool{"hasMembership": strings.Contains(r.URL.RawQuery, "in%40example.com")})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ci.Close()

	imp := &creds.Impersonator{Client: iam.Client(), Endpoint: iam.URL}
	ctx := context.Background()
	client := func(email string) *http.Client {
		return &http.Client{Transport: &oauth2.Transport{Source: imp.TokenSource(email, groupsScope)}}
	}
	groups := &CloudIdentityGroups{Client: client(sa), Endpoint: ci.URL + "/"}
	for member, want := range map[string]bool{"in@example.com": true, "out@example.com": false} {
		got, err := groups.IsMember(ctx, "g@example.com", member)
		if err != nil || got != want {
			t.Errorf("IsMember(%s) = %v, %v; want %v", member, got, err, want)
		}
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("minted %d tokens for three calls, want 1 (cached)", n)
	}

	// An account kode-gopher can't impersonate: the refused mint reaches
	// the caller as an error naming it, not as "not a member".
	other := &CloudIdentityGroups{Client: client("other@proj.iam.gserviceaccount.com"), Endpoint: ci.URL + "/"}
	ok, err := other.IsMember(ctx, "g@example.com", "in@example.com")
	if err == nil || ok || !strings.Contains(err.Error(), "impersonate") {
		t.Errorf("IsMember with a refused mint = %v, %v; want an impersonate error", ok, err)
	}
}

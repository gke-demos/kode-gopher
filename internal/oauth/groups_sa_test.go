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
// token, minted once and reused (the composition cmd/kode-gopher's
// groupsClient builds).
func TestGroupsAsServiceAccount(t *testing.T) {
	const sa = "kode-gopher-groups@proj.iam.gserviceaccount.com"
	var mints atomic.Int32
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/"), ":generateAccessToken")
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
		switch {
		case r.URL.Path == "/groups:lookup":
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/abc"})
		case r.URL.Path == "/groups/abc/memberships:checkTransitiveMembership":
			writeJSON(w, http.StatusOK, map[string]bool{"hasMembership": strings.Contains(r.URL.RawQuery, "in%40example.com")})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ci.Close()

	imp := &creds.Impersonator{Client: iam.Client(), Endpoint: iam.URL}
	ctx := context.Background()
	groups := &CloudIdentityGroups{Client: oauth2.NewClient(ctx, imp.TokenSource(sa)), Endpoint: ci.URL + "/"}
	for member, want := range map[string]bool{"in@example.com": true, "out@example.com": false} {
		got, err := groups.IsMember(ctx, "g@example.com", member)
		if err != nil || got != want {
			t.Errorf("IsMember(%s) = %v, %v; want %v", member, got, err, want)
		}
	}
	if n := mints.Load(); n != 1 {
		t.Errorf("minted %d tokens for three calls, want 1 (cached)", n)
	}

	// Another account (no Groups Reader) is refused, and the error
	// reaches the caller rather than reading as "not a member".
	other := &CloudIdentityGroups{Client: oauth2.NewClient(ctx, imp.TokenSource("other@proj.iam.gserviceaccount.com")), Endpoint: ci.URL + "/"}
	if ok, err := other.IsMember(ctx, "g@example.com", "in@example.com"); err == nil || ok {
		t.Errorf("IsMember as another account = %v, %v; want a 403 error", ok, err)
	}
}

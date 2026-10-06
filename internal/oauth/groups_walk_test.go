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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeStarterDirectory is Cloud Identity as a Business Starter customer
// sees it: checkTransitiveMembership is 403, groups:lookup and
// memberships.list work. groups maps a group email to its members
// ("group:" prefix for a nested group); deny lists groups the caller
// can't read.
func fakeStarterDirectory(t *testing.T, groups map[string][]string, deny map[string]bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var transitiveCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/groups:lookup":
			email := r.URL.Query().Get("groupKey.id")
			if _, ok := groups[email]; !ok {
				http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/" + email})
		case strings.HasSuffix(r.URL.Path, ":checkTransitiveMembership"):
			transitiveCalls.Add(1)
			http.Error(w, `{"error":{"code":403,"message":"edition"}}`, http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/memberships"):
			email := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/groups/"), "/memberships")
			if deny[email] {
				http.Error(w, `{"error":{"code":403}}`, http.StatusForbidden)
				return
			}
			if r.URL.Query().Get("view") != "FULL" {
				t.Errorf("memberships.list view = %q, want FULL", r.URL.Query().Get("view"))
			}
			var ms []map[string]any
			for _, m := range groups[email] {
				typ, id := "USER", m
				if g, ok := strings.CutPrefix(m, "group:"); ok {
					typ, id = "GROUP", g
				}
				ms = append(ms, map[string]any{"preferredMemberKey": map[string]string{"id": id}, "type": typ})
			}
			// Two pages, to exercise pageToken.
			if r.URL.Query().Get("pageToken") == "" && len(ms) > 1 {
				writeJSON(w, http.StatusOK, map[string]any{"memberships": ms[:1], "nextPageToken": "p2"})
				return
			}
			if r.URL.Query().Get("pageToken") == "p2" {
				ms = ms[1:]
			}
			writeJSON(w, http.StatusOK, map[string]any{"memberships": ms})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &transitiveCalls
}

func TestGroupsWalkWithoutTransitive(t *testing.T) {
	srv, transitive := fakeStarterDirectory(t, map[string][]string{
		"allowed@d.example": {"direct@d.example", "group:nested@d.example"},
		// nested points back at allowed: a cycle the walk must survive.
		"nested@d.example": {"group:allowed@d.example", "In@Other.Example"},
	}, nil)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	for member, want := range map[string]bool{
		"direct@d.example": true,
		"in@other.example": true, // through the nested group, other case
		"out@d.example":    false,
	} {
		got, err := c.IsMember(ctx, "allowed@d.example", member)
		if err != nil || got != want {
			t.Errorf("IsMember(%s) = %v, %v; want %v", member, got, err, want)
		}
	}
	if n := transitive.Load(); n != 1 {
		t.Errorf("checkTransitiveMembership called %d times, want 1 (then walk only)", n)
	}
}

func TestGroupsWalkUnreadableNested(t *testing.T) {
	srv, _ := fakeStarterDirectory(t, map[string][]string{
		"allowed@d.example": {"group:secret@d.example"},
		"secret@d.example":  {"in@d.example"},
	}, map[string]bool{"secret@d.example": true})
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	if ok, err := c.IsMember(context.Background(), "allowed@d.example", "in@d.example"); err == nil || ok {
		t.Errorf("IsMember with an unreadable nested group = %v, %v; want an error, not a no", ok, err)
	}
}

func TestGroupsWalkDepthLimit(t *testing.T) {
	groups := map[string][]string{}
	for i := 0; i <= walkMaxDepth; i++ {
		groups[fmt.Sprintf("g%d@d.example", i)] = []string{fmt.Sprintf("group:g%d@d.example", i+1)}
	}
	groups[fmt.Sprintf("g%d@d.example", walkMaxDepth+1)] = []string{"deep@d.example"}
	srv, _ := fakeStarterDirectory(t, groups, nil)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	_, err := c.IsMember(context.Background(), "g0@d.example", "deep@d.example")
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("IsMember past the depth limit: err = %v, want a depth error", err)
	}
}

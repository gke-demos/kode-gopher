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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStarterDirectory is Cloud Identity as a Business Starter customer
// sees it: checkTransitiveMembership is 403, groups:lookup and
// memberships.list work. groups maps a group email to its members
// ("group:" prefix for a nested group); deny lists groups the caller
// can't read.
func fakeStarterDirectory(t *testing.T, groups map[string][]string, deny map[string]bool) (*httptest.Server, *atomic.Int32) {
	srv, transitive, _ := fakeDirectory(t, groups, deny, http.StatusForbidden)
	return srv, transitive
}

// fakeDirectory is fakeStarterDirectory with the transitive call's
// status chosen, and a count of memberships.list calls. Members may be
// "group:<email>" (nested group), "ns:<email>" (a group from an external
// identity source) or "other:<id>" (a non-user membership).
func fakeDirectory(t *testing.T, groups map[string][]string, deny map[string]bool, transitiveStatus int) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var transitiveCalls, listCalls atomic.Int32
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
			http.Error(w, `{"error":{"message":"transitive unavailable"}}`, transitiveStatus)
		case strings.HasSuffix(r.URL.Path, "/memberships"):
			listCalls.Add(1)
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
				typ, id, ns := "USER", m, ""
				if g, ok := strings.CutPrefix(m, "group:"); ok {
					typ, id = "GROUP", g
				}
				if g, ok := strings.CutPrefix(m, "ns:"); ok {
					typ, id, ns = "GROUP", g, "identitysources/abc"
				}
				if g, ok := strings.CutPrefix(m, "other:"); ok {
					typ, id = "OTHER", g
				}
				ms = append(ms, map[string]any{"preferredMemberKey": map[string]string{"id": id, "namespace": ns}, "type": typ})
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
	return srv, &transitiveCalls, &listCalls
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

// A nested group that can't be resolved, listed before a direct member,
// mustn't lock the direct member out; a non-member gets an error (the
// walk was incomplete), not a "no".
func TestGroupsWalkBrokenNestedBeforeDirect(t *testing.T) {
	srv, _ := fakeStarterDirectory(t, map[string][]string{
		// gone@ isn't a group the directory knows: lookup 404s.
		"allowed@d.example": {"group:gone@d.example", "direct@d.example"},
	}, nil)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	if ok, err := c.IsMember(ctx, "allowed@d.example", "direct@d.example"); err != nil || !ok {
		t.Errorf("direct member after a broken nested group = %v, %v; want true", ok, err)
	}
	ok, err := c.IsMember(ctx, "allowed@d.example", "out@d.example")
	if err == nil || ok || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("non-member with a broken nested group = %v, %v; want an incomplete-walk error", ok, err)
	}
}

func TestGroupsWalkGroupLimit(t *testing.T) {
	groups := map[string][]string{}
	var root []string
	for i := 0; i < walkMaxGroups+5; i++ {
		g := fmt.Sprintf("n%d@d.example", i)
		groups[g] = nil
		root = append(root, "group:"+g)
	}
	groups["allowed@d.example"] = append(root, "direct@d.example")
	srv, _ := fakeStarterDirectory(t, groups, nil)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	if ok, err := c.IsMember(ctx, "allowed@d.example", "direct@d.example"); err != nil || !ok {
		t.Errorf("direct member of a group past the limit = %v, %v; want true", ok, err)
	}
	if _, err := c.IsMember(ctx, "allowed@d.example", "out@d.example"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("non-member past the group limit: err = %v, want a limit error", err)
	}
}

// Only a 403 means "not this edition": a 500 is reported, and the
// transitive call is tried again next time.
func TestGroupsTransitiveServerErrorNoFallback(t *testing.T) {
	srv, transitive, lists := fakeDirectory(t, map[string][]string{"allowed@d.example": {"in@d.example"}}, nil, http.StatusInternalServerError)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	for i := 0; i < 2; i++ {
		if _, err := c.IsMember(context.Background(), "allowed@d.example", "in@d.example"); err == nil {
			t.Errorf("call %d: no error from a 500", i)
		}
	}
	if transitive.Load() != 2 || lists.Load() != 0 {
		t.Errorf("transitive calls %d, list calls %d; want 2 and 0 (no fallback on a 500)", transitive.Load(), lists.Load())
	}
}

// One walk serves every user: membership lists are cached.
func TestGroupsWalkCachesLists(t *testing.T) {
	srv, _, lists := fakeDirectory(t, map[string][]string{
		"allowed@d.example": {"group:nested@d.example"},
		"nested@d.example":  {"a@d.example", "b@d.example"},
	}, nil, http.StatusForbidden)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	for _, m := range []string{"a@d.example", "b@d.example", "out@d.example"} {
		if _, err := c.IsMember(context.Background(), "allowed@d.example", m); err != nil {
			t.Fatalf("IsMember(%s): %v", m, err)
		}
	}
	// Two groups, two pages each for the one with two members: 3 calls.
	if n := lists.Load(); n != 3 {
		t.Errorf("memberships.list called %d times for three users, want 3 (cached after the first walk)", n)
	}
}

// Non-user memberships never match, and a nested group from an external
// identity source can't be walked: an error for a non-member, not a no.
func TestGroupsWalkMemberTypes(t *testing.T) {
	srv, _ := fakeStarterDirectory(t, map[string][]string{
		"allowed@d.example": {"other:out@d.example", "ns:ext@d.example"},
	}, nil)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ok, err := c.IsMember(context.Background(), "allowed@d.example", "out@d.example")
	if ok || err == nil || !strings.Contains(err.Error(), "external identity source") {
		t.Errorf("IsMember = %v, %v; want no match (OTHER) and an external-source error", ok, err)
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

// fakeEnterpriseDirectory is Cloud Identity for an edition with
// checkTransitiveMembership. It answers that call honestly from the
// query, reading the quoted member key as a string literal would
// (a backslash escapes the next character), and lists members on one
// page. It counts transitive calls.
func fakeEnterpriseDirectory(t *testing.T, members []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var transitiveCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/groups:lookup":
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/g"})
		case strings.HasSuffix(r.URL.Path, ":checkTransitiveMembership"):
			transitiveCalls.Add(1)
			q, ok := strings.CutPrefix(r.URL.Query().Get("query"), "member_key_id == '")
			q, ok2 := strings.CutSuffix(q, "'")
			if !ok || !ok2 {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			var id strings.Builder
			for i := 0; i < len(q); i++ {
				if q[i] == '\\' && i+1 < len(q) {
					i++
				}
				id.WriteByte(q[i])
			}
			writeJSON(w, http.StatusOK, map[string]bool{"hasMembership": slices.Contains(members, id.String())})
		default:
			var ms []map[string]any
			for _, m := range members {
				ms = append(ms, map[string]any{"preferredMemberKey": map[string]string{"id": m}, "type": "USER"})
			}
			writeJSON(w, http.StatusOK, map[string]any{"memberships": ms})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &transitiveCalls
}

// The transitive call quotes the member in its query, so a member with a
// quote or backslash is matched exactly by the walk instead: o'brien is
// not obrien, and back\slash is not backslash.
func TestGroupsQuotedMemberWalks(t *testing.T) {
	srv, transitive := fakeEnterpriseDirectory(t, []string{"obrien@d.example", "backslash@d.example", `list\ed@d.example`})
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	for member, want := range map[string]bool{
		"o'brien@d.example":    false,
		`back\slash@d.example`: false,
		`list\ed@d.example`:    true,
	} {
		if got, err := c.IsMember(ctx, "g@d.example", member); err != nil || got != want {
			t.Errorf("IsMember(%s) = %v, %v; want %v", member, got, err, want)
		}
	}
	if n := transitive.Load(); n != 0 {
		t.Errorf("checkTransitiveMembership asked %d times about quoted members, want 0", n)
	}
	// An ordinary address still takes the transitive call.
	if got, err := c.IsMember(ctx, "g@d.example", "obrien@d.example"); err != nil || !got || transitive.Load() != 1 {
		t.Errorf("IsMember(obrien) = %v, %v after %d transitive calls; want true after 1", got, err, transitive.Load())
	}
}

// A cached group name may be stale: a 404 from the transitive call on a
// cached name looks the group up again before it means "not a member".
func TestGroupsStaleNameRelookup(t *testing.T) {
	var mu sync.Mutex
	current := "groups/old"
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur := current
		mu.Unlock()
		switch r.URL.Path {
		case "/groups:lookup":
			lookups.Add(1)
			writeJSON(w, http.StatusOK, map[string]string{"name": cur})
		case "/" + cur + "/memberships:checkTransitiveMembership":
			if strings.Contains(r.URL.Query().Get("query"), "in@d.example") {
				writeJSON(w, http.StatusOK, map[string]bool{"hasMembership": true})
				return
			}
			http.Error(w, "no such member", http.StatusNotFound)
		default:
			http.Error(w, "no such group", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
		t.Fatalf("first check = %v, %v; want true", ok, err)
	}
	// The group is deleted and recreated under a new name.
	mu.Lock()
	current = "groups/new"
	mu.Unlock()
	if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
		t.Errorf("member after the group was recreated = %v, %v; want true via a fresh lookup", ok, err)
	}
	// A 404 with a cached name costs one lookup, then the 404 on the
	// fresh name is about the member: not a member, and no loop.
	before := lookups.Load()
	if ok, err := c.IsMember(ctx, "g@d.example", "ghost@d.example"); err != nil || ok {
		t.Errorf("unknown member = %v, %v; want false", ok, err)
	}
	if n := lookups.Load() - before; n != 1 {
		t.Errorf("unknown member cost %d group lookups, want 1", n)
	}
}

// A group whose pages never end is an incomplete walk, not an endless
// loop. The fake stops after ten times the limit so code without one
// returns (with a "no") instead of hanging.
func TestGroupsWalkPageLimit(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/groups:lookup":
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/g"})
		case strings.HasSuffix(r.URL.Path, ":checkTransitiveMembership"):
			http.Error(w, "edition", http.StatusForbidden)
		default:
			if pages.Add(1) >= 10*walkMaxPages {
				writeJSON(w, http.StatusOK, map[string]any{"memberships": []any{}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"memberships": []any{}, "nextPageToken": "again"})
		}
	}))
	t.Cleanup(srv.Close)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ok, err := c.IsMember(context.Background(), "g@d.example", "in@d.example")
	if err == nil || ok || !strings.Contains(err.Error(), "pages") || pages.Load() != walkMaxPages {
		t.Errorf("endless pages = %v, %v after %d pages; want a page-limit error after %d", ok, err, pages.Load(), walkMaxPages)
	}
}

// recreatableDirectory is Cloud Identity holding one group,
// g@d.example, whose resource name the test can change (the group deleted
// and recreated). Calls about any other name are answered goneStatus: 403
// "Permission denied ... (or it may not exist)" or 404, whichever the
// test asks for. transitive says whether this edition has
// checkTransitiveMembership (else it is 403 for every name); listStatus,
// when set, is memberships.list's answer for the current name too (a
// group that can't be read). The members are in@d.example and
// o\'brien@d.example.
type recreatableDirectory struct {
	mu         sync.Mutex
	current    string
	goneStatus int
	transitive bool
	listStatus int

	lookups, transitiveCalls, lists atomic.Int32
}

func (d *recreatableDirectory) recreate(name string) {
	d.mu.Lock()
	d.current = name
	d.mu.Unlock()
}

func (d *recreatableDirectory) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		cur, gone, transitive, listStatus := d.current, d.goneStatus, d.transitive, d.listStatus
		d.mu.Unlock()
		if r.URL.Path == "/groups:lookup" {
			d.lookups.Add(1)
			writeJSON(w, http.StatusOK, map[string]string{"name": cur})
			return
		}
		name, call, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/memberships")
		isTransitive := call == ":checkTransitiveMembership"
		if isTransitive {
			d.transitiveCalls.Add(1)
		} else {
			d.lists.Add(1)
		}
		switch {
		case isTransitive && !transitive:
			http.Error(w, `{"error":{"code":403,"message":"edition"}}`, http.StatusForbidden)
		case name != cur:
			http.Error(w, fmt.Sprintf(`{"error":{"code":%d,"message":"Error(2028): Permission denied for resource %s (or it may not exist)."}}`, gone, name), gone)
		case isTransitive:
			writeJSON(w, http.StatusOK, map[string]bool{"hasMembership": strings.Contains(r.URL.Query().Get("query"), "'in@d.example'")})
		case listStatus != 0:
			http.Error(w, fmt.Sprintf(`{"error":{"code":%d}}`, listStatus), listStatus)
		default:
			writeJSON(w, http.StatusOK, map[string]any{"memberships": []any{
				map[string]any{"preferredMemberKey": map[string]string{"id": "in@d.example"}, "type": "USER"},
				map[string]any{"preferredMemberKey": map[string]string{"id": `o\'brien@d.example`}, "type": "USER"},
			}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// expireLists ages c's cached membership lists past allowCacheTTL, as if
// the time had passed, leaving the cached group names (an hour) as they
// are.
func expireLists(c *CloudIdentityGroups) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, e := range c.lists {
		e.at = e.at.Add(-allowCacheTTL - time.Second)
		c.lists[name] = e
	}
}

// A group recreated within the hour, on an edition with the transitive
// call, where Cloud Identity answers the old name with 403 rather than
// 404: the walk's 403 on the old name looks the group up again, and the
// new name keeps the transitive call.
func TestGroupsStaleNameTransitive403(t *testing.T) {
	d := &recreatableDirectory{current: "groups/old", goneStatus: http.StatusForbidden, transitive: true}
	srv := d.server(t)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
		t.Fatalf("first check = %v, %v", ok, err)
	}
	d.recreate("groups/new")
	if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
		t.Errorf("member after the group was recreated = %v, %v; want true via a fresh lookup", ok, err)
	}
	if n := d.lookups.Load(); n != 2 {
		t.Errorf("group looked up %d times, want 2 (one extra for the stale name)", n)
	}
	// The new name kept the transitive call: no walk of it.
	lists := d.lists.Load()
	if ok, err := c.IsMember(ctx, "g@d.example", "out@d.example"); err != nil || ok {
		t.Errorf("non-member = %v, %v; want false", ok, err)
	}
	if n := d.lists.Load() - lists; n != 0 {
		t.Errorf("new name walked (%d list calls) instead of asked transitively", n)
	}
	if n := d.lookups.Load(); n != 2 {
		t.Errorf("group looked up %d times, want 2 (new name cached)", n)
	}
}

// A group recreated within the hour on the walk path (an edition without
// the transitive call, or a member address the walk must match), with the
// old name answering 404 or 403: one fresh lookup, and the member is in.
func TestGroupsStaleNameWalk(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gone       int
		transitive bool
		member     string
	}{
		{"edition 404", http.StatusNotFound, false, "in@d.example"},
		{"edition 403", http.StatusForbidden, false, "in@d.example"},
		{"quoted member 404", http.StatusNotFound, true, `o\'brien@d.example`},
		{"quoted member 403", http.StatusForbidden, true, `o\'brien@d.example`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &recreatableDirectory{current: "groups/old", goneStatus: tc.gone, transitive: tc.transitive}
			srv := d.server(t)
			c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
			ctx := context.Background()
			if ok, err := c.IsMember(ctx, "g@d.example", tc.member); err != nil || !ok {
				t.Fatalf("first check = %v, %v", ok, err)
			}
			d.recreate("groups/new")
			// Past the membership-list cache, within the name cache.
			expireLists(c)
			if ok, err := c.IsMember(ctx, "g@d.example", tc.member); err != nil || !ok {
				t.Errorf("member after the group was recreated = %v, %v; want true via a fresh lookup", ok, err)
			}
			if n := d.lookups.Load(); n != 2 {
				t.Errorf("group looked up %d times, want 2 (one extra for the stale name)", n)
			}
		})
	}
}

// The edition fallback still works with a fresh name: the transitive
// call's 403 is "not this edition", the group is walked, and nothing is
// looked up again.
func TestGroupsEditionFallbackFreshName(t *testing.T) {
	d := &recreatableDirectory{current: "groups/g", goneStatus: http.StatusNotFound}
	srv := d.server(t)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	for _, tc := range []struct {
		member string
		want   bool
	}{{"in@d.example", true}, {"out@d.example", false}} {
		if ok, err := c.IsMember(ctx, "g@d.example", tc.member); err != nil || ok != tc.want {
			t.Errorf("IsMember(%s) = %v, %v; want %v", tc.member, ok, err, tc.want)
		}
	}
	if l, tr, ls := d.lookups.Load(), d.transitiveCalls.Load(), d.lists.Load(); l != 1 || tr != 1 || ls != 1 {
		t.Errorf("lookups %d, transitive calls %d, list calls %d; want 1, 1, 1 (fresh name: fallback, no re-lookup)", l, tr, ls)
	}
}

// A group that can't be listed (no rights, or a recreated group not yet
// readable) costs at most one extra lookup per check, never a loop, and
// stays an error rather than a "no". A fresh name that can't be listed
// costs no extra lookup at all.
func TestGroupsStaleNameAtMostOneExtraLookup(t *testing.T) {
	d := &recreatableDirectory{current: "groups/old", goneStatus: http.StatusForbidden}
	srv := d.server(t)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
		t.Fatalf("first check = %v, %v", ok, err)
	}
	d.mu.Lock()
	d.listStatus = http.StatusForbidden
	d.current = "groups/new"
	d.mu.Unlock()
	expireLists(c)
	for i := 0; i < 3; i++ {
		before := d.lookups.Load()
		if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); ok || err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("check %d on an unlistable group = %v, %v; want an HTTP 403 error", i, ok, err)
		}
		if n := d.lookups.Load() - before; n != 1 {
			t.Errorf("check %d cost %d group lookups, want 1", i, n)
		}
	}
	fresh := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	before := d.lookups.Load()
	if ok, err := fresh.IsMember(ctx, "g@d.example", "in@d.example"); err == nil || ok {
		t.Errorf("fresh name on an unlistable group = %v, %v; want an error", ok, err)
	}
	if n := d.lookups.Load() - before; n != 1 {
		t.Errorf("fresh name on an unlistable group cost %d lookups, want 1", n)
	}
}

// Google failing is not a stale name: a 5xx or 429 from listing the
// group under a cached name is an error, with no fresh lookup and the
// name kept.
func TestGroupsWalkOutageNotStale(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			d := &recreatableDirectory{current: "groups/g", goneStatus: http.StatusNotFound}
			srv := d.server(t)
			c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
			ctx := context.Background()
			if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
				t.Fatalf("first check = %v, %v", ok, err)
			}
			d.mu.Lock()
			d.listStatus = status
			d.mu.Unlock()
			expireLists(c)
			if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err == nil || ok {
				t.Errorf("check during an outage = %v, %v; want an error", ok, err)
			}
			if n := d.lookups.Load(); n != 1 {
				t.Errorf("an outage cost %d group lookups, want 1 (the name kept)", n)
			}
			if _, ok := c.names["g@d.example"]; !ok {
				t.Error("an outage evicted the cached name")
			}
		})
	}
}

// Concurrent checks that all read the stale name each cost at most one
// extra lookup.
func TestGroupsStaleNameConcurrent(t *testing.T) {
	d := &recreatableDirectory{current: "groups/old", goneStatus: http.StatusForbidden, transitive: true}
	srv := d.server(t)
	c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
	ctx := context.Background()
	if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
		t.Fatalf("first check = %v, %v", ok, err)
	}
	d.recreate("groups/new")
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := c.IsMember(ctx, "g@d.example", "in@d.example"); err != nil || !ok {
				t.Errorf("concurrent check after recreation = %v, %v; want true", ok, err)
			}
		}()
	}
	wg.Wait()
	if got := d.lookups.Load() - 1; got < 1 || got > n {
		t.Errorf("%d concurrent checks cost %d lookups, want 1..%d", n, got, n)
	}
}

// forgetName leaves a name that a concurrent check has already refreshed.
func TestGroupsForgetNameKeepsRefreshed(t *testing.T) {
	c := &CloudIdentityGroups{names: map[string]cached[string]{"g@d.example": {v: "groups/new"}}}
	c.forgetName("g@d.example", "groups/old")
	if e, ok := c.names["g@d.example"]; !ok || e.v != "groups/new" {
		t.Errorf("forgetName of a stale name evicted the fresh one: %v, %v", e, ok)
	}
	c.forgetName("g@d.example", "groups/new")
	if _, ok := c.names["g@d.example"]; ok {
		t.Error("forgetName left the name it was asked to drop")
	}
}

// A retry that reads a name another check has just cached is answered
// as is: no second eviction and no lookup of its own, so a check never
// costs more than one extra lookup however checks interleave. Here the
// other check's name is planted while the stale one is being listed,
// and both names answer 403.
func TestGroupsStaleNameRetryReadsRefreshedName(t *testing.T) {
	var lookups atomic.Int32
	c := &CloudIdentityGroups{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/groups:lookup":
			lookups.Add(1)
			writeJSON(w, http.StatusOK, map[string]string{"name": "groups/looked-up"})
		case strings.HasSuffix(r.URL.Path, ":checkTransitiveMembership"):
			http.Error(w, `{"error":{"code":403,"message":"edition"}}`, http.StatusForbidden)
		default:
			if r.URL.Path == "/groups/old/memberships" {
				c.mu.Lock()
				c.names["g@d.example"] = cached[string]{v: "groups/other", at: time.Now()}
				c.mu.Unlock()
			}
			http.Error(w, `{"error":{"code":403,"message":"Permission denied (or it may not exist)."}}`, http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	c.Client, c.Endpoint = srv.Client(), srv.URL+"/"
	c.names = map[string]cached[string]{"g@d.example": {v: "groups/old", at: time.Now()}}
	ok, err := c.IsMember(context.Background(), "g@d.example", "in@d.example")
	if ok || err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("IsMember = %v, %v; want an HTTP 403 error", ok, err)
	}
	if n := lookups.Load(); n != 0 {
		t.Errorf("a retry on a name another check cached cost %d lookups, want 0", n)
	}
	if e := c.names["g@d.example"]; e.v != "groups/other" {
		t.Errorf("cached name = %q, want the other check's groups/other kept", e.v)
	}
}

// Only the group being checked can have a stale name: a nested group's
// 403 or 404 leaves the root's cached name alone and makes the walk
// incomplete, for every later check too.
func TestGroupsNestedGoneNotStaleRoot(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var lookups atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/groups:lookup":
					lookups.Add(1)
					writeJSON(w, http.StatusOK, map[string]string{"name": "groups/" + r.URL.Query().Get("groupKey.id")})
				case strings.HasSuffix(r.URL.Path, ":checkTransitiveMembership"):
					http.Error(w, `{"error":{"code":403,"message":"edition"}}`, http.StatusForbidden)
				case r.URL.Path == "/groups/g@d.example/memberships":
					writeJSON(w, http.StatusOK, map[string]any{"memberships": []any{
						map[string]any{"preferredMemberKey": map[string]string{"id": "nested@d.example"}, "type": "GROUP"},
					}})
				default:
					http.Error(w, fmt.Sprintf(`{"error":{"code":%d}}`, status), status)
				}
			}))
			t.Cleanup(srv.Close)
			c := &CloudIdentityGroups{Client: srv.Client(), Endpoint: srv.URL + "/"}
			for i := 0; i < 2; i++ {
				ok, err := c.IsMember(context.Background(), "g@d.example", "out@d.example")
				if ok || err == nil || !strings.Contains(err.Error(), "incomplete") {
					t.Errorf("check %d = %v, %v; want an incomplete-walk error", i, ok, err)
				}
			}
			// The root and the nested group, once each.
			if n := lookups.Load(); n != 2 {
				t.Errorf("group lookups = %d, want 2 (root name kept)", n)
			}
		})
	}
}

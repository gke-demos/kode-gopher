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
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// allowCacheTTL is how long an allow-list decision is reused. Removing
// someone from a group takes effect within this plus one access-token
// lifetime (15 min): about 20 minutes.
const allowCacheTTL = 5 * time.Minute

// AllowList admits users by Workspace domain or Google group. Any match
// admits; an empty list admits no one.
type AllowList struct {
	// Domains match the ID token's hd claim, never the email suffix.
	Domains []string
	// Groups are group email addresses. Membership is transitive.
	Groups []string
	// Groups checks group membership. Required when Groups is set.
	GroupChecker GroupChecker

	mu    sync.Mutex
	cache map[string]allowEntry
}

type allowEntry struct {
	err error
	at  time.Time
}

// errNotAllowed is a user outside the allow-list.
var errNotAllowed = errors.New("this account isn't allowed to use this kode-gopher server")

// admit returns nil if u may sign in, errNotAllowed if not, or another
// error if the check itself failed (not cached).
func (a *AllowList) admit(ctx context.Context, u User) error {
	if u.Domain != "" && slices.Contains(a.Domains, strings.ToLower(u.Domain)) {
		return nil
	}
	if len(a.Groups) == 0 {
		return errNotAllowed
	}
	a.mu.Lock()
	e, ok := a.cache[u.Sub]
	a.mu.Unlock()
	if ok && time.Since(e.at) < allowCacheTTL {
		return e.err
	}
	err := errNotAllowed
	for _, g := range a.Groups {
		in, cErr := a.GroupChecker.IsMember(ctx, g, u.Email)
		if cErr != nil {
			return fmt.Errorf("check group %s: %w", g, cErr)
		}
		if in {
			err = nil
			break
		}
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]allowEntry{}
	}
	a.cache[u.Sub] = allowEntry{err: err, at: time.Now()}
	a.mu.Unlock()
	return err
}

// GroupChecker answers whether member is in group, directly or through
// nested groups.
type GroupChecker interface {
	IsMember(ctx context.Context, group, member string) (bool, error)
}

// CloudIdentityGroups checks membership with the Cloud Identity Groups
// API, as kode-gopher's own identity or a service account it
// impersonates (either needs the Groups Reader admin role). Users don't
// consent to any groups scope.
//
// checkTransitiveMembership answers in one call, but only for Workspace
// Enterprise and Cloud Identity Premium accounts; other editions
// (Business Starter, Standard, Plus) get 403. Then IsMember walks the
// group and its nested groups with memberships.list, which every
// edition has, and skips the transitive call for that group for an
// hour. Membership lists are cached for allowCacheTTL, so one walk
// serves every user checked against the same groups.
type CloudIdentityGroups struct {
	// Client is authenticated with the
	// cloud-identity.groups.readonly scope.
	Client *http.Client
	// Endpoint defaults to https://cloudidentity.googleapis.com/v1/.
	Endpoint string

	mu           sync.Mutex
	names        map[string]cached[string]       // group email -> groups/<id>
	lists        map[string]cached[[]membership] // groups/<id> -> direct members
	noTransitive map[string]time.Time            // groups/<id> -> when the transitive call got 403
}

type cached[T any] struct {
	v  T
	at time.Time
}

const (
	// walkMaxDepth and walkMaxGroups bound a membership walk against
	// runaway nesting.
	walkMaxDepth  = 10
	walkMaxGroups = 100
	// groupNameTTL bounds how long a group email -> name mapping is
	// trusted (a group deleted and recreated gets a new name).
	groupNameTTL = time.Hour
	// transitiveRetry is how long a group's 403 from
	// checkTransitiveMembership is remembered before trying it again.
	transitiveRetry = time.Hour
)

// IsMember implements GroupChecker.
func (c *CloudIdentityGroups) IsMember(ctx context.Context, group, member string) (bool, error) {
	name, err := c.groupName(ctx, group)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	at, walk := c.noTransitive[name]
	walk = walk && time.Since(at) < transitiveRetry
	c.mu.Unlock()
	if !walk {
		in, err := c.transitive(ctx, name, member)
		var se *statusError
		if !errors.As(err, &se) || se.code != http.StatusForbidden {
			return in, err
		}
		// Not this edition, or no rights (then the walk fails too, with
		// its own 403, not a false "no").
		log.Printf("oauth: checkTransitiveMembership on %s: HTTP 403; walking nested groups instead for %s", group, transitiveRetry)
		c.mu.Lock()
		if c.noTransitive == nil {
			c.noTransitive = map[string]time.Time{}
		}
		c.noTransitive[name] = time.Now()
		c.mu.Unlock()
	}
	return c.walk(ctx, name, member)
}

func (c *CloudIdentityGroups) transitive(ctx context.Context, name, member string) (bool, error) {
	q := url.Values{"query": {fmt.Sprintf("member_key_id == '%s'", strings.ReplaceAll(member, "'", ""))}}
	var r struct {
		HasMembership bool `json:"hasMembership"`
	}
	err := c.get(ctx, name+"/memberships:checkTransitiveMembership?"+q.Encode(), &r)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		// A member key the directory doesn't know isn't a member.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.HasMembership, nil
}

// walk looks for member in the group and, breadth first, in the groups
// nested in it. Each group's direct members are checked before its
// nested groups are expanded, and a nested group that can't be read is
// remembered rather than fatal: a match anywhere admits. Only if nothing
// matched does that failure become the answer, an error rather than a
// "no", since the member might be in the group that couldn't be read.
//
// Users and service accounts match by email. Other member types
// (devices, shared drives, customer-wide entries such as "everyone in
// the organization") aren't expanded and never match.
func (c *CloudIdentityGroups) walk(ctx context.Context, root, member string) (bool, error) {
	member = strings.ToLower(member)
	seen := map[string]bool{root: true}
	level := []string{root}
	var incomplete error
	for depth := 0; len(level) > 0; depth++ {
		if depth >= walkMaxDepth {
			incomplete = fmt.Errorf("group nesting deeper than %d levels", walkMaxDepth)
			break
		}
		var next []string
		for _, g := range level {
			members, err := c.members(ctx, g)
			if err != nil {
				if g == root {
					return false, err
				}
				incomplete = err
				continue
			}
			if directMember(members, member) {
				return true, nil
			}
			children, err := c.nestedGroups(ctx, members, seen)
			if err != nil {
				incomplete = err
			}
			next = append(next, children...)
		}
		level = next
	}
	if incomplete != nil {
		return false, fmt.Errorf("not found, but the walk was incomplete: %w", incomplete)
	}
	return false, nil
}

// directMember reports whether member (lowercased) is a user or service
// account in members.
func directMember(members []membership, member string) bool {
	for _, m := range members {
		switch m.Type {
		case "USER", "SERVICE_ACCOUNT", "TYPE_UNSPECIFIED", "":
			if strings.ToLower(m.PreferredMemberKey.ID) == member {
				return true
			}
		}
	}
	return false
}

// nestedGroups resolves the groups in members that haven't been seen,
// marking them seen. A group it can't resolve, or past walkMaxGroups, is
// skipped and reported in the error; the others are still returned.
func (c *CloudIdentityGroups) nestedGroups(ctx context.Context, members []membership, seen map[string]bool) ([]string, error) {
	var next []string
	var skipped error
	for _, m := range members {
		if m.Type != "GROUP" {
			continue
		}
		id := strings.ToLower(m.PreferredMemberKey.ID)
		if m.PreferredMemberKey.Namespace != "" {
			skipped = fmt.Errorf("nested group %s is from an external identity source (namespace %s)", id, m.PreferredMemberKey.Namespace)
			continue
		}
		child, err := c.groupName(ctx, id)
		if err != nil {
			skipped = fmt.Errorf("nested group %s: %w", id, err)
			continue
		}
		if seen[child] {
			continue
		}
		if len(seen) >= walkMaxGroups {
			skipped = fmt.Errorf("more than %d nested groups", walkMaxGroups)
			continue
		}
		seen[child] = true
		next = append(next, child)
	}
	return next, skipped
}

type membership struct {
	PreferredMemberKey struct {
		ID        string `json:"id"`
		Namespace string `json:"namespace"`
	} `json:"preferredMemberKey"`
	Type string `json:"type"`
}

// members lists a group's direct memberships, all pages, cached for
// allowCacheTTL.
func (c *CloudIdentityGroups) members(ctx context.Context, name string) ([]membership, error) {
	c.mu.Lock()
	e, ok := c.lists[name]
	c.mu.Unlock()
	if ok && time.Since(e.at) < allowCacheTTL {
		return e.v, nil
	}
	var all []membership
	token := ""
	for {
		q := url.Values{"view": {"FULL"}, "pageSize": {"200"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var r struct {
			Memberships   []membership `json:"memberships"`
			NextPageToken string       `json:"nextPageToken"`
		}
		if err := c.get(ctx, name+"/memberships?"+q.Encode(), &r); err != nil {
			return nil, err
		}
		all = append(all, r.Memberships...)
		if r.NextPageToken == "" {
			break
		}
		token = r.NextPageToken
	}
	c.mu.Lock()
	if c.lists == nil {
		c.lists = map[string]cached[[]membership]{}
	}
	c.lists[name] = cached[[]membership]{v: all, at: time.Now()}
	c.mu.Unlock()
	return all, nil
}

func (c *CloudIdentityGroups) groupName(ctx context.Context, group string) (string, error) {
	c.mu.Lock()
	e, ok := c.names[group]
	c.mu.Unlock()
	if ok && time.Since(e.at) < groupNameTTL {
		return e.v, nil
	}
	var r struct {
		Name string `json:"name"`
	}
	if err := c.get(ctx, "groups:lookup?"+url.Values{"groupKey.id": {group}}.Encode(), &r); err != nil {
		return "", err
	}
	if r.Name == "" {
		return "", errors.New("group lookup returned no name")
	}
	c.mu.Lock()
	if c.names == nil {
		c.names = map[string]cached[string]{}
	}
	c.names[group] = cached[string]{v: r.Name, at: time.Now()}
	c.mu.Unlock()
	return r.Name, nil
}

func (c *CloudIdentityGroups) get(ctx context.Context, path string, out any) error {
	ep := c.Endpoint
	if ep == "" {
		ep = "https://cloudidentity.googleapis.com/v1/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep+path, nil) //nolint:gosec // operator-configured Cloud Identity endpoint
	if err != nil {
		return err
	}
	resp, err := c.Client.Do(req) //nolint:gosec // as above
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("cloud identity: HTTP %d: %s", e.code, e.body)
}

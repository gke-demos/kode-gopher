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
// Enterprise and Cloud Identity Premium; other editions (Business
// Starter, ...) get 403. Then IsMember walks the group and its nested
// groups with memberships.list, which every edition has, and stops
// trying the transitive call.
type CloudIdentityGroups struct {
	// Client is authenticated with the
	// cloud-identity.groups.readonly scope.
	Client *http.Client
	// Endpoint defaults to https://cloudidentity.googleapis.com/v1/.
	Endpoint string

	mu           sync.Mutex
	names        map[string]string // group email -> groups/<id>
	noTransitive bool              // checkTransitiveMembership got 403: walk instead
}

// Limits on a membership walk, against runaway nesting; beyond them the
// check fails (server_error) rather than answering.
const (
	walkMaxDepth  = 10
	walkMaxGroups = 100
)

// IsMember implements GroupChecker.
func (c *CloudIdentityGroups) IsMember(ctx context.Context, group, member string) (bool, error) {
	name, err := c.groupName(ctx, group)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	walk := c.noTransitive
	c.mu.Unlock()
	if !walk {
		in, err := c.transitive(ctx, name, member)
		var se *statusError
		if !errors.As(err, &se) || se.code != http.StatusForbidden {
			return in, err
		}
		// Not this edition (or no rights: then the walk fails too, with
		// its own 403).
		c.mu.Lock()
		c.noTransitive = true
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
// nested in it. A nested group that can't be read is an error, not a
// "no": the answer would be wrong for its members.
func (c *CloudIdentityGroups) walk(ctx context.Context, root, member string) (bool, error) {
	member = strings.ToLower(member)
	seen := map[string]bool{root: true}
	level := []string{root}
	for depth := 0; len(level) > 0; depth++ {
		if depth >= walkMaxDepth {
			return false, fmt.Errorf("group nesting deeper than %d levels", walkMaxDepth)
		}
		var next []string
		for _, g := range level {
			members, err := c.members(ctx, g)
			if err != nil {
				return false, err
			}
			for _, m := range members {
				id := strings.ToLower(m.PreferredMemberKey.ID)
				if m.Type != "GROUP" {
					if id == member {
						return true, nil
					}
					continue
				}
				child, err := c.groupName(ctx, id)
				if err != nil {
					return false, fmt.Errorf("nested group %s: %w", id, err)
				}
				if !seen[child] {
					if len(seen) >= walkMaxGroups {
						return false, fmt.Errorf("more than %d nested groups", walkMaxGroups)
					}
					seen[child] = true
					next = append(next, child)
				}
			}
		}
		level = next
	}
	return false, nil
}

type membership struct {
	PreferredMemberKey struct {
		ID string `json:"id"`
	} `json:"preferredMemberKey"`
	Type string `json:"type"`
}

// members lists a group's direct memberships, all pages.
func (c *CloudIdentityGroups) members(ctx context.Context, name string) ([]membership, error) {
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
			return all, nil
		}
		token = r.NextPageToken
	}
}

func (c *CloudIdentityGroups) groupName(ctx context.Context, group string) (string, error) {
	c.mu.Lock()
	name, ok := c.names[group]
	c.mu.Unlock()
	if ok {
		return name, nil
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
		c.names = map[string]string{}
	}
	c.names[group] = r.Name
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

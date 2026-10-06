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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Service is service-identity mode (docs/design-in-cluster.md, section
// 4): every run gets a short-lived token for one Google service account,
// minted by impersonation and served through sandbox-server's metadata
// emulator, like Minted. kode-gopher's own identity (Workload Identity
// in a pod) only needs to mint tokens for that account; snippets get
// whatever the account is granted.
type Service struct {
	email        string
	project      string
	quotaProject string
	mint         *Impersonator
}

// NewService runs snippets as the service account email, minting with
// imp. An empty project defaults to the account's own project (from
// <name>@<project>.iam.gserviceaccount.com).
func NewService(imp *Impersonator, email, project, quotaProject string) (*Service, error) {
	if !strings.Contains(email, "@") {
		return nil, fmt.Errorf("service account %q isn't an email", email)
	}
	if project == "" {
		project = ServiceAccountProject(email)
	}
	return &Service{email: email, project: project, quotaProject: quotaProject, mint: imp}, nil
}

// ServiceAccountProject is the project in a user-managed service
// account's email, or "" for any other email.
func ServiceAccountProject(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	p, ok := strings.CutSuffix(domain, ".iam.gserviceaccount.com")
	if !ok {
		return ""
	}
	return p
}

// Materialize implements Source: nothing goes into the sandbox as files
// or command env.
func (s *Service) Materialize(_ context.Context) (map[string][]byte, map[string]string, error) {
	return map[string][]byte{}, map[string]string{}, nil
}

// AccessToken implements TokenMinter.
func (s *Service) AccessToken(ctx context.Context) (AccessToken, error) {
	tok, err := s.mint.Token(ctx, s.email)
	if err != nil {
		return AccessToken{}, err
	}
	return AccessToken{
		Token:        tok.AccessToken,
		Expiry:       tok.Expiry,
		Email:        s.email,
		Project:      s.project,
		QuotaProject: s.quotaProject,
	}, nil
}

// Identity implements Source.
func (s *Service) Identity(_ context.Context) (Identity, error) {
	return Identity{Mode: "service", CredType: "service_account", Email: s.email, ProjectID: s.project}, nil
}

// Impersonator mints cloud-platform access tokens for Google service
// accounts with the IAM Credentials API (generateAccessToken). Its
// Client calls as kode-gopher's own identity, which needs
// roles/iam.serviceAccountTokenCreator on each account. Tokens are
// cached per account and reused until minTokenLife before expiry.
type Impersonator struct {
	// Client is authorized as kode-gopher (see NewImpersonator).
	Client *http.Client
	// Endpoint is the IAM Credentials API's base URL. Default:
	// https://iamcredentials.googleapis.com. Tests override it.
	Endpoint string

	mu      sync.Mutex
	sources map[string]oauth2.TokenSource
}

// NewImpersonator calls as kode-gopher's ADC.
func NewImpersonator(ctx context.Context) (*Impersonator, error) {
	hc, err := google.DefaultClient(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("find ADC: %w", err)
	}
	return &Impersonator{Client: hc}, nil
}

// Token returns a token for the service account email.
func (i *Impersonator) Token(_ context.Context, email string) (*oauth2.Token, error) {
	i.mu.Lock()
	if i.sources == nil {
		i.sources = map[string]oauth2.TokenSource{}
	}
	ts, ok := i.sources[email]
	if !ok {
		ts = oauth2.ReuseTokenSourceWithExpiry(nil, impersonated{i, email}, minTokenLife)
		i.sources[email] = ts
	}
	i.mu.Unlock()
	return ts.Token()
}

// TokenSource is an oauth2.TokenSource for email, sharing Token's cache,
// for clients that take one (oauth2.NewClient).
func (i *Impersonator) TokenSource(email string) oauth2.TokenSource {
	return tokenSourceFunc(func() (*oauth2.Token, error) { return i.Token(context.Background(), email) })
}

type tokenSourceFunc func() (*oauth2.Token, error)

func (f tokenSourceFunc) Token() (*oauth2.Token, error) { return f() }

// impersonated is one account's token source. oauth2.TokenSource has no
// context, so each mint gets its own timeout.
type impersonated struct {
	i     *Impersonator
	email string
}

func (s impersonated) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := s.i.Endpoint
	if base == "" {
		base = "https://iamcredentials.googleapis.com"
	}
	u := base + "/v1/projects/-/serviceAccounts/" + url.PathEscape(s.email) + ":generateAccessToken"
	body, _ := json.Marshal(map[string]any{"scope": []string{cloudPlatformScope}, "lifetime": "3600s"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.i.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("impersonate %s: %w", s.email, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("impersonate %s: %w", s.email, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, fmt.Errorf("impersonate %s: HTTP %d: %s", s.email, resp.StatusCode, e.Error.Message)
	}
	var out struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("impersonate %s: parse response: %w", s.email, err)
	}
	if out.AccessToken == "" || out.ExpireTime.IsZero() {
		return nil, errors.New("impersonate " + s.email + ": response has no token or expiry")
	}
	return &oauth2.Token{AccessToken: out.AccessToken, TokenType: "Bearer", Expiry: out.ExpireTime}, nil
}

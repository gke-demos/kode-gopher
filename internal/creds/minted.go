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
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// minTokenLife is the least lifetime a minted token has left when it's
// handed to a run. Runs are bounded well below this (exec timeout).
const minTokenLife = 5 * time.Minute

// Minted is the access-token credential source: kode-gopher's own ADC
// mints short-lived cloud-platform tokens, and each run gets one through
// sandbox-server's metadata emulator. Nothing is written into the
// sandbox, and no refresh token ever leaves kode-gopher.
//
// Run locally, the ADC is the user's gcloud login. In a pod, it's the
// pod's Workload Identity: that's the service-identity mode of
// docs/design-in-cluster.md, and the data-path test for build step 1.
type Minted struct {
	baseCtx      context.Context
	creds        *google.Credentials
	ts           oauth2.TokenSource
	quotaProject string
	readOnly     bool

	roOnce sync.Once
	roTS   oauth2.TokenSource
	roErr  error

	idOnce sync.Once
	id     Identity
	idErr  error
}

// NewMinted finds ADC the usual way (GOOGLE_APPLICATION_CREDENTIALS,
// gcloud's well-known file, then the metadata server).
func NewMinted(ctx context.Context) (*Minted, error) {
	c, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("find ADC: %w", err)
	}
	return newMinted(ctx, c, os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT"), false)
}

// NewReadOnlyMinted finds ADC and restricts minted tokens to
// ReadOnlyScopes. Fails closed when ADC is backed by the GCE/GKE
// metadata server or an authorized_user refresh token, neither of which
// can narrow scopes at token mint time.
func NewReadOnlyMinted(ctx context.Context) (*Minted, error) {
	c, err := google.FindDefaultCredentials(ctx, ReadOnlyScopes...)
	if err != nil {
		return nil, fmt.Errorf("find ADC: %w", err)
	}
	return newMinted(ctx, c, os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT"), true)
}

func newMinted(ctx context.Context, c *google.Credentials, quotaProject string, readOnly bool) (*Minted, error) {
	baseCtx := context.WithoutCancel(ctx)
	m := &Minted{
		baseCtx:      baseCtx,
		creds:        c,
		ts:           oauth2.ReuseTokenSourceWithExpiry(nil, c.TokenSource, minTokenLife),
		quotaProject: quotaProject,
		readOnly:     readOnly,
	}
	if readOnly {
		roTS, err := buildReadOnlyTokenSource(baseCtx, c)
		if err != nil {
			return nil, err
		}
		m.roOnce.Do(func() { m.roTS = roTS })
	}
	return m, nil
}

func buildReadOnlyTokenSource(ctx context.Context, c *google.Credentials) (oauth2.TokenSource, error) {
	if len(c.JSON) == 0 {
		return nil, errors.New("read-only credentials with access-token mode require service_account, impersonated_service_account, or external_account ADC JSON (got metadata); use --credentials=service --service-account=<email> instead")
	}
	var raw struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(c.JSON, &raw); err != nil {
		return nil, fmt.Errorf("parse ADC JSON: %w", err)
	}
	switch raw.Type {
	case "service_account", "impersonated_service_account", "external_account":
	default:
		return nil, fmt.Errorf("read-only credentials with access-token mode do not support ADC type %q (refresh tokens keep their original scopes); use --credentials=service --service-account=<email> or --auth=oauth --read-only", raw.Type)
	}
	roCreds, err := google.CredentialsFromJSONWithType(ctx, c.JSON, google.CredentialsType(raw.Type), ReadOnlyScopes...)
	if err != nil {
		return nil, fmt.Errorf("build read-only credentials: %w", err)
	}
	return oauth2.ReuseTokenSourceWithExpiry(nil, roCreds.TokenSource, minTokenLife), nil
}

// Materialize implements Source: nothing goes into the sandbox as files
// or command env. The run gets its token from AccessToken.
func (m *Minted) Materialize(_ context.Context) (map[string][]byte, map[string]string, error) {
	return map[string][]byte{}, map[string]string{}, nil
}

// AccessToken implements TokenMinter.
func (m *Minted) AccessToken(ctx context.Context) (AccessToken, error) {
	if m.readOnly {
		return m.ReadOnlyAccessToken(ctx)
	}
	return m.mintFrom(ctx, m.ts, nil)
}

// ReadOnlyAccessToken implements ReadOnlyTokenMinter.
func (m *Minted) ReadOnlyAccessToken(ctx context.Context) (AccessToken, error) {
	m.roOnce.Do(func() {
		m.roTS, m.roErr = buildReadOnlyTokenSource(m.baseCtx, m.creds)
	})
	if m.roErr != nil {
		return AccessToken{}, m.roErr
	}
	return m.mintFrom(ctx, m.roTS, slices.Clone(ReadOnlyScopes))
}

func (m *Minted) mintFrom(ctx context.Context, ts oauth2.TokenSource, scopes []string) (AccessToken, error) {
	tok, err := ts.Token()
	if err != nil {
		return AccessToken{}, fmt.Errorf("mint access token: %w", err)
	}
	if tok.Expiry.IsZero() {
		return AccessToken{}, fmt.Errorf("minted token has no expiry")
	}
	id, _ := m.Identity(ctx) // best-effort: email and project are optional
	return AccessToken{
		Token:        tok.AccessToken,
		Expiry:       tok.Expiry,
		Email:        id.Email,
		Project:      id.ProjectID,
		QuotaProject: m.quotaProject,
		Scopes:       scopes,
	}, nil
}

// Identity implements Source. Cached, like Forwarded's.
func (m *Minted) Identity(ctx context.Context) (Identity, error) {
	m.idOnce.Do(func() {
		m.id, m.idErr = m.lookupIdentity(ctx)
	})
	return m.id, m.idErr
}

func (m *Minted) lookupIdentity(ctx context.Context) (Identity, error) {
	id := Identity{Mode: "access-token", ProjectID: m.creds.ProjectID, ReadOnly: m.readOnly}
	if id.ProjectID == "" {
		id.ProjectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if len(m.creds.JSON) == 0 {
		// No credentials file: the metadata server (Workload Identity).
		id.CredType = "metadata"
		email, err := metadata.EmailWithContext(ctx, "default")
		if err != nil {
			return id, fmt.Errorf("metadata email: %w", err)
		}
		id.Email = email
		return id, nil
	}
	var raw struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(m.creds.JSON, &raw); err != nil {
		return id, fmt.Errorf("parse ADC JSON: %w", err)
	}
	id.CredType = raw.Type
	switch raw.Type {
	case "service_account":
		id.Email = raw.ClientEmail
	case "authorized_user":
		email, err := fetchUserEmail(ctx, m.creds.JSON)
		if err != nil {
			return id, fmt.Errorf("userinfo lookup: %w", err)
		}
		id.Email = email
	}
	return id, nil
}

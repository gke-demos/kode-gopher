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
	"os"
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
	creds        *google.Credentials
	ts           oauth2.TokenSource
	quotaProject string

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
	return &Minted{
		creds:        c,
		ts:           oauth2.ReuseTokenSourceWithExpiry(nil, c.TokenSource, minTokenLife),
		quotaProject: os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT"),
	}, nil
}

// Materialize implements Source: nothing goes into the sandbox as files
// or command env. The run gets its token from AccessToken.
func (m *Minted) Materialize(_ context.Context) (map[string][]byte, map[string]string, error) {
	return map[string][]byte{}, map[string]string{}, nil
}

// AccessToken implements TokenMinter.
func (m *Minted) AccessToken(ctx context.Context) (AccessToken, error) {
	tok, err := m.ts.Token()
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
	id := Identity{Mode: "access-token", ProjectID: m.creds.ProjectID}
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

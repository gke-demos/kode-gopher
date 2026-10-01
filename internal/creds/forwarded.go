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
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

// Sandbox-side path where forwarded ADC lands. Kept internal to
// creds so a change here only touches this package.
const (
	sandboxADCRelPath = ".kode-gopher/creds/adc.json"
	sandboxADCAbsPath = "/app/.kode-gopher/creds/adc.json"
	appCredsEnv       = "GOOGLE_APPLICATION_CREDENTIALS" // #nosec G101 -- an env var name, not a credential
)

// Forwarded is the desktop / personal-use credential source: reads the
// gcloud Application Default Credentials JSON from disk on every
// Materialize call, ships it into the sandbox alongside the small
// allow-list of GOOGLE_CLOUD_* env vars callers care about.
//
// Identity() parses the ADC JSON's `type` field. For service_account
// creds it returns client_email directly; for authorized_user (the
// common `gcloud auth application-default login` case) it exchanges
// the refresh token for an access token and hits the OAuth2 userinfo
// endpoint. Identity result is cached — userinfo is ~200ms and the
// identity doesn't change over server lifetime.
type Forwarded struct {
	// ADCPath is the absolute path to the local ADC JSON. Empty is
	// legal (Materialize returns empty maps, Identity returns Mode=none).
	ADCPath string
	// EnvAllowList names host env vars to forward. Populated ones
	// end up in Materialize's env return; missing ones are skipped.
	// Kept small on purpose — this is not a general env-pass-through.
	EnvAllowList []string

	idOnce sync.Once
	id     Identity
	idErr  error
}

// NewForwarded constructs a Forwarded source. Neither arg is validated
// eagerly; the ADC file is checked at Materialize / Identity time.
func NewForwarded(adcPath string, envAllowList []string) *Forwarded {
	return &Forwarded{ADCPath: adcPath, EnvAllowList: envAllowList}
}

// Materialize implements Source. If the ADC file is missing or
// unreadable, returns just the env allowlist — GCP calls in the
// sandbox will fail, which is a legible failure mode for the caller.
func (f *Forwarded) Materialize(_ context.Context) (map[string][]byte, map[string]string, error) {
	files := map[string][]byte{}
	envs := map[string]string{}
	for _, k := range f.EnvAllowList {
		if v := os.Getenv(k); v != "" {
			envs[k] = v
		}
	}
	if f.ADCPath == "" {
		return files, envs, nil
	}
	adc, err := os.ReadFile(f.ADCPath)
	if err != nil {
		if os.IsNotExist(err) {
			return files, envs, nil
		}
		return nil, nil, fmt.Errorf("read ADC at %s: %w", f.ADCPath, err)
	}
	files[sandboxADCRelPath] = adc
	envs[appCredsEnv] = sandboxADCAbsPath
	return files, envs, nil
}

// Identity implements Source. Cached after first call — subsequent
// calls return the cached result without hitting disk or the network,
// even if the ADC file changes underneath. This is the right tradeoff
// for a long-lived MCP server: identity is a property of the process's
// startup credentials, not something to poll for.
func (f *Forwarded) Identity(ctx context.Context) (Identity, error) {
	f.idOnce.Do(func() {
		f.id, f.idErr = f.lookupIdentity(ctx)
	})
	return f.id, f.idErr
}

func (f *Forwarded) lookupIdentity(ctx context.Context) (Identity, error) {
	if f.ADCPath == "" {
		return Identity{Mode: "none"}, nil
	}
	adc, err := os.ReadFile(f.ADCPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Identity{Mode: "none"}, nil
		}
		return Identity{}, fmt.Errorf("read ADC at %s: %w", f.ADCPath, err)
	}

	var raw struct {
		Type           string `json:"type"`
		ClientEmail    string `json:"client_email"`
		QuotaProjectID string `json:"quota_project_id"`
	}
	if err := json.Unmarshal(adc, &raw); err != nil {
		return Identity{}, fmt.Errorf("parse ADC JSON: %w", err)
	}

	id := Identity{
		Mode:      "forwarded",
		CredType:  raw.Type,
		ProjectID: raw.QuotaProjectID,
	}
	if id.ProjectID == "" {
		id.ProjectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}

	switch raw.Type {
	case "service_account":
		id.Email = raw.ClientEmail
	case "authorized_user":
		email, err := fetchUserEmail(ctx, adc)
		if err != nil {
			// Return what we have plus the error — callers can decide
			// whether to surface the identity or the error. The next
			// Identity call returns this same (id, err) since idOnce
			// already fired.
			return id, fmt.Errorf("userinfo lookup: %w", err)
		}
		id.Email = email
	}
	return id, nil
}

// fetchUserEmail exchanges the refresh token in adcJSON for an access
// token via the userinfo.email scope, then GETs the OAuth2 userinfo
// endpoint to pull the account's email address. Only used for
// authorized_user creds (service accounts already carry client_email
// in the JSON).
func fetchUserEmail(ctx context.Context, adcJSON []byte) (string, error) {
	creds, err := google.CredentialsFromJSONWithType(ctx, adcJSON, google.AuthorizedUser, "https://www.googleapis.com/auth/userinfo.email")
	if err != nil {
		return "", fmt.Errorf("credentials from JSON: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("userinfo GET: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode userinfo: %w", err)
	}
	if info.Email == "" {
		return "", fmt.Errorf("userinfo response missing email field")
	}
	return info.Email, nil
}

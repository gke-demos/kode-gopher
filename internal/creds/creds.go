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

// Package creds is the unified interface between kode-gopher's host
// layer and whatever ambient Google-Cloud credentials it should forward
// into the sandbox pod.
//
// Prior to slice 4 this was a bare `func() (files, env)` callback on
// mcp.Config, adequate for materializing ADC bytes but incapable of
// answering "what identity does this sandbox actually run as?" — the
// question gcp_auth_status needs. Source formalizes both duties:
//
//	Materialize — for the executor: what files and env should be
//	forwarded on each tool invocation.
//	Identity — for gcp_auth_status: whose identity does the sandbox
//	run under, in what form, on what project.
//
// Implementations: Forwarded (desktop / gcloud ADC copied into the
// sandbox), Minted (short-lived access tokens served to the run through
// sandbox-server's metadata emulator; a TokenMinter), Service (the same,
// for one impersonated service account) and OAuthUser (one HTTP
// request's own token).
package creds

import (
	"context"
	"time"
)

// Identity summarizes whose credentials the sandbox will actually use
// at run time. Best-effort — Email in particular requires a live
// userinfo call for authorized_user creds, and callers should treat
// missing fields as "unknown" rather than "definitely absent."
type Identity struct {
	// Mode is the credential-forwarding shape: "forwarded" means the
	// host copied ADC into the sandbox; "access-token" means each run
	// gets a token minted by kode-gopher (Minted); "service" means each
	// run gets a token for a configured service account (Service, or a
	// client-credentials OAuthUser); "oauth" means the signed-in user's
	// own token (OAuthUser); "none" means there are no forwarded
	// credentials (GCP calls will fail).
	Mode string `json:"mode"`

	// CredType echoes ADC's `type` field ("authorized_user" or
	// "service_account") when Mode=forwarded or access-token, or
	// "metadata" when kode-gopher's ADC is the metadata server. Empty for
	// Mode=none.
	CredType string `json:"credential_type,omitempty"`

	// Email is the identity the sandbox runs as. For service_account
	// creds this comes from the JSON's client_email field. For
	// authorized_user creds it comes from an OAuth2 userinfo lookup.
	// For metadata credentials it comes from the metadata server. May
	// be empty on lookup failure.
	Email string `json:"email,omitempty"`

	// ProjectID is the GCP project associated with these credentials
	// (ADC's quota_project_id, or $GOOGLE_CLOUD_PROJECT, or the
	// metadata server's project). May be empty if none is discoverable.
	ProjectID string `json:"project_id,omitempty"`
}

// Source is what mcp.Config holds. Concurrent-safe: implementations
// may cache expensive-to-compute results (userinfo lookups are ~200ms
// and the identity doesn't change over server lifetime), but every
// method must be safe to call from any goroutine.
type Source interface {
	// Materialize returns files to inject into the sandbox and env
	// vars to apply to the user program. Called on every tool
	// invocation, so it must be cheap. Files are merged under /app;
	// env applies only to the user's run phase (not tidy/build).
	//
	// A nil error with empty maps is fine — "no credentials to
	// forward" is a valid state.
	Materialize(ctx context.Context) (files map[string][]byte, env map[string]string, err error)

	// Identity returns metadata about the credentials Materialize
	// forwards. Best-effort; implementations should cache the result
	// after the first successful call.
	Identity(ctx context.Context) (Identity, error)
}

// AccessToken is a short-lived Google access token for one run, plus
// what the sandbox's metadata emulator reports alongside it.
type AccessToken struct {
	Token        string
	Expiry       time.Time
	Email        string
	Project      string
	QuotaProject string
}

// TokenMinter is a Source that hands each run an access token rather
// than files. The executor passes it to the run phase only, through
// sandbox-server's metadata emulator; it needs in-cluster connectivity.
type TokenMinter interface {
	Source
	AccessToken(ctx context.Context) (AccessToken, error)
}

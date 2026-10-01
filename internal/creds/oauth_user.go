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
	"errors"
	"time"
)

// TokenInfoKey is the auth.TokenInfo.Extra key under which an HTTP
// request's token verifier (internal/oauth) puts that request's Source.
// It overrides the server-wide one for that request's tool calls.
const TokenInfoKey = "kode-gopher/credentials" //nolint:gosec // a map key, not a credential

// OAuthUser is one request's credentials in OAuth mode: the user's
// Google access token, unsealed from their kode-gopher access token.
// It's built per request, holds no refresh token, and each run gets the
// token through sandbox-server's metadata emulator (so it needs
// in-cluster connectivity, like Minted).
//
// A client-credentials token (no user) has the same shape, with the
// token of the service account configured for that client.
type OAuthUser struct {
	Token        string
	Expiry       time.Time
	Email        string
	Project      string
	QuotaProject string
	// ServiceAccount marks a client-credentials token: Email is the
	// service account, and Identity reports mode=service.
	ServiceAccount bool
}

// Materialize implements Source: nothing goes into the sandbox as files
// or command env.
func (u *OAuthUser) Materialize(_ context.Context) (map[string][]byte, map[string]string, error) {
	return map[string][]byte{}, map[string]string{}, nil
}

// AccessToken implements TokenMinter.
func (u *OAuthUser) AccessToken(_ context.Context) (AccessToken, error) {
	if u.Token == "" || time.Until(u.Expiry) <= 0 {
		return AccessToken{}, errors.New("the Google access token in this request's kode-gopher token has expired; the client should refresh its token")
	}
	return AccessToken{
		Token:        u.Token,
		Expiry:       u.Expiry,
		Email:        u.Email,
		Project:      u.Project,
		QuotaProject: u.QuotaProject,
	}, nil
}

// Identity implements Source.
func (u *OAuthUser) Identity(_ context.Context) (Identity, error) {
	if u.ServiceAccount {
		return Identity{Mode: "service", CredType: "service_account", Email: u.Email, ProjectID: u.Project}, nil
	}
	return Identity{Mode: "oauth", CredType: "authorized_user", Email: u.Email, ProjectID: u.Project}, nil
}

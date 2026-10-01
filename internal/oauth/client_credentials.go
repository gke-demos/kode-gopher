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
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// ServiceTokens mints Google access tokens for service accounts
// (creds.Impersonator).
type ServiceTokens interface {
	Token(ctx context.Context, serviceAccount string) (*oauth2.Token, error)
}

// modeService marks an access token from the client-credentials grant.
const modeService = "service"

// clientCredentials is the client-credentials grant (RFC 6749 §4.4) for
// pre-registered confidential clients with a service account. There's
// no user: the token carries the service account's Google token, and
// its subject is the client. No refresh token is issued (§4.4.3); the
// client asks again with its secret.
func (s *Server) clientCredentials(r *http.Request, c *client) (map[string]any, *tokenError) {
	f := r.PostForm
	if c.ServiceAccount == "" || c.Secret == "" {
		return nil, &tokenError{Code: "unauthorized_client", Desc: "this client may not use the client_credentials grant"}
	}
	scope := ScopeExecute
	if req := strings.Fields(f.Get("scope")); len(req) > 0 {
		for _, sc := range req {
			if sc != ScopeExecute {
				return nil, &tokenError{Code: "invalid_scope", Desc: "only " + ScopeExecute + " is available"}
			}
		}
	}
	if res := f.Get("resource"); res != "" && res != s.resource {
		return nil, &tokenError{Code: "invalid_target", Desc: "resource must be " + s.resource}
	}
	tok, err := s.cfg.ServiceTokens.Token(r.Context(), c.ServiceAccount)
	if err != nil {
		log.Printf("oauth: client %s: mint token for %s: %v", c.ID, c.ServiceAccount, err) //nolint:gosec // c is an authenticated pre-registered client
		return nil, &tokenError{Code: "server_error", Desc: "couldn't mint a token for the client's service account"}
	}
	now := time.Now()
	at, exp, terr := s.sealAccess(accessPayload{
		Sub: serviceSubject(c.ID), Email: c.ServiceAccount, ClientID: c.ID, Aud: s.resource, Scope: scope,
		Token: tok.AccessToken, TokenExp: tok.Expiry.Unix(), Mode: modeService,
	}, now)
	if terr != nil {
		return nil, terr
	}
	return map[string]any{
		"access_token": at,
		"token_type":   "Bearer",
		"expires_in":   int64(exp.Sub(now).Seconds()),
		"scope":        scope,
	}, nil
}

// serviceSubject is a client-credentials token's subject: MCP sessions
// and the per-user sandbox cap bind to it. The prefix keeps it apart
// from Google subs, which are numeric.
func serviceSubject(clientID string) string { return "client:" + clientID }

// serviceClientCurrent reports whether a client-credentials token still
// matches the configuration: removing the client, or changing its
// service account, revokes its tokens at once.
func (s *Server) serviceClientCurrent(p *accessPayload) bool {
	c, ok := s.static[p.ClientID]
	return ok && c.ServiceAccount != "" && c.ServiceAccount == p.Email && p.Sub == serviceSubject(c.ID)
}

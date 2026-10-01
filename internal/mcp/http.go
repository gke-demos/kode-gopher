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

package mcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPOptions configures RunHTTP.
type HTTPOptions struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Verifier authenticates every request's bearer token. Required,
	// unless Protect is set. The TokenInfo's UserID binds each MCP
	// session to one user (the SDK refuses a session's requests from
	// anyone else) and keys Config.MaxSandboxesPerUser. A
	// creds.TokenInfoKey entry in its Extra overrides
	// Config.Credentials for that request.
	Verifier auth.TokenVerifier
	// Protect, if set, wraps /mcp instead of a plain bearer check on
	// Verifier: the OAuth server's (internal/oauth), with its metadata
	// pointers and scope challenge.
	Protect func(http.Handler) http.Handler
	// Routes, if set, serves every other path: the OAuth endpoints.
	Routes http.Handler
	// SessionTimeout ends MCP sessions that see no requests for this
	// long, which closes their sandboxes. Zero: sessions end only when
	// the client deletes them.
	SessionTimeout time.Duration
}

// RunHTTP serves MCP over streamable HTTP at /mcp (and a liveness
// probe at /healthz) until ctx is cancelled, then releases every
// session's sandbox.
func (s *Server) RunHTTP(ctx context.Context, opts HTTPOptions) error {
	if opts.Verifier == nil && opts.Protect == nil {
		return errors.New("mcp: RunHTTP needs a token verifier")
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: s.httpHandler(opts), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mcp: serving streamable HTTP on %s/mcp", ln.Addr())
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()

	select {
	case err = <-serveErr:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Close, not Shutdown: open SSE streams would hold Shutdown until
	// the timeout, and every session is ending anyway.
	_ = hs.Close()
	s.shutdown(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// httpHandler routes /mcp (bearer-authenticated), /healthz, and
// anything else to opts.Routes.
func (s *Server) httpHandler(opts HTTPOptions) http.Handler {
	srv := s.newSDKServer()
	mcpHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{
		SessionTimeout: opts.SessionTimeout,
	})
	mux := http.NewServeMux()
	protect := opts.Protect
	if protect == nil {
		protect = auth.RequireBearerToken(opts.Verifier, nil)
	}
	mux.Handle("/mcp", protect(mcpHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if opts.Routes != nil {
		mux.Handle("/", opts.Routes)
	}
	return mux
}

// StaticTokenVerifier accepts exactly token, as user. For a single
// operator-issued token until OAuth lands (docs/design-in-cluster.md,
// build step 3).
func StaticTokenVerifier(token, user string) auth.TokenVerifier {
	want := []byte(token)
	return func(_ context.Context, got string, _ *http.Request) (*auth.TokenInfo, error) {
		if len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			return nil, auth.ErrInvalidToken
		}
		// The SDK requires an expiry; a static token has none of its own.
		return &auth.TokenInfo{UserID: user, Expiration: time.Now().Add(time.Hour)}, nil
	}
}

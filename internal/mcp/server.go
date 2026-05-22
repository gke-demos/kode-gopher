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

// Package mcp wraps github.com/modelcontextprotocol/go-sdk to expose
// kode-gopher as an MCP server. One server process holds at most one
// goruntime.Session for its lifetime, opens it lazily on the first
// tool call, and serializes all tool invocations with a mutex.
//
// Today the server registers exactly one tool, execute_go_code; the
// design's lookup_package_docs and gcp_auth_status land in slice 4.
package mcp

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gke-demos/go-runtime-sandbox/pkg/goruntime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is what the server advertises to clients.
const Version = "0.1.0"

// CredentialHook returns extra files + env vars to inject on every
// tool call. Files are merged into the sandbox file map (typically a
// forwarded ADC json under .kode-gopher/creds/); env vars apply only
// to the user binary's run phase (executor.Request.Env).
//
// Returning nil maps is fine. A nil hook means "no creds forwarding"
// — the sandbox runs without GOOGLE_APPLICATION_CREDENTIALS.
type CredentialHook func() (files map[string][]byte, env map[string]string)

// Config is everything Server needs to know to open and drive a
// goruntime.Session. Zero-value fields fall back to sensible defaults.
type Config struct {
	// Namespace is the k8s namespace that holds the SandboxTemplate
	// and where the SandboxClaim is created.
	Namespace string
	// Template is the SandboxTemplate name to claim from.
	Template string
	// Claim, if non-empty, reattaches to an existing sandbox instead
	// of creating a new one on first tool call.
	Claim string
	// Persistent: on shutdown, Disconnect (leave sandbox alive) rather
	// than Close (delete it). Useful for development across multiple
	// server restarts.
	Persistent bool
	// OpenTimeout bounds the goruntime.Open call on first tool use.
	OpenTimeout time.Duration
	// ExecTimeout bounds each individual sandbox /execute call. The
	// upstream HTTP layer caps at ~60s regardless, so values >60s
	// only affect goruntime's internal accounting.
	ExecTimeout time.Duration
	// Credentials, if non-nil, is called on every tool invocation to
	// fold ambient host credentials into the request.
	Credentials CredentialHook
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.Template == "" {
		c.Template = "go-runtime-template"
	}
	if c.OpenTimeout == 0 {
		c.OpenTimeout = 5 * time.Minute
	}
	if c.ExecTimeout == 0 {
		c.ExecTimeout = 90 * time.Second
	}
}

// Server is the kode-gopher MCP server.
type Server struct {
	cfg Config

	// mu guards session (and only session). Held briefly during
	// lazy-open and shutdown.
	mu      sync.Mutex
	session *goruntime.Session

	// execMu serializes Execute calls so concurrent tool invocations
	// can't race on /app or clobber each other's build artifacts. The
	// SDK may dispatch tool calls concurrently; this gives the
	// session the "one-at-a-time" semantics agents actually expect.
	execMu sync.Mutex
}

// New creates a Server with cfg's defaults filled in.
func New(cfg Config) *Server {
	cfg.applyDefaults()
	return &Server{cfg: cfg}
}

// Run starts the MCP server on the stdio transport and blocks until
// ctx is cancelled or the transport closes. Always calls shutdown on
// return (using a fresh context so it completes even after ctx is
// cancelled by SIGINT).
func (s *Server) Run(ctx context.Context) error {
	srv := sdk.NewServer(&sdk.Implementation{
		Name:    "kode-gopher",
		Version: Version,
	}, nil)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "execute_go_code",
		Description: executeGoCodeDescription,
	}, s.handleExecuteGoCode)

	runErr := srv.Run(ctx, &sdk.StdioTransport{})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.shutdown(shutdownCtx); err != nil {
		log.Printf("mcp shutdown: %v", err)
	}

	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}

// ensureSession lazy-opens the goruntime.Session on first use and
// reuses it on subsequent calls.
func (s *Server) ensureSession(ctx context.Context) (*goruntime.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		return s.session, nil
	}
	openCtx, cancel := context.WithTimeout(ctx, s.cfg.OpenTimeout)
	defer cancel()
	log.Printf("opening sandbox (namespace=%s template=%s claim=%q)", s.cfg.Namespace, s.cfg.Template, s.cfg.Claim)
	sess, err := goruntime.Open(openCtx, goruntime.Options{
		Namespace: s.cfg.Namespace,
		Template:  s.cfg.Template,
		ClaimName: s.cfg.Claim,
	})
	if err != nil {
		return nil, fmt.Errorf("open sandbox: %w", err)
	}
	log.Printf("sandbox open: claim=%s", sess.ClaimName())
	s.session = sess
	return sess, nil
}

// shutdown is called by Run after the transport closes. Closes (or
// disconnects from, if Persistent) the sandbox.
func (s *Server) shutdown(ctx context.Context) error {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.mu.Unlock()
	if sess == nil {
		return nil
	}
	if s.cfg.Persistent {
		log.Printf("disconnecting (claim %s preserved; reattach with --claim=%s)", sess.ClaimName(), sess.ClaimName())
		return sess.Disconnect(ctx)
	}
	log.Printf("closing sandbox %s", sess.ClaimName())
	return sess.Close(ctx)
}

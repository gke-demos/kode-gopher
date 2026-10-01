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
// kode-gopher as an MCP server. Each MCP session gets its own sandbox,
// opened lazily on the session's first tool call, and serializes that
// session's tool invocations with a mutex. Over stdio there is one
// session for the life of the process. Over streamable HTTP (RunHTTP)
// there is one per Mcp-Session-Id, and its sandbox is closed when the
// session ends.
package mcp

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/kode-gopher/internal/creds"
	"github.com/gke-demos/kode-gopher/internal/sandbox"
)

// Version is what the server advertises to clients.
const Version = "0.1.0"

// Config is everything Server needs to know to open and drive a
// sandbox.Session. Zero-value fields fall back to sensible defaults.
type Config struct {
	// Namespace is the k8s namespace that holds the SandboxWarmPool
	// and where the SandboxClaim is created.
	Namespace string
	// WarmPool is the SandboxWarmPool name to claim from.
	WarmPool string
	// Claim, if non-empty, reattaches to an existing sandbox instead
	// of creating a new one on first tool call. stdio only.
	Claim string
	// Persistent: on shutdown, Disconnect (leave sandbox alive) rather
	// than Close (delete it). Useful for development across multiple
	// server restarts. stdio only.
	Persistent bool
	// OpenTimeout bounds the sandbox.Open call on first tool use.
	OpenTimeout time.Duration
	// ExecTimeout bounds each individual sandbox /execute call. Its
	// upstream HTTP layer cap comes from internal/sandbox.Options
	// PerAttemptTimeout (default 3min).
	ExecTimeout time.Duration
	// KubeContext, if non-empty, forwards to sandbox.Options.KubeContext
	// so the server can pin a kubeconfig context rather than inheriting
	// ambient `kubectl config current-context`. Empty preserves
	// previous behavior.
	KubeContext string
	// Credentials, if non-nil, is called on every tool invocation to
	// materialize credential files/env into the request, and by
	// gcp_auth_status to report the sandbox's identity. A nil Source
	// means "no creds forwarding" — the sandbox runs without
	// GOOGLE_APPLICATION_CREDENTIALS.
	Credentials creds.Source
	// InCluster forwards to sandbox.Options.InCluster: dial sandboxes by
	// their Service instead of port-forwarding. A creds.TokenMinter
	// Credentials source requires it.
	InCluster bool
	// Lease forwards to sandbox.Options.Lease: claims expire within
	// Lease unless renewed, so a crashed server's sandboxes go away.
	// Zero: claims don't expire.
	Lease time.Duration
	// MaxSandboxesPerUser caps the open sandboxes (one per MCP session)
	// per authenticated user. Zero: no cap.
	MaxSandboxesPerUser int
}

func (c *Config) applyDefaults() {
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.WarmPool == "" {
		c.WarmPool = "go-runtime-pool"
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

	// mu guards slots and open.
	mu    sync.Mutex
	slots map[string]*slot // by MCP session ID; stdio's is ""
	open  map[string]int   // open sandboxes by user
}

// slot is one MCP session's sandbox.
type slot struct {
	id   string
	user string // TokenInfo.UserID; "" over stdio

	// mu guards session (and only session). Held briefly during
	// lazy-open and shutdown.
	mu      sync.Mutex
	session *sandbox.Session

	// execMu serializes Execute calls so concurrent tool invocations
	// can't race on /app or clobber each other's build artifacts. The
	// SDK may dispatch tool calls concurrently; this gives the
	// session the "one-at-a-time" semantics agents actually expect.
	execMu sync.Mutex
}

// New creates a Server with cfg's defaults filled in.
func New(cfg Config) *Server {
	cfg.applyDefaults()
	return &Server{cfg: cfg, slots: map[string]*slot{}, open: map[string]int{}}
}

// newSDKServer registers kode-gopher's tools on a go-sdk server.
func (s *Server) newSDKServer() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{
		Name:    "kode-gopher",
		Version: Version,
	}, nil)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "execute_go_code",
		Description: executeGoCodeDescription,
	}, s.handleExecuteGoCode)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "gcp_auth_status",
		Description: gcpAuthStatusDescription,
	}, s.handleGCPAuthStatus)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "lookup_package_docs",
		Description: lookupPackageDocsDescription,
	}, s.handleLookupPackageDocs)
	return srv
}

// Run starts the MCP server on the stdio transport and blocks until
// ctx is cancelled or the transport closes. Always calls shutdown on
// return (using a fresh context so it completes even after ctx is
// cancelled by SIGINT).
func (s *Server) Run(ctx context.Context) error {
	runErr := s.newSDKServer().Run(ctx, &sdk.StdioTransport{})

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s.shutdown(shutdownCtx)

	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}

// slotFor returns the calling MCP session's slot, creating it on the
// session's first tool call. An HTTP session's slot is released, and
// its sandbox closed, when the session ends (client DELETE or idle
// timeout).
func (s *Server) slotFor(req *sdk.CallToolRequest) *slot {
	var id, user string
	if req != nil && req.Session != nil {
		id = req.Session.ID()
	}
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
		user = req.Extra.TokenInfo.UserID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sl, ok := s.slots[id]; ok {
		return sl
	}
	sl := &slot{id: id, user: user}
	s.slots[id] = sl
	if id != "" {
		ss := req.Session
		go func() {
			_ = ss.Wait()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s.release(ctx, id)
		}()
	}
	return sl
}

// credentialsFor is the credential source for one tool call: the
// request's own (OAuth: the signed-in user's Google token, put in the
// TokenInfo by the verifier) if it has one, else Config.Credentials.
func (s *Server) credentialsFor(req *sdk.CallToolRequest) creds.Source {
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
		if src, ok := req.Extra.TokenInfo.Extra[creds.TokenInfoKey].(creds.Source); ok {
			return src
		}
	}
	return s.cfg.Credentials
}

// ensureSession lazy-opens the slot's sandbox.Session on first use and
// reuses it on subsequent calls.
func (s *Server) ensureSession(ctx context.Context, sl *slot) (*sandbox.Session, error) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.session != nil {
		return sl.session, nil
	}
	if err := s.reserve(sl.user); err != nil {
		return nil, err
	}
	openCtx, cancel := context.WithTimeout(ctx, s.cfg.OpenTimeout)
	defer cancel()
	log.Printf("opening sandbox (namespace=%s pool=%s claim=%q context=%q mcp-session=%q)", s.cfg.Namespace, s.cfg.WarmPool, s.cfg.Claim, s.cfg.KubeContext, sl.id)
	sess, err := sandbox.Open(openCtx, sandbox.Options{
		Namespace:   s.cfg.Namespace,
		WarmPool:    s.cfg.WarmPool,
		ClaimName:   s.cfg.Claim,
		KubeContext: s.cfg.KubeContext,
		InCluster:   s.cfg.InCluster,
		Lease:       s.cfg.Lease,
	})
	if err != nil {
		s.unreserve(sl.user)
		return nil, fmt.Errorf("open sandbox: %w", err)
	}
	log.Printf("sandbox open: claim=%s", sess.ClaimName())
	sl.session = sess
	return sess, nil
}

// dropSession closes sess and clears it from sl, if it's still sl's
// session. Used when the sandbox died, so the next call opens a new one.
func (s *Server) dropSession(ctx context.Context, sl *slot, sess *sandbox.Session) {
	sl.mu.Lock()
	current := sl.session == sess
	if current {
		sl.session = nil
	}
	sl.mu.Unlock()
	_ = sess.Close(ctx)
	if current {
		s.unreserve(sl.user)
	}
}

func (s *Server) reserve(user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max := s.cfg.MaxSandboxesPerUser; max > 0 && s.open[user] >= max {
		return fmt.Errorf("too many open sandboxes for this user (limit %d); end another MCP session first", max)
	}
	s.open[user]++
	return nil
}

func (s *Server) unreserve(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[user]--; s.open[user] <= 0 {
		delete(s.open, user)
	}
}

// release removes a slot and closes (or disconnects from, if
// Persistent) its sandbox. It waits for a running tool call to finish.
func (s *Server) release(ctx context.Context, id string) {
	s.mu.Lock()
	sl, ok := s.slots[id]
	delete(s.slots, id)
	s.mu.Unlock()
	if !ok {
		return
	}
	sl.execMu.Lock()
	defer sl.execMu.Unlock()
	sl.mu.Lock()
	sess := sl.session
	sl.session = nil
	sl.mu.Unlock()
	if sess == nil {
		return
	}
	defer s.unreserve(sl.user)
	var err error
	if s.cfg.Persistent {
		log.Printf("disconnecting (claim %s preserved; reattach with --claim=%s)", sess.ClaimName(), sess.ClaimName())
		err = sess.Disconnect(ctx)
	} else {
		log.Printf("closing sandbox %s", sess.ClaimName())
		err = sess.Close(ctx)
	}
	if err != nil {
		log.Printf("mcp: release sandbox %s: %v", sess.ClaimName(), err)
	}
}

// shutdown releases every slot. Called when the transport closes.
func (s *Server) shutdown(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.slots))
	for id := range s.slots {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.release(ctx, id)
		}()
	}
	wg.Wait()
}

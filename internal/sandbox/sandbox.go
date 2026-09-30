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

// Package sandbox is kode-gopher's thin wrapper over
// sigs.k8s.io/agent-sandbox/clients/go/sandbox. Replaces our previous
// dependency on github.com/gke-demos/go-runtime-sandbox/pkg/goruntime
// so we own the bug-fix surface and can extend it (e.g., expose the
// SandboxReadyTimeout option, which the slice 1.7 GKE smoketest
// needed but goruntime didn't surface).
//
// The shape mirrors what we actually use from goruntime today —
// Open / Execute / Reset / Disconnect / Close / ClaimName — plus the
// truncation and multi-file tar-shipping behavior we depend on. We
// deliberately don't expose every knob the agent-sandbox client
// supports; add them when there's a real caller.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	sb "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// Options configures Open. Required: WarmPool. Everything else has a
// default appropriate for our local kind / GKE Autopilot setups.
type Options struct {
	// Namespace where the SandboxClaim is created. Default: "default".
	Namespace string

	// WarmPool is the SandboxWarmPool to claim from. Required: v1beta1
	// claims name a pool, not a template. With no ready sandbox in the
	// pool, the controller cold-starts one from the pool's template.
	WarmPool string

	// ClaimName, if non-empty, reattaches to that existing sandbox
	// rather than creating a new one. Useful for development across
	// server restarts (pair with Session.Disconnect on shutdown).
	ClaimName string

	// SandboxReadyTimeout bounds how long we wait for the controller
	// to resolve the SandboxClaim to a backing pod. Zero uses the
	// agent-sandbox default (180s). Slice 1.7 noted this is sometimes
	// tight on cold GKE Autopilot nodes — bump to 5min there.
	SandboxReadyTimeout time.Duration

	// PerAttemptTimeout is the ResponseHeaderTimeout on the HTTP client
	// used to POST /execute against the in-pod sandbox server. The
	// upstream default is 60s, which the in-pod server treats as an
	// effective total-exec cap (it buffers response and writes headers
	// only when the command exits). Zero uses defaultPerAttemptTimeout
	// (3min) — comfortably above any real prewarmed build/run cycle
	// but still short enough that a truly hung pod fails fast.
	PerAttemptTimeout time.Duration

	// KubeContext, if non-empty, selects a specific kubeconfig context
	// rather than inheriting ambient `kubectl config current-context`.
	// Closes the slice-1.5 / slice-1.7 context-drift footgun: the
	// server + smoketest scripts can now pin the cluster explicitly
	// without relying on out-of-process kubectl state. Empty preserves
	// the previous behavior (upstream client uses InClusterConfig →
	// default kubeconfig with no context override).
	KubeContext string

	// Truncate controls LLM-friendly head+tail truncation of Execute
	// stdout/stderr. Zero value applies defaults (8 KiB each).
	Truncate TruncateConfig

	// InCluster dials each sandbox by its headless Service
	// (Status.ServiceFQDN) instead of port-forwarding to the router.
	// Set it when kode-gopher runs in the cluster. Request.Credentials
	// requires it.
	InCluster bool
}

// Credentials is a short-lived Google access token for one run step.
// The in-pod sandbox-server holds it in memory for that command only
// and serves it from a GCE metadata emulator on 127.0.0.1
// (docs/design-in-cluster.md, section 4). The field names are
// sandbox-server's wire format.
type Credentials struct {
	AccessToken  string    `json:"access_token"`
	Expiry       time.Time `json:"expiry"`
	Email        string    `json:"email,omitempty"`
	Project      string    `json:"project,omitempty"`
	QuotaProject string    `json:"quota_project,omitempty"`
}

// Request is a single Execute call: ship Files under /app and run
// Command via `sh -c` in that workdir.
type Request struct {
	// Files maps destination path (may contain "/") to file content.
	// Paths not listed are left alone; /app persists across Execute
	// calls in the same session (caches, prior artifacts).
	Files map[string][]byte

	// Command is run via `sh -c` inside the sandbox.
	Command string

	// Timeout bounds this single Execute call. Zero = defaultTimeout
	// (5min). The upstream agent-sandbox HTTP layer's own cap comes
	// from Options.PerAttemptTimeout — we default that to 3min.
	Timeout time.Duration

	// Credentials, if set, is what Command runs as. It needs
	// Options.InCluster: the agent-sandbox client's Run sends only the
	// command, so this call goes straight to the sandbox's Service.
	Credentials *Credentials
}

// Result is what Execute produced, post-truncation.
type Result struct {
	Stdout          string
	Stderr          string
	ExitCode        int
	Duration        time.Duration
	StdoutTruncated bool
	StderrTruncated bool
}

// Session is an open connection to a sandbox.
type Session struct {
	client    *sb.Client
	box       *sb.Sandbox
	truncate  TruncateConfig
	ownClient bool
	inCluster bool
	// direct posts credentialed run steps to the sandbox's Service.
	direct *http.Client
}

const (
	defaultTimeout           = 5 * time.Minute
	defaultPerAttemptTimeout = 3 * time.Minute
	tarUploadName            = ".kg-upload.tar"
	// serverPort is sandbox-server's port (the agent-sandbox default).
	serverPort = 8888
	// maxExecuteResponse matches the agent-sandbox client's cap.
	maxExecuteResponse = 16 << 20
)

// Open creates a Session either by creating a new sandbox (ClaimName
// empty) or reattaching to an existing claim.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.WarmPool == "" {
		return nil, fmt.Errorf("sandbox: WarmPool is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}

	perAttempt := opts.PerAttemptTimeout
	if perAttempt == 0 {
		perAttempt = defaultPerAttemptTimeout
	}
	clientOpts := sb.Options{
		WarmPoolName:      opts.WarmPool,
		Namespace:         opts.Namespace,
		PerAttemptTimeout: perAttempt,
		// Route by the sandbox's headless Service, not the pod IP the
		// client read when it opened. A restarted pod gets a new IP, and
		// the router kept dialing the old one (502, connection refused).
		DisablePodIPRouting: true,
	}
	if opts.InCluster {
		clientOpts.Connectivity = sb.ConnectivityInClusterService
	}
	if opts.SandboxReadyTimeout > 0 {
		clientOpts.SandboxReadyTimeout = opts.SandboxReadyTimeout
	}
	if opts.KubeContext != "" {
		// Upstream's default kubeconfig loader uses empty ConfigOverrides
		// and ignores context selection. To honor --context we build a
		// *rest.Config here with an explicit CurrentContext override.
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.KubeContext}
		rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("sandbox: build rest.Config for context %q: %w", opts.KubeContext, err)
		}
		clientOpts.RestConfig = rc
	}

	client, err := sb.NewClient(ctx, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("sandbox: new client: %w", err)
	}

	var box *sb.Sandbox
	if opts.ClaimName != "" {
		box, err = client.GetSandbox(ctx, opts.ClaimName, opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("sandbox: reattach %q: %w", opts.ClaimName, err)
		}
	} else {
		box, err = client.CreateSandbox(ctx, opts.WarmPool, opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("sandbox: create sandbox: %w", err)
		}
	}

	tc := opts.Truncate
	if tc.HeadBytes == 0 && tc.TailBytes == 0 {
		tc.HeadBytes = defaultHeadBytes
		tc.TailBytes = defaultTailBytes
	}

	return &Session{
		client:    client,
		box:       box,
		truncate:  tc,
		ownClient: true,
		inCluster: opts.InCluster,
		direct:    &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: perAttempt}},
	}, nil
}

// ClaimName returns the underlying sandbox claim name. Persist this
// if you want to reattach in a future Open call.
func (s *Session) ClaimName() string { return s.box.ClaimName() }

// Execute materializes req.Files under /app and runs req.Command via
// `sh -c`. Files not listed in req.Files are left untouched; /app
// state persists across calls in this session.
//
// Routing for Files: keys without "/" use the sandbox client's Write
// directly; any key containing "/" triggers the tar path (build
// in-memory, single Write of the archive, server-side `tar -xf` +
// remove). The agent-sandbox client's Write doesn't accept path
// separators, hence the tar fallback.
func (s *Session) Execute(ctx context.Context, req Request) (*Result, error) {
	if req.Command == "" && len(req.Files) == 0 {
		return nil, fmt.Errorf("sandbox: Request must set Command or Files")
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	if err := s.materialize(callCtx, req.Files); err != nil {
		return nil, err
	}
	if req.Command == "" {
		return &Result{Duration: time.Since(start)}, nil
	}

	var raw *sb.ExecutionResult
	var err error
	if req.Credentials != nil {
		raw, err = s.runWithCredentials(callCtx, req.Command, req.Credentials)
	} else {
		raw, err = s.box.Run(callCtx, req.Command, sb.WithTimeout(timeout))
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: run: %w", classifySessionErr(err))
	}
	stdout, outTrunc := truncate(raw.Stdout, s.truncate)
	stderr, errTrunc := truncate(raw.Stderr, s.truncate)
	return &Result{
		Stdout:          stdout,
		Stderr:          stderr,
		ExitCode:        raw.ExitCode,
		Duration:        time.Since(start),
		StdoutTruncated: outTrunc,
		StderrTruncated: errTrunc,
	}, nil
}

func (s *Session) runWithCredentials(ctx context.Context, command string, c *Credentials) (*sb.ExecutionResult, error) {
	if !s.inCluster {
		return nil, errors.New("credentials need in-cluster connectivity (Options.InCluster)")
	}
	fqdn := s.box.ServiceFQDN()
	if fqdn == "" {
		return nil, errors.New("sandbox has no Service to address (spec.service unset on the template)")
	}
	return postExecute(ctx, s.direct, "http://"+net.JoinHostPort(fqdn, strconv.Itoa(serverPort)), command, c)
}

// postExecute sends one /execute with credentials, once: a run isn't
// idempotent, so unlike the client's other calls it is never retried.
// Errors never include the request body.
func postExecute(ctx context.Context, hc *http.Client, baseURL, command string, c *Credentials) (*sb.ExecutionResult, error) {
	payload, err := json.Marshal(struct {
		Command     string       `json:"command"`
		Credentials *Credentials `json:"credentials"`
	}{command, c})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/execute", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &sb.HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "run"}
	}
	result := sb.ExecutionResult{ExitCode: -1}
	lr := io.LimitedReader{R: resp.Body, N: maxExecuteResponse}
	if err := json.NewDecoder(&lr).Decode(&result); err != nil {
		if lr.N <= 0 {
			return nil, fmt.Errorf("%w: command output too large", sb.ErrResponseTooLarge)
		}
		return nil, fmt.Errorf("decode run result: %w", err)
	}
	return &result, nil
}

func (s *Session) materialize(ctx context.Context, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	for name := range files {
		if err := validatePath(name); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
	}

	if !needsTar(files) {
		for name, content := range files {
			if err := s.box.Write(ctx, name, content); err != nil {
				return fmt.Errorf("sandbox: write %q: %w", name, err)
			}
		}
		return nil
	}

	archive, err := buildTar(files)
	if err != nil {
		return fmt.Errorf("sandbox: build tar: %w", err)
	}
	if err := s.box.Write(ctx, tarUploadName, archive); err != nil {
		return fmt.Errorf("sandbox: upload tar: %w", err)
	}
	cmd := fmt.Sprintf("tar -xf %s && rm -f %s", tarUploadName, tarUploadName)
	res, err := s.box.Run(ctx, cmd, sb.WithTimeout(60*time.Second))
	if err != nil {
		return fmt.Errorf("sandbox: tar extract: %w", classifySessionErr(err))
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: tar extract failed (exit %d): %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// Reset wipes /app (the sandbox workdir) while preserving $GOCACHE
// and $GOMODCACHE. Use between logical operations when you want a
// clean filesystem without paying for a new sandbox.
//
// Note that the controller / pod / cache state survive; only /app's
// content goes. Returns an error only if the underlying shell call
// itself fails — a non-zero exit from `rm` (e.g. "no matches") is
// treated as success.
func (s *Session) Reset(ctx context.Context) error {
	if _, err := s.box.Run(ctx, "rm -rf -- * .[!.]* 2>/dev/null; true"); err != nil {
		return fmt.Errorf("sandbox: reset: %w", classifySessionErr(err))
	}
	return nil
}

// Disconnect drops the network connection to the sandbox but leaves
// the SandboxClaim and pod alive. Use when handing the ClaimName
// back to a caller for later reattach.
func (s *Session) Disconnect(ctx context.Context) error {
	return s.box.Disconnect(ctx)
}

// Close deletes the sandbox claim, tearing down the pod. If this
// Session owns its agent-sandbox client (i.e., Open created it), the
// client's owned sandboxes are also cleaned up.
func (s *Session) Close(ctx context.Context) error {
	err := s.box.Close(ctx)
	if s.ownClient {
		s.client.DeleteAll(ctx)
	}
	return err
}

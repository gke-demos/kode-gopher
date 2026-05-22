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
	"context"
	"fmt"
	"time"

	sb "sigs.k8s.io/agent-sandbox/clients/go/sandbox"
)

// Options configures Open. Required: Template. Everything else has a
// default appropriate for our local kind / GKE Autopilot setups.
type Options struct {
	// Namespace where the SandboxClaim is created. Default: "default".
	Namespace string

	// Template is the SandboxTemplate name to claim from. Required.
	Template string

	// ClaimName, if non-empty, reattaches to that existing sandbox
	// rather than creating a new one. Useful for development across
	// server restarts (pair with Session.Disconnect on shutdown).
	ClaimName string

	// SandboxReadyTimeout bounds how long we wait for the controller
	// to resolve the SandboxClaim to a backing pod. Zero uses the
	// agent-sandbox default (180s). Slice 1.7 noted this is sometimes
	// tight on cold GKE Autopilot nodes — bump to 5min there.
	SandboxReadyTimeout time.Duration

	// Truncate controls LLM-friendly head+tail truncation of Execute
	// stdout/stderr. Zero value applies defaults (8 KiB each).
	Truncate TruncateConfig
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
	// (5min). The agent-sandbox HTTP layer caps at ~60s regardless,
	// so values larger than that only affect our own accounting.
	Timeout time.Duration
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
}

const (
	defaultTimeout = 5 * time.Minute
	tarUploadName  = ".kg-upload.tar"
)

// Open creates a Session either by creating a new sandbox (ClaimName
// empty) or reattaching to an existing claim.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.Template == "" {
		return nil, fmt.Errorf("sandbox: Template is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}

	clientOpts := sb.Options{
		TemplateName: opts.Template,
		Namespace:    opts.Namespace,
	}
	if opts.SandboxReadyTimeout > 0 {
		clientOpts.SandboxReadyTimeout = opts.SandboxReadyTimeout
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
		box, err = client.CreateSandbox(ctx, opts.Template, opts.Namespace)
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

	raw, err := s.box.Run(callCtx, req.Command, sb.WithTimeout(timeout))
	if err != nil {
		return nil, fmt.Errorf("sandbox: run: %w", err)
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
		return fmt.Errorf("sandbox: tar extract: %w", err)
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
		return fmt.Errorf("sandbox: reset: %w", err)
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

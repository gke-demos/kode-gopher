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

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/gke-demos/kode-gopher/internal/mcp"
)

// runServe is `kode-gopher serve`: starts the MCP server on stdio,
// holding one sandbox.Session for the lifetime of the process, or on
// streamable HTTP, with one sandbox per MCP session.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	namespace := fs.String("namespace", "default", "Kubernetes namespace for the sandbox claim (must already exist)")
	kubeCtx := fs.String("context", "", "kubeconfig context for the sandbox cluster (empty = ambient `kubectl config current-context`)")
	claim := fs.String("claim", "", "reattach to an existing sandbox claim instead of creating a new one on first tool call")
	persistent := fs.Bool("persistent", false, "on shutdown, Disconnect from the sandbox (preserve for reattach) instead of Close (delete it)")
	openTO := fs.Duration("open-timeout", 5*time.Minute, "max time spent opening the sandbox on first tool call")
	execTO := fs.Duration("exec-timeout", 90*time.Second, "per-phase sandbox /execute timeout (bounded upstream by PerAttemptTimeout, default 3min)")
	inCluster := fs.Bool("in-cluster", false, "dial sandboxes by their in-cluster Service instead of port-forwarding (kode-gopher running in the cluster)")
	transport := fs.String("transport", "stdio", "stdio, or http (streamable HTTP at <addr>/mcp, one sandbox per MCP session)")
	addr := fs.String("addr", ":8080", "http: listen address")
	tokenFile := fs.String("auth-token-file", "", "http: file holding the static bearer token clients must send (required for http)")
	authUser := fs.String("auth-user", "operator", "http: the user the static token authenticates as")
	sessionTO := fs.Duration("session-timeout", 15*time.Minute, "http: end MCP sessions idle this long, closing their sandboxes")
	lease := fs.Duration("claim-lease", 10*time.Minute, "http: sandbox claims expire this long after the last renewal, so a crashed server's sandboxes go away")
	maxPerUser := fs.Int("max-sandboxes-per-user", 2, "http: open sandboxes (MCP sessions with a sandbox) allowed per user; 0 = no cap")
	credMode := fs.String("credentials", credForwarded, "how the snippet gets Google credentials: forwarded (copy local ADC into the sandbox) or access-token (mint a short-lived token from ADC and serve it to the run only; needs --in-cluster)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kode-gopher serve [flags]\n\nMCP server over stdio or streamable HTTP. Registers execute_go_code, gcp_auth_status and lookup_package_docs.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	var verifier auth.TokenVerifier
	switch *transport {
	case "stdio":
		*lease, *maxPerUser = 0, 0
	case "http":
		if *claim != "" || *persistent {
			log.Printf("mcp serve: --claim and --persistent are stdio-only")
			return 2
		}
		token, err := readToken(*tokenFile)
		if err != nil {
			log.Printf("mcp serve: %v", err)
			return 2
		}
		verifier = mcp.StaticTokenVerifier(token, *authUser)
	default:
		log.Printf("mcp serve: --transport must be stdio or http, got %q", *transport)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	credSrc, err := credentialSource(ctx, *credMode, *inCluster)
	if err != nil {
		log.Printf("mcp serve: %v", err)
		return 2
	}
	srv := mcp.New(mcp.Config{
		Namespace:   *namespace,
		WarmPool:    "go-runtime-pool",
		Claim:       *claim,
		Persistent:  *persistent,
		OpenTimeout: *openTO,
		ExecTimeout: *execTO,
		KubeContext: *kubeCtx,
		Credentials: credSrc,
		InCluster:   *inCluster,
		Lease:       *lease,

		MaxSandboxesPerUser: *maxPerUser,
	})

	if *transport == "http" {
		err = srv.RunHTTP(ctx, mcp.HTTPOptions{Addr: *addr, Verifier: verifier, SessionTimeout: *sessionTO})
	} else {
		err = srv.Run(ctx)
	}
	if err != nil {
		log.Printf("mcp serve: %v", err)
		return 1
	}
	return 0
}

// readToken reads the static bearer token from path. Never logged.
func readToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("--transport=http needs --auth-token-file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read auth token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if len(token) < 32 {
		return "", fmt.Errorf("auth token in %s is shorter than 32 characters", path)
	}
	return token, nil
}

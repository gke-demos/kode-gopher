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
	"syscall"
	"time"

	"github.com/gke-demos/kode-gopher/internal/mcp"
)

// runServe is `kode-gopher serve`: starts the MCP server on stdio,
// holding one sandbox.Session for the lifetime of the process.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	namespace := fs.String("namespace", "default", "Kubernetes namespace for the sandbox claim (must already exist)")
	kubeCtx := fs.String("context", "", "kubeconfig context for the sandbox cluster (empty = ambient `kubectl config current-context`)")
	claim := fs.String("claim", "", "reattach to an existing sandbox claim instead of creating a new one on first tool call")
	persistent := fs.Bool("persistent", false, "on shutdown, Disconnect from the sandbox (preserve for reattach) instead of Close (delete it)")
	openTO := fs.Duration("open-timeout", 5*time.Minute, "max time spent opening the sandbox on first tool call")
	execTO := fs.Duration("exec-timeout", 90*time.Second, "per-phase sandbox /execute timeout (bounded upstream by PerAttemptTimeout, default 3min)")
	inCluster := fs.Bool("in-cluster", false, "dial sandboxes by their in-cluster Service instead of port-forwarding (kode-gopher running in the cluster)")
	credMode := fs.String("credentials", credForwarded, "how the snippet gets Google credentials: forwarded (copy local ADC into the sandbox) or access-token (mint a short-lived token from ADC and serve it to the run only; needs --in-cluster)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kode-gopher serve [flags]\n\nMCP server over stdio. Registers one tool: execute_go_code.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
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
	})

	if err := srv.Run(ctx); err != nil {
		log.Printf("mcp serve: %v", err)
		return 1
	}
	return 0
}

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
	"os"
	"time"

	"github.com/gke-demos/kode-gopher/internal/creds"
)

// runAuth is `kode-gopher auth <subcommand>`. Currently one subcommand:
// `status`. Structured this way so future auth-adjacent verbs (login,
// revoke, whoami-server) have an obvious home.
func runAuth(args []string) int {
	if len(args) == 0 {
		printAuthUsage()
		return 2
	}
	switch args[0] {
	case "status":
		return runAuthStatus(args[1:])
	case "-h", "--help", "help":
		printAuthUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown auth subcommand %q\n\n", args[0])
		printAuthUsage()
		return 2
	}
}

func printAuthUsage() {
	fmt.Fprintf(os.Stderr, "usage: kode-gopher auth <subcommand>\n\nsubcommands:\n  status  print the credential identity kode-gopher will forward into sandboxes\n")
}

// runAuthStatus prints the ambient credential identity in the same
// shape the MCP `gcp_auth_status` tool returns. Useful for confirming
// which account you're wired up as before spinning up a sandbox.
func runAuthStatus(args []string) int {
	fs := flag.NewFlagSet("auth status", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kode-gopher auth status\n\nprints mode, credential type, email, and project id\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	src := creds.NewForwarded(localADCPath(), forwardedEnv)
	id, err := src.Identity(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity lookup failed: %v\n", err)
	}
	fmt.Printf("mode:            %s\n", strOrDash(id.Mode))
	fmt.Printf("credential_type: %s\n", strOrDash(id.CredType))
	fmt.Printf("email:           %s\n", strOrDash(id.Email))
	fmt.Printf("project_id:      %s\n", strOrDash(id.ProjectID))
	if err != nil {
		return 1
	}
	return 0
}

func strOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

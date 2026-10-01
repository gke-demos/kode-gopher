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

// Package main is the kode-gopher CLI. In slice 1 the only subcommand
// is
//
//	kode-gopher exec <file.go>
//
// which normalizes the input (verbatim if it's a full package-main
// file; otherwise rewriting the package decl to main and adding the
// wrapper from internal/wrapper), ships the resulting Go files into a
// sandbox, runs Build/Run/Fetch via internal/executor, and prints
// stdout/stderr plus any structured result the wrapper produced.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gke-demos/kode-gopher/internal/creds"
	"github.com/gke-demos/kode-gopher/internal/executor"
	"github.com/gke-demos/kode-gopher/internal/normalize"
	"github.com/gke-demos/kode-gopher/internal/sandbox"
	"github.com/gke-demos/kode-gopher/internal/version"
)

const sandboxWarmPool = "go-runtime-pool"

// forwardedEnv lists host env vars copied into the sandbox if set.
// The unified creds.NewForwarded takes this as its EnvAllowList.
var forwardedEnv = []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_QUOTA_PROJECT"}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("kode-gopher: ")

	// Subcommand routing first; per-subcommand FlagSet so flags can
	// appear after the subcommand name (`exec --namespace=...` rather
	// than the stdlib `flag` default of "all flags before positionals").
	if len(os.Args) < 2 {
		printRootUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "exec":
		os.Exit(runExec(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "auth":
		os.Exit(runAuth(os.Args[2:]))
	case "version", "--version":
		fmt.Println(version.String("kode-gopher"))
		os.Exit(0)
	case "-h", "--help", "help":
		printRootUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		printRootUsage()
		os.Exit(2)
	}
}

func printRootUsage() {
	fmt.Fprintf(os.Stderr, "usage: kode-gopher <subcommand> [flags]\n\nsubcommands:\n  exec <file.go>  ship a Go file into a sandbox and run it\n  serve           start the MCP server on stdio\n  auth <verb>     inspect ambient credentials (verb: status)\n  version         print the build version\n")
}

func runExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	namespace := fs.String("namespace", "default", "Kubernetes namespace for the sandbox claim (must already exist)")
	kubeCtx := fs.String("context", "", "kubeconfig context for the sandbox cluster (empty = ambient `kubectl config current-context`)")
	openTO := fs.Duration("open-timeout", 5*time.Minute, "max time spent opening the sandbox")
	execTO := fs.Duration("exec-timeout", 90*time.Second, "per-phase sandbox /execute timeout (bounded upstream by PerAttemptTimeout, default 3min)")
	claim := fs.String("claim", "", "reattach to an existing sandbox claim instead of creating a new one")
	keep := fs.Bool("keep", false, "leave the sandbox alive on exit (Disconnect) instead of deleting it (Close)")
	extraImports := fs.String("extra-imports", "", "comma-separated import paths to add as blank imports (forces `go mod tidy` to resolve them)")
	inCluster := fs.Bool("in-cluster", false, "dial sandboxes by their in-cluster Service instead of port-forwarding (kode-gopher running in the cluster)")
	credCfg := addCredFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kode-gopher exec [flags] <file.go>\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)

	exitCode, err := run(path, *namespace, *kubeCtx, *openTO, *execTO, *claim, *keep, splitCSV(*extraImports), *inCluster, credCfg)
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	return exitCode
}

// splitCSV turns "a, b,c " into ["a", "b", "c"]; empty input → nil.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func run(path, namespace, kubeContext string, openTimeout, execTimeout time.Duration, claim string, keep bool, extraImports []string, inCluster bool, credCfg *credFlags) (int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	norm, err := normalize.Normalize(map[string][]byte{"main.go": src}, normalize.Options{ExtraImports: extraImports})
	if err != nil {
		return 0, fmt.Errorf("normalize %s: %w", path, err)
	}
	log.Printf("normalize: mode=%s files=%v", norm.Mode, fileKeys(norm.Files))

	// go.mod is bootstrapped from the sandbox image's prewarm lockfile
	// inside internal/executor (baseGoModPath). Not synthesized here.
	// If normalize provides its own go.mod (multi-file, slice-4), that
	// wins — the executor's cp is gated on `[ ! -f go.mod ]`.
	files := map[string][]byte{}
	for k, v := range norm.Files {
		files[k] = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	credSrc, err := credCfg.source(ctx, inCluster)
	if err != nil {
		return 0, err
	}
	credFiles, envs, err := credSrc.Materialize(ctx)
	if err != nil {
		return 0, fmt.Errorf("materialize credentials: %w", err)
	}
	for k, v := range credFiles {
		files[k] = v
	}
	var runCreds *sandbox.Credentials
	if m, ok := credSrc.(creds.TokenMinter); ok {
		tok, err := m.AccessToken(ctx)
		if err != nil {
			return 0, err
		}
		runCreds = &sandbox.Credentials{AccessToken: tok.Token, Expiry: tok.Expiry,
			Email: tok.Email, Project: tok.Project, QuotaProject: tok.QuotaProject}
		log.Printf("run phase gets a minted access token (%s, expires %s)", tok.Email, tok.Expiry.Format(time.RFC3339))
	} else if _, ok := credFiles[".kode-gopher/creds/adc.json"]; ok {
		log.Printf("forwarding ADC from %s", localADCPath())
	} else {
		log.Printf("no local ADC at %s — GCP calls will fail unless the sandbox has its own creds", localADCPath())
	}

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	log.Printf("opening sandbox (namespace=%s pool=%s claim=%q context=%q)", namespace, sandboxWarmPool, claim, kubeContext)
	sess, err := sandbox.Open(openCtx, sandbox.Options{
		Namespace:   namespace,
		WarmPool:    sandboxWarmPool,
		KubeContext: kubeContext,
		ClaimName:   claim,
		InCluster:   inCluster,
	})
	if err != nil {
		return 0, fmt.Errorf("open sandbox: %w", err)
	}
	log.Printf("sandbox open: claim=%s", sess.ClaimName())

	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 60*time.Second)
		defer c()
		if keep {
			if dErr := sess.Disconnect(shutdownCtx); dErr != nil {
				log.Printf("disconnect: %v", dErr)
				return
			}
			log.Printf("disconnected; reattach with --claim=%s", sess.ClaimName())
			return
		}
		if cErr := sess.Close(shutdownCtx); cErr != nil {
			log.Printf("close: %v", cErr)
		}
	}()

	outcome, err := executor.Run(ctx, sess, executor.Request{
		Files:       files,
		Env:         envs,
		Credentials: runCreds,
		Timeout:     execTimeout,
	})
	if err != nil {
		return 0, err
	}

	printOutcome(outcome)
	return outcome.ExitCode, nil
}

// printOutcome renders an Outcome in a format that lets a human or LLM
// see the phase, exit code, both streams, and the structured result
// at a glance.
func printOutcome(o *executor.Outcome) {
	fmt.Printf("phase=%s  exit=%d  tidied=%v  (%s, build %s)\n", o.Phase, o.ExitCode, o.Tidied,
		o.Duration.Round(time.Millisecond), o.BuildDuration.Round(time.Millisecond))
	for _, w := range o.Warnings {
		fmt.Print("\nwarning: ", ensureNewline(w))
	}
	if o.Stdout != "" {
		fmt.Print("\n── stdout ──\n", ensureNewline(o.Stdout))
	}
	if o.Stderr != "" {
		fmt.Print("\n── stderr ──\n", ensureNewline(o.Stderr))
	}
	if o.Result != nil {
		fmt.Print("\n── result ──\n")
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(o.Result)
	}
	if o.Stdout == "" && o.Stderr == "" && o.Result == nil {
		fmt.Println("\n(no output)")
	}
}

func ensureNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func localADCPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".config", "gcloud", "application_default_credentials.json")
}

func fileKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

const (
	credForwarded   = "forwarded"
	credAccessToken = "access-token"
	credService     = "service"
)

// credFlags are exec's and serve's --credentials settings.
type credFlags struct {
	mode           *string
	serviceAccount *string
}

func addCredFlags(fs *flag.FlagSet) *credFlags {
	return &credFlags{
		mode:           fs.String("credentials", credForwarded, "how the snippet gets Google credentials: forwarded (copy local ADC into the sandbox), access-token (mint a short-lived token from ADC and serve it to the run only), or service (the same, for --service-account, impersonated with ADC); access-token and service need --in-cluster"),
		serviceAccount: fs.String("service-account", "", "--credentials=service: the Google service account snippets run as; kode-gopher's ADC needs roles/iam.serviceAccountTokenCreator on it"),
	}
}

// source builds the --credentials source. access-token and service need
// in-cluster connectivity: the token travels with the run request
// straight to the sandbox's Service.
func (f *credFlags) source(ctx context.Context, inCluster bool) (creds.Source, error) {
	mode := *f.mode
	if *f.serviceAccount != "" && mode != credService {
		return nil, fmt.Errorf("--service-account needs --credentials=%s", credService)
	}
	switch mode {
	case credForwarded:
		return creds.NewForwarded(localADCPath(), forwardedEnv), nil
	case credAccessToken, credService:
		if !inCluster {
			return nil, fmt.Errorf("--credentials=%s needs --in-cluster", mode)
		}
		if mode == credAccessToken {
			return creds.NewMinted(ctx)
		}
		if *f.serviceAccount == "" {
			return nil, fmt.Errorf("--credentials=%s needs --service-account", credService)
		}
		imp, err := creds.NewImpersonator(ctx)
		if err != nil {
			return nil, err
		}
		return creds.NewService(imp, *f.serviceAccount, os.Getenv("GOOGLE_CLOUD_PROJECT"), os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT"))
	}
	return nil, fmt.Errorf("--credentials must be %q, %q or %q, got %q", credForwarded, credAccessToken, credService, mode)
}

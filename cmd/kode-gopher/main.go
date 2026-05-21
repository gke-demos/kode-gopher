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
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gke-demos/go-runtime-sandbox/pkg/goruntime"

	"github.com/gke-demos/kode-gopher/internal/executor"
	"github.com/gke-demos/kode-gopher/internal/normalize"
)

const (
	sandboxNamespace = "default"
	sandboxTemplate  = "go-runtime-template"
	adcInSandbox     = "/app/.kode-gopher/creds/adc.json"
)

// forwardedEnv lists host env vars copied into the sandbox if set.
// Same explicit allow-list as slice 0; a real forwarding policy is
// slice-4 work.
var forwardedEnv = []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_QUOTA_PROJECT"}

func main() {
	var (
		openTO = flag.Duration("open-timeout", 5*time.Minute, "max time spent opening the sandbox")
		execTO = flag.Duration("exec-timeout", 90*time.Second, "per-phase sandbox /execute timeout (upstream caps at ~60s regardless)")
		claim  = flag.String("claim", "", "reattach to an existing sandbox claim instead of creating a new one")
		keep   = flag.Bool("keep", false, "leave the sandbox alive on exit (Disconnect) instead of deleting it (Close)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: kode-gopher exec [flags] <file.go>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 2 || flag.Arg(0) != "exec" {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(1)

	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("kode-gopher: ")

	exitCode, err := run(path, *openTO, *execTO, *claim, *keep)
	if err != nil {
		log.Fatalf("%v", err)
	}
	os.Exit(exitCode)
}

func run(path string, openTimeout, execTimeout time.Duration, claim string, keep bool) (int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	norm, err := normalize.Normalize(src)
	if err != nil {
		return 0, fmt.Errorf("normalize %s: %w", path, err)
	}
	log.Printf("normalize: mode=%s files=%v", norm.Mode, fileKeys(norm.Files))

	// Add go.mod (synthesized; sandbox runs `go mod tidy` to resolve
	// the user's imports) and forwarded credentials.
	files := map[string][]byte{
		"go.mod": []byte("module kode_gopher_user\n\ngo 1.26\n"),
	}
	for k, v := range norm.Files {
		files[k] = v
	}

	envs := collectForwardedEnv()
	if adc, ok := readLocalADC(); ok {
		files[".kode-gopher/creds/adc.json"] = adc
		envs["GOOGLE_APPLICATION_CREDENTIALS"] = adcInSandbox
		log.Printf("forwarding ADC from %s", localADCPath())
	} else {
		log.Printf("no local ADC at %s — GCP calls will fail unless the sandbox has its own creds", localADCPath())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	log.Printf("opening sandbox (namespace=%s template=%s claim=%q)", sandboxNamespace, sandboxTemplate, claim)
	sess, err := goruntime.Open(openCtx, goruntime.Options{
		Namespace: sandboxNamespace,
		Template:  sandboxTemplate,
		ClaimName: claim,
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
		Files:   files,
		Env:     envs,
		Timeout: execTimeout,
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
	fmt.Printf("phase=%s  exit=%d  (%s)\n", o.Phase, o.ExitCode, o.Duration.Round(time.Millisecond))
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

func collectForwardedEnv() map[string]string {
	out := map[string]string{}
	for _, k := range forwardedEnv {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func localADCPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".config", "gcloud", "application_default_credentials.json")
}

func readLocalADC() ([]byte, bool) {
	p := localADCPath()
	if p == "" {
		return nil, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("read ADC at %s: %v", p, err)
		}
		return nil, false
	}
	return b, true
}

func fileKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

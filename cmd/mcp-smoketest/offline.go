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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stopServer, when set, stops the `kode-gopher serve --transport=http`
// subprocess. fatalf calls it so a failed check doesn't leave the
// server (and its sandbox) behind.
var stopServer func()

func fatalf(format string, args ...any) {
	if stopServer != nil {
		stopServer()
	}
	log.Fatalf(format, args...)
}

func fatal(args ...any) { fatalf("%s", fmt.Sprint(args...)) }

// runOffline is --offline: the checks that need no Google credentials
// (CI's kind e2e). kode-gopher runs with no ADC, so gcp_auth_status
// must report mode=none; snippets build and run but call no Google API.
func runOffline(ctx context.Context, session *sdk.ClientSession, namespace string) {
	fmt.Println("\n========== offline snippet (testdata/offline_snippet.go) ==========")
	out, err := callExecuteGoCode(ctx, session, "testdata/offline_snippet.go")
	if err != nil {
		fatalf("[offline] call: %v", err)
	}
	fmt.Printf("phase=%s mode=%s exit=%d tidied=%t duration=%dms\n", out.Phase, out.Mode, out.ExitCode, out.Tidied, out.DurationMS)
	if out.Phase != "run" || out.ExitCode != 0 {
		fatalf("[offline] phase=%s exit=%d (want run, 0). stderr:\n%s", out.Phase, out.ExitCode, out.Stderr)
	}
	if out.Tidied {
		fatal("[offline] the build ran go mod tidy: storage and option should come from the prewarmed lockfile")
	}
	if out.Result == nil || out.Result.Kind != "ok" {
		fatalf("[offline] want result.kind=ok, got %+v", out.Result)
	}
	var val struct {
		Go     string `json:"go"`
		Bucket string `json:"bucket"`
		ADC    bool   `json:"adc_in_environment"`
		Links  bool   `json:"service_links"`
	}
	if err := json.Unmarshal(out.Result.Value, &val); err != nil {
		fatalf("[offline] decode result.value: %v", err)
	}
	if val.Bucket != "kode-gopher-offline" || !strings.HasPrefix(val.Go, "go1.") {
		fatalf("[offline] unexpected result.value: %s", out.Result.Value)
	}
	if val.ADC {
		fatal("[offline] GOOGLE_APPLICATION_CREDENTIALS is set in the sandbox, but kode-gopher has no credentials")
	}
	if val.Links {
		fatal("[offline] SANDBOX_ROUTER_SVC_SERVICE_HOST is set in the sandbox; the template should have enableServiceLinks: false")
	}
	fmt.Printf("  result.kind=ok go=%s bucket=%s\n", val.Go, val.Bucket)

	checkClaimLeases(ctx, namespace)

	fmt.Println("\n========== build error ==========")
	out, err = callExecuteGoCodeSource(ctx, session, "package main\n\nfunc main() { undefinedCall() }\n")
	if err != nil {
		fatalf("[build-error] call: %v", err)
	}
	if out.Phase != "build" || out.ExitCode == 0 || !strings.Contains(out.Stderr, "undefined: undefinedCall") {
		fatalf("[build-error] want phase=build, nonzero exit and the compiler error; got phase=%s exit=%d stderr:\n%s", out.Phase, out.ExitCode, out.Stderr)
	}
	fmt.Printf("  phase=build exit=%d\n", out.ExitCode)

	fmt.Println("\n========== panic (testdata/panic_snippet.go) ==========")
	out, err = callExecuteGoCode(ctx, session, "testdata/panic_snippet.go")
	if err != nil {
		fatalf("[panic] call: %v", err)
	}
	if out.Phase != "run" || out.Result == nil || out.Result.Kind != "panic" ||
		!strings.Contains(out.Result.Message, "kode-gopher demo panic") || out.Result.Stack == "" {
		fatalf("[panic] want phase=run and a panic result with message and stack; got phase=%s result=%+v", out.Phase, out.Result)
	}
	fmt.Println("  result.kind=panic, with message and stack")

	checkLargeResults(ctx, session)

	fmt.Println("\n========== gcp_auth_status ==========")
	auth := callAuthStatus(ctx, session)
	if auth.Mode != "none" {
		fatalf("gcp_auth_status: mode=%q, want none (no ADC)", auth.Mode)
	}
	fmt.Printf("  mode=%s\n", auth.Mode)

	fmt.Println("\n========== lookup_package_docs ==========")
	checkPackageDocs(ctx, session)

	fmt.Println("\n✅ MCP smoketest (offline) complete")
}

// checkClaimLeases: while the session holds a sandbox, every claim in
// the namespace carries a lease (spec.lifecycle.shutdownTime), over
// stdio as well as HTTP, so a server killed without a clean shutdown
// can't leak its sandbox, and the expiry is in the near future (set,
// and renewed, from now). Needs a namespace holding only this session's
// claims, as the kind e2e's does.
func checkClaimLeases(ctx context.Context, namespace string) {
	fmt.Println("\n========== claim leases ==========")
	// #nosec G204 -- the operator's own namespace flag.
	out, err := exec.CommandContext(ctx, "kubectl", "-n", namespace, "get", "sandboxclaims",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.spec.lifecycle.shutdownTime}{"\n"}{end}`).Output()
	if err != nil {
		fatalf("[leases] kubectl get sandboxclaims: %v", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		fatal("[leases] no sandbox claims while a session holds a sandbox")
	}
	for _, l := range lines {
		name, expiry, _ := strings.Cut(l, "=")
		if expiry == "" {
			fatalf("[leases] claim %s has no spec.lifecycle.shutdownTime: a killed server would leak it", name)
		}
		at, err := time.Parse(time.RFC3339, expiry)
		if err != nil {
			fatalf("[leases] claim %s: shutdownTime %q: %v", name, expiry, err)
		}
		// The default lease is 10 min; allow for clock skew with kind.
		if left := time.Until(at); left <= 0 || left > 15*time.Minute {
			fatalf("[leases] claim %s expires %s (in %s): want within the next lease", name, expiry, left.Round(time.Second))
		}
		fmt.Printf("  %s expires %s unless renewed\n", name, expiry)
	}
}

// checkLargeResults: a result well past stdout's 16 KiB truncation
// comes back whole, and one past the result size limit comes back as
// a warning and an error, not as a silently missing result.
func checkLargeResults(ctx context.Context, session *sdk.ClientSession) {
	snippet := func(n int) string {
		return fmt.Sprintf("package snippet\n\nimport (\n\t\"context\"\n\t\"strings\"\n)\n\nfunc run(ctx context.Context) (any, error) {\n\treturn strings.Repeat(\"x\", %d), nil\n}\n", n)
	}

	fmt.Println("\n========== 40 KiB result ==========")
	out, err := callExecuteGoCodeSource(ctx, session, snippet(40<<10))
	if err != nil {
		fatalf("[large-result] call: %v", err)
	}
	var s string
	if out.Result == nil || out.Result.Kind != "ok" || json.Unmarshal(out.Result.Value, &s) != nil || len(s) != 40<<10 {
		fatalf("[large-result] want result.kind=ok with a 40960-byte string; got result=%v warnings=%q", out.Result != nil, out.Warnings)
	}
	fmt.Println("  result.kind=ok, all 40960 bytes")

	fmt.Println("\n========== 300 KiB result (over the limit) ==========")
	out, err = callExecuteGoCodeSource(ctx, session, snippet(300<<10))
	if err != nil {
		fatalf("[too-large-result] call: %v", err)
	}
	if out.Result != nil || !strings.Contains(strings.Join(out.Warnings, " "), "over the 256 KiB limit") {
		fatalf("[too-large-result] want no result and a size warning; got result=%v warnings=%q", out.Result != nil, out.Warnings)
	}
	fmt.Printf("  no result, warning: %s\n", out.Warnings[len(out.Warnings)-1])
}

// connectHTTP starts `kode-gopher serve --transport=http` on a free
// local port with a fresh static token, checks that a request without
// the token is refused, and connects with it.
func connectHTTP(ctx context.Context, serverPath, namespace string, serverArgs []string) (*sdk.ClientSession, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	_ = l.Close()

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b)
	dir, err := os.MkdirTemp("", "mcp-smoketest-")
	if err != nil {
		return nil, err
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return nil, err
	}

	args := append([]string{"serve", "--namespace=" + namespace, "--transport=http", "--addr=" + addr, "--auth-token-file=" + tokenFile}, serverArgs...)
	// #nosec G204 -- the operator's own flags.
	cmd := exec.Command(serverPath, args...)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stopServer = func() {
		// SIGINT: serve closes its sessions' sandboxes on the way out.
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = os.RemoveAll(dir)
	}

	base := "http://" + addr
	if err := waitHealthy(ctx, base, exited); err != nil {
		return nil, err
	}
	resp, err := http.Post(base+"/mcp", "application/json", strings.NewReader(`{}`)) //nolint:noctx // local server, short-lived
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, fmt.Errorf("POST /mcp without a token: HTTP %d, want 401", resp.StatusCode)
	}
	fmt.Printf("http: serving at %s/mcp; no token gets 401\n", base)

	client := sdk.NewClient(&sdk.Implementation{Name: "mcp-smoketest", Version: "0.1.0"}, nil)
	return client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   base + "/mcp",
		HTTPClient: &http.Client{Transport: bearer(token)},
	}, nil)
}

func waitHealthy(ctx context.Context, base string, exited <-chan struct{}) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("kode-gopher serve exited before it was healthy")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		resp, err := http.Get(base + "/healthz") //nolint:noctx // local server, short-lived
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return fmt.Errorf("kode-gopher serve not healthy at %s/healthz after 30s", base)
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2/google"
)

func testCreds() *credentials {
	return &credentials{
		AccessToken: "ya29.test-token",
		Expiry:      time.Now().Add(30 * time.Minute),
		Email:       "user@example.com",
		Project:     "test-project",
	}
}

func startTestEmulator(t *testing.T, c *credentials) *metadataEmulator {
	t.Helper()
	e, err := startMetadataEmulator(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func mdGet(t *testing.T, addr, path string, flavor bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if flavor {
		req.Header.Set("Metadata-Flavor", "Google")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := resp.Header.Get("Metadata-Flavor"); got != "Google" {
		t.Errorf("GET %s: response Metadata-Flavor = %q", path, got)
	}
	return resp.StatusCode, string(body)
}

func TestMetadataEmulatorEndpoints(t *testing.T) {
	e := startTestEmulator(t, testCreds())
	const sa = "/computeMetadata/v1/instance/service-accounts/"
	for _, tc := range []struct {
		path   string
		flavor bool
		status int
		want   string
	}{
		{"/", false, 200, "computeMetadata/"},
		{"/computeMetadata/v1/project/project-id", true, 200, "test-project"},
		{"/computeMetadata/v1/project/project-id", false, 403, ""},
		{"/computeMetadata/v1/universe/universe-domain", true, 200, "googleapis.com"},
		{sa + "default/email", true, 200, "user@example.com"},
		{sa + "default/scopes", true, 200, cloudPlatformScope},
		{sa + "default/token", true, 200, `"access_token":"ya29.test-token"`},
		{sa + "default/token?scopes=a,b", true, 200, `"token_type":"Bearer"`},
		{sa + "default/?recursive=true", true, 200, `"email":"user@example.com"`},
		{sa + "user@example.com/token", true, 200, `"access_token":"ya29.test-token"`},
		{sa + "other@example.com/token", true, 404, ""},
		{sa + "default/identity?audience=x", true, 404, ""},
		{"/computeMetadata/v1/instance/zone", true, 404, ""},
	} {
		status, body := mdGet(t, e.addr, tc.path, tc.flavor)
		if status != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("GET %s (flavor=%v) = %d %q; want %d containing %q", tc.path, tc.flavor, status, body, tc.status, tc.want)
		}
	}
}

func TestMetadataEmulatorExpiredToken(t *testing.T) {
	c := testCreds()
	e := startTestEmulator(t, c)
	c.Expiry = time.Now().Add(-time.Second)
	if status, _ := mdGet(t, e.addr, "/computeMetadata/v1/instance/service-accounts/default/token", true); status != 404 {
		t.Errorf("expired token: status %d, want 404", status)
	}
}

// TestMetadataEmulatorADC checks the path snippets take: Go's ADC, with
// no credentials file, falls through to the metadata server named by
// GCE_METADATA_HOST.
func TestMetadataEmulatorADC(t *testing.T) {
	e := startTestEmulator(t, testCreds())
	t.Setenv("GCE_METADATA_HOST", e.addr)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	creds, err := google.FindDefaultCredentials(context.Background(), cloudPlatformScope)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ProjectID != "test-project" {
		t.Errorf("ProjectID = %q, want test-project", creds.ProjectID)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "ya29.test-token" {
		t.Errorf("AccessToken = %q", tok.AccessToken)
	}
	if d := time.Until(tok.Expiry); d < 25*time.Minute || d > 31*time.Minute {
		t.Errorf("token expires in %v, want ~30m", d)
	}
}

// TestHelperFetchToken is not a test: TestExecuteWithCredentials runs the
// test binary as a command in the sandbox, and this fetches the token
// from the emulator the way a client library would.
func TestHelperFetchToken(t *testing.T) {
	if os.Getenv("KG_HELPER") != "1" {
		t.Skip("helper process only")
	}
	req, _ := http.NewRequest(http.MethodGet,
		"http://"+os.Getenv("GCE_METADATA_HOST")+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("token=%s project=%s\n", tok.AccessToken, os.Getenv("GOOGLE_CLOUD_PROJECT"))
}

func executeWithCreds(t *testing.T, ts string, command string, c any) (int, executeResponse, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"command": command, "credentials": c})
	resp, err := http.Post(ts+"/execute", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out executeResponse
	_ = json.Unmarshal(body, &out)
	return resp.StatusCode, out, string(body)
}

func TestExecuteWithCredentials(t *testing.T) {
	ts, _ := newTestServer(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := fmt.Sprintf(`echo "host=$GCE_METADATA_HOST"; KG_HELPER=1 %q -test.run='^TestHelperFetchToken$'`, bin)
	status, out, body := executeWithCreds(t, ts.URL, cmd, testCreds())
	if status != http.StatusOK || out.ExitCode != 0 {
		t.Fatalf("status %d: %s", status, body)
	}
	if !strings.Contains(out.Stdout, "token=ya29.test-token project=test-project") {
		t.Errorf("command didn't get the token from the emulator:\n%s", out.Stdout)
	}

	// Once the command is done, the emulator's port is closed.
	host, _, _ := strings.Cut(strings.TrimPrefix(out.Stdout, "host="), "\n")
	if host == "" {
		t.Fatalf("no GCE_METADATA_HOST in output:\n%s", out.Stdout)
	}
	if conn, err := net.DialTimeout("tcp", host, time.Second); err == nil {
		conn.Close()
		t.Errorf("emulator at %s still accepting connections after the command ended", host)
	}
}

func TestExecuteWithoutCredentialsHasNoEmulator(t *testing.T) {
	ts, _ := newTestServer(t)
	out, err := execute(t, context.Background(), ts, `echo "host=${GCE_METADATA_HOST:-unset}"`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "host=" + envOrUnset("GCE_METADATA_HOST"); strings.TrimSpace(out.Stdout) != want {
		t.Errorf("stdout = %q, want %q", out.Stdout, want)
	}
}

func envOrUnset(k string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return "unset"
}

func TestExecuteRejectsBadCredentials(t *testing.T) {
	ts, _ := newTestServer(t)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	for name, c := range map[string]map[string]any{
		"no token":   {"expiry": future},
		"no expiry":  {"access_token": "x"},
		"expired":    {"access_token": "x", "expiry": past},
		"bad expiry": {"access_token": "x", "expiry": "soon"},
	} {
		status, _, body := executeWithCreds(t, ts.URL, "true", c)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, status, body)
		}
	}
}

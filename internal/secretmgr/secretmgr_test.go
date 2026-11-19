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

package secretmgr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseURIValidAndInvalid(t *testing.T) {
	valid := []struct {
		in       string
		wantName string
		wantLoc  string
	}{
		{
			in:       "sm://projects/my-proj/secrets/kg-oauth-keyring",
			wantName: "projects/my-proj/secrets/kg-oauth-keyring/versions/latest",
		},
		{
			in:       "sm://projects/1067056737933/secrets/_kg_token/versions/3",
			wantName: "projects/1067056737933/secrets/_kg_token/versions/3",
		},
		{
			in:       "sm://projects/google.com:corp-proj/locations/us-central1/secrets/regional-sec",
			wantName: "projects/google.com:corp-proj/locations/us-central1/secrets/regional-sec/versions/latest",
			wantLoc:  "us-central1",
		},
		{
			in:       "sm://projects/my-proj/locations/europe-west1/secrets/regional-sec/versions/v1_prod",
			wantName: "projects/my-proj/locations/europe-west1/secrets/regional-sec/versions/v1_prod",
			wantLoc:  "europe-west1",
		},
	}
	for _, tc := range valid {
		ref, err := ParseURI(tc.in)
		if err != nil {
			t.Fatalf("ParseURI(%q) unexpected error: %v", tc.in, err)
		}
		if ref.ResourceName() != tc.wantName || ref.Location != tc.wantLoc {
			t.Fatalf("ParseURI(%q) = %+v (name=%q), want name=%q loc=%q", tc.in, ref, ref.ResourceName(), tc.wantName, tc.wantLoc)
		}
	}

	invalid := []string{
		"/etc/kode-gopher/oauth/keyring",
		"sm://",
		"sm://projects//secrets/foo",
		"sm://projects/../secrets/foo",
		"sm://projects/-badproj/secrets/foo",
		"sm://projects/p/secrets/-badsecret",
		"sm://projects/p/secrets/foo/versions/-badflag",
		"sm://projects/p/secrets/foo/versions/_badver",
		"sm://projects/p/locations/-badloc/secrets/foo",
		"sm://projects/p/secrets/foo.bar",
		"sm://projects/p/secrets/foo/versions/1?alt=media",
		"sm://projects/p/secrets/foo/versions/1#frag",
		"sm://projects/p/secrets/foo/extra",
		"sm://projects/p/locations/UPPER/secrets/foo",
		"sm://projects/p/locations/us-central1/secrets/foo/versions/",
	}
	for _, bad := range invalid {
		if got, err := ParseURI(bad); err == nil {
			t.Errorf("ParseURI(%q) succeeded with %+v, want error", bad, got)
		}
	}
}

func TestLoaderReadsLocalFileAndNilReceiver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	want := "local-file-payload\n"
	if err := os.WriteFile(p, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	var nilLoader *Loader
	got, err := nilLoader.Read(context.Background(), p)
	if err != nil || string(got) != want {
		t.Fatalf("nilLoader.Read(local) = %q, %v; want %q", got, err, want)
	}
}

func TestLoaderAccessesSecretManagerAndVerifiesCRC32C(t *testing.T) {
	payload := []byte(`{"web":{"client_id":"cid","client_secret":"super-sensitive-value"}}`)
	goodCRC := strconv.FormatUint(uint64(crc32.Checksum(payload, castagnoliTable)), 10)
	urlSafePayload := []byte{0xfb, 0xff, 0xfe, 0x01, 0x02}
	urlSafeCRC := strconv.FormatUint(uint64(crc32.Checksum(urlSafePayload, castagnoliTable)), 10)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects/demo-proj/secrets/oauth-client/versions/2:access", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "projects/demo-proj/secrets/oauth-client/versions/2",
			"payload": map[string]any{
				"data":       base64.StdEncoding.EncodeToString(payload),
				"dataCrc32c": goodCRC,
			},
		})
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/empty-zero-crc/versions/1:access", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"payload":{"dataCrc32c":"0"}}`)
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/null-crc/versions/1:access", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"payload":{"data":%q,"dataCrc32c":null}}`, base64.RawURLEncoding.EncodeToString(urlSafePayload))
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/urlsafe-crc/versions/1:access", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"payload":{"data":%q,"dataCrc32c":%q}}`, base64.RawURLEncoding.EncodeToString(urlSafePayload), urlSafeCRC)
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/zero-crc-mismatch/versions/1:access", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"payload":{"data":%q,"dataCrc32c":"0"}}`, base64.StdEncoding.EncodeToString(payload))
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/empty-crc-str/versions/1:access", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"payload":{"data":%q,"dataCrc32c":""}}`, base64.StdEncoding.EncodeToString(payload))
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/corrupt/versions/latest:access", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payload": map[string]any{
				"data":       base64.StdEncoding.EncodeToString(payload),
				"dataCrc32c": "999999",
			},
		})
	})
	mux.HandleFunc("/v1/projects/demo-proj/secrets/denied/versions/1:access", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":403,"message":"Permission 'secretmanager.versions.access' denied for resource.","status":"PERMISSION_DENIED"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	loader := &Loader{Client: srv.Client(), Endpoint: srv.URL}

	got, err := loader.Read(context.Background(), "sm://projects/demo-proj/secrets/oauth-client/versions/2")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("Read(valid) = %q, %v", got, err)
	}
	if empty, err := loader.Read(context.Background(), "sm://projects/demo-proj/secrets/empty-zero-crc/versions/1"); err != nil || len(empty) != 0 {
		t.Fatalf("Read(empty-zero-crc) = %q, %v", empty, err)
	}
	if gotURLSafe, err := loader.Read(context.Background(), "sm://projects/demo-proj/secrets/null-crc/versions/1"); err != nil || string(gotURLSafe) != string(urlSafePayload) {
		t.Fatalf("Read(null-crc urlsafe) = %x, %v", gotURLSafe, err)
	}
	if gotURLSafe2, err := loader.Read(context.Background(), "sm://projects/demo-proj/secrets/urlsafe-crc/versions/1"); err != nil || string(gotURLSafe2) != string(urlSafePayload) {
		t.Fatalf("Read(urlsafe-crc) = %x, %v", gotURLSafe2, err)
	}
	for _, badURI := range []string{
		"sm://projects/demo-proj/secrets/zero-crc-mismatch/versions/1",
		"sm://projects/demo-proj/secrets/corrupt",
	} {
		_, err = loader.Read(context.Background(), badURI)
		if err == nil || !strings.Contains(err.Error(), "CRC32C mismatch") {
			t.Fatalf("Read(%s) err = %v, want CRC32C mismatch", badURI, err)
		}
		if strings.Contains(err.Error(), "super-sensitive-value") {
			t.Fatalf("error leaked secret payload: %v", err)
		}
	}
	if _, err := loader.Read(context.Background(), "sm://projects/demo-proj/secrets/empty-crc-str/versions/1"); err == nil || !strings.Contains(err.Error(), "invalid dataCrc32c") {
		t.Fatalf("Read(empty-crc-str) err = %v, want invalid dataCrc32c", err)
	}
	if _, err := loader.Read(context.Background(), "sm://projects/demo-proj/secrets/denied/versions/1"); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("Read(denied) err = %v, want HTTP 403", err)
	}

	capped := &Loader{Client: srv.Client(), Endpoint: srv.URL, MaxBytes: 16}
	if _, err := capped.Read(context.Background(), "sm://projects/demo-proj/secrets/oauth-client/versions/2"); err == nil || !strings.Contains(err.Error(), "exceeds 16 bytes") {
		t.Fatalf("Read(MaxBytes=16) err = %v, want exceeds 16 bytes", err)
	}
}

func TestLoaderAutoClientSurvivesCancelledFirstContext(t *testing.T) {
	adcFile := filepath.Join(t.TempDir(), "adc.json")
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer tokenSrv.Close()

	adcJSON := fmt.Sprintf(`{"type":"authorized_user","client_id":"cid","client_secret":"csec","refresh_token":"rtok","token_uri":%q}`, tokenSrv.URL)
	if err := os.WriteFile(adcFile, []byte(adcJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adcFile)

	smSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"payload":{"data":%q}}`, base64.StdEncoding.EncodeToString([]byte("ok-secret")))
	}))
	defer smSrv.Close()

	loader := &Loader{Endpoint: smSrv.URL}
	ctx1, cancel1 := context.WithCancel(context.Background())
	if got, err := loader.Read(ctx1, "sm://projects/p/secrets/s/versions/1"); err != nil || string(got) != "ok-secret" {
		t.Fatalf("first Read = %q, %v", got, err)
	}
	// Cancel ctx1 and invalidate cached token by clearing autoClient's token?
	// Even better: force a fresh token mint by initializing httpClient under a
	// cancelled ctx1 before the first token is minted!
	cancel1()

	loader2 := &Loader{Endpoint: smSrv.URL}
	ctx2, cancel2 := context.WithCancel(context.Background())
	if _, err := loader2.httpClient(ctx2); err != nil {
		t.Fatalf("httpClient: %v", err)
	}
	cancel2() // Cancelled BEFORE the first token mint on autoClient!
	if got, err := loader2.Read(context.Background(), "sm://projects/p/secrets/s/versions/1"); err != nil || string(got) != "ok-secret" {
		t.Fatalf("Read after first ctx cancelled = %q, %v (autoClient retained cancelled ctx!)", got, err)
	}
}

func TestEndpointForGlobalAndRegionalRefs(t *testing.T) {
	var l Loader
	globalRef, err := ParseURI("sm://projects/p/secrets/s/versions/1")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.endpointFor(globalRef); got != "https://secretmanager.googleapis.com" {
		t.Errorf("global endpoint = %q", got)
	}
	regionalRef, err := ParseURI("sm://projects/p/locations/us-central1/secrets/s/versions/1")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.endpointFor(regionalRef); got != "https://secretmanager.us-central1.rep.googleapis.com" {
		t.Errorf("regional endpoint = %q", got)
	}
}

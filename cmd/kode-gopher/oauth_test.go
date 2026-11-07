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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGroupsClientRejectsNonEmail(t *testing.T) {
	_, err := groupsClient(context.Background(), "kode-gopher-groups")
	if err == nil || !strings.Contains(err.Error(), "isn't a service account email") {
		t.Errorf("groupsClient(no @) error = %v, want a not-an-email error", err)
	}
}

func TestParseDomainMapAndProviders(t *testing.T) {
	m, err := parseDomainMap("Google.com=/etc/a.json, gke.ninja=/etc/b.json", "--flag")
	if err != nil || m["google.com"] != "/etc/a.json" || m["gke.ninja"] != "/etc/b.json" {
		t.Fatalf("parseDomainMap = %v, %v", m, err)
	}
	for _, bad := range []string{"no-equals", "=empty-domain", "google.com=", "a.com=1,A.COM=2"} {
		if _, err := parseDomainMap(bad, "--flag"); err == nil {
			t.Errorf("parseDomainMap(%q) succeeded, want error", bad)
		}
	}

	clientFile := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(clientFile, []byte(`{"web":{"client_id":"cid","client_secret":"csec"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildVaultProviders(
		map[string]string{"google.com": clientFile},
		map[string]string{"google.com": "projects/p/locations/l/authProviders/corp"},
		nil, false,
	); err != nil {
		t.Fatalf("buildVaultProviders valid: %v", err)
	}
	if _, err := buildVaultProviders(
		map[string]string{"google.com": clientFile},
		nil, nil, false,
	); err == nil {
		t.Error("buildVaultProviders missing vault auth provider succeeded")
	}
	if _, err := buildVaultProviders(
		nil,
		map[string]string{"google.com": "projects/p/locations/l/authProviders/corp"},
		nil, false,
	); err == nil {
		t.Error("buildVaultProviders missing client file succeeded")
	}
	if _, err := buildSealedProviders(map[string]string{"google.com": clientFile}, false); err != nil {
		t.Fatalf("buildSealedProviders valid: %v", err)
	}
}

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
	"strings"
	"testing"
	"time"
)

func TestMetadataEmulatorCustomScopes(t *testing.T) {
	c := &credentials{
		AccessToken: "test-readonly-token",
		Expiry:      time.Now().Add(30 * time.Minute),
		Email:       "ro@example.com",
		Project:     "test-project",
		Scopes: []string{
			"https://www.googleapis.com/auth/cloud-platform.read-only",
			"  ",
			"https://www.googleapis.com/auth/logging.read",
		},
	}
	e := startTestEmulator(t, c)
	status, scopesBody := mdGet(t, e.addr, "/computeMetadata/v1/instance/service-accounts/default/scopes", true)
	wantScopes := "https://www.googleapis.com/auth/cloud-platform.read-only\nhttps://www.googleapis.com/auth/logging.read\n"
	if status != 200 || scopesBody != wantScopes {
		t.Errorf("default/scopes = %d %q, want 200 %q", status, scopesBody, wantScopes)
	}
	status, recBody := mdGet(t, e.addr, "/computeMetadata/v1/instance/service-accounts/default/?recursive=true", true)
	if status != 200 || strings.Contains(recBody, `"https://www.googleapis.com/auth/cloud-platform"`) ||
		!strings.Contains(recBody, `"https://www.googleapis.com/auth/cloud-platform.read-only"`) {
		t.Errorf("recursive scopes = %d %s", status, recBody)
	}
}

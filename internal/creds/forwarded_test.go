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

package creds

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestForwarded_Materialize_MissingADC — no ADC file means empty
// files map + just the env allowlist. Not an error (GCP calls will
// fail inside the sandbox, which is the legible failure mode).
func TestForwarded_Materialize_MissingADC(t *testing.T) {
	f := NewForwarded(filepath.Join(t.TempDir(), "nope.json"), []string{"GOOGLE_CLOUD_PROJECT"})
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-proj")
	files, envs, err := f.Materialize(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected no files, got %v", files)
	}
	if envs["GOOGLE_CLOUD_PROJECT"] != "test-proj" {
		t.Errorf("env not forwarded: %v", envs)
	}
	if _, ok := envs[appCredsEnv]; ok {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS should not be set when ADC absent: %v", envs)
	}
}

// TestForwarded_Materialize_WithADC — a present ADC file produces
// the expected sandbox-relative file entry + env var.
func TestForwarded_Materialize_WithADC(t *testing.T) {
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	body := []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`)
	if err := os.WriteFile(adc, body, 0600); err != nil {
		t.Fatal(err)
	}
	f := NewForwarded(adc, nil)
	files, envs, err := f.Materialize(context.Background())
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got := files[sandboxADCRelPath]; string(got) != string(body) {
		t.Errorf("ADC bytes at %q = %q, want %q", sandboxADCRelPath, string(got), string(body))
	}
	if envs[appCredsEnv] != sandboxADCAbsPath {
		t.Errorf("%s = %q, want %q", appCredsEnv, envs[appCredsEnv], sandboxADCAbsPath)
	}
}

// TestForwarded_Identity_ServiceAccount — client_email surfaces
// directly without a network round-trip; ProjectID resolves from
// quota_project_id in the JSON.
func TestForwarded_Identity_ServiceAccount(t *testing.T) {
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	body := []byte(`{
        "type": "service_account",
        "client_email": "sa@myproj.iam.gserviceaccount.com",
        "quota_project_id": "myproj"
    }`)
	if err := os.WriteFile(adc, body, 0600); err != nil {
		t.Fatal(err)
	}

	f := NewForwarded(adc, nil)
	id, err := f.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.Mode != "forwarded" {
		t.Errorf("Mode = %q, want forwarded", id.Mode)
	}
	if id.CredType != "service_account" {
		t.Errorf("CredType = %q, want service_account", id.CredType)
	}
	if id.Email != "sa@myproj.iam.gserviceaccount.com" {
		t.Errorf("Email = %q, want sa@myproj.iam.gserviceaccount.com", id.Email)
	}
	if id.ProjectID != "myproj" {
		t.Errorf("ProjectID = %q, want myproj", id.ProjectID)
	}
}

// TestForwarded_Identity_ProjectFallback — no quota_project_id in
// ADC, fall back to $GOOGLE_CLOUD_PROJECT.
func TestForwarded_Identity_ProjectFallback(t *testing.T) {
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	body := []byte(`{"type":"service_account","client_email":"sa@x.iam.gserviceaccount.com"}`)
	if err := os.WriteFile(adc, body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-proj")

	f := NewForwarded(adc, nil)
	id, err := f.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.ProjectID != "env-proj" {
		t.Errorf("ProjectID fallback = %q, want env-proj", id.ProjectID)
	}
}

// TestForwarded_Identity_NoADC — missing file returns Mode=none
// without error (caller can decide to warn or not).
func TestForwarded_Identity_NoADC(t *testing.T) {
	f := NewForwarded(filepath.Join(t.TempDir(), "nope.json"), nil)
	id, err := f.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity should be nil-err for missing ADC, got %v", err)
	}
	if id.Mode != "none" {
		t.Errorf("Mode = %q, want none", id.Mode)
	}
}

// TestForwarded_Identity_Cached — the second call is served from
// cache; we prove it by pointing at a bad path AFTER the first call
// (once idOnce fires we should never re-read).
func TestForwarded_Identity_Cached(t *testing.T) {
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	body := []byte(`{"type":"service_account","client_email":"sa@x.iam.gserviceaccount.com","quota_project_id":"p"}`)
	if err := os.WriteFile(adc, body, 0600); err != nil {
		t.Fatal(err)
	}
	f := NewForwarded(adc, nil)
	first, err := f.Identity(context.Background())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Delete the ADC — cached path must survive.
	if err := os.Remove(adc); err != nil {
		t.Fatal(err)
	}
	second, err := f.Identity(context.Background())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Errorf("Identity not cached: first=%+v second=%+v", first, second)
	}
}

// TestForwarded_Identity_BadJSON — malformed ADC returns an error
// rather than a partial Identity.
func TestForwarded_Identity_BadJSON(t *testing.T) {
	dir := t.TempDir()
	adc := filepath.Join(dir, "adc.json")
	if err := os.WriteFile(adc, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	f := NewForwarded(adc, nil)
	if _, err := f.Identity(context.Background()); err == nil {
		t.Error("expected parse error, got nil")
	}
}

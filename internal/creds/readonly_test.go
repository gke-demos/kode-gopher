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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func TestServiceReadOnly(t *testing.T) {
	const sa = "runner@proj-1.iam.gserviceaccount.com"
	var mu sync.Mutex
	var gotScopes [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope []string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		gotScopes = append(gotScopes, slices.Clone(body.Scope))
		n := len(gotScopes)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": fmt.Sprintf("tok-sa-%d", n),
			"expireTime":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)

	imp := &Impersonator{Client: srv.Client(), Endpoint: srv.URL}
	s, err := NewService(imp, sa, "", "quota-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Normal run mints cloud-platform; per-call readOnly=true mints ReadOnlyScopes
	// without colliding in the Impersonator cache.
	_, _, rwTok, err := ForRun(ctx, s, false)
	if err != nil {
		t.Fatalf("ForRun(readOnly=false): %v", err)
	}
	if rwTok.Token != "tok-sa-1" || len(rwTok.Scopes) != 0 {
		t.Errorf("rwTok = %+v", rwTok)
	}

	_, _, roTok, err := ForRun(ctx, s, true)
	if err != nil {
		t.Fatalf("ForRun(readOnly=true): %v", err)
	}
	if roTok.Token != "tok-sa-2" || !slices.Equal(roTok.Scopes, ReadOnlyScopes) {
		t.Errorf("roTok = %+v", roTok)
	}

	mu.Lock()
	if len(gotScopes) != 2 || !slices.Equal(gotScopes[0], []string{cloudPlatformScope}) || !slices.Equal(gotScopes[1], ReadOnlyScopes) {
		t.Errorf("IAM requested scopes = %v", gotScopes)
	}
	mu.Unlock()

	// Server-wide WithReadOnly(true) mints ReadOnlyScopes even when ForRun readOnly=false.
	sRO, err := NewService(imp, sa, "", "quota-1")
	if err != nil {
		t.Fatal(err)
	}
	sRO.WithReadOnly(true)
	if id, _ := sRO.Identity(ctx); !id.ReadOnly {
		t.Errorf("WithReadOnly Identity = %+v, want ReadOnly=true", id)
	}
	_, _, pinnedTok, err := ForRun(ctx, sRO, false)
	if err != nil {
		t.Fatalf("ForRun(pinned ReadOnly): %v", err)
	}
	if pinnedTok.Token != "tok-sa-2" || !slices.Equal(pinnedTok.Scopes, ReadOnlyScopes) {
		t.Errorf("pinnedTok = %+v, want cached tok-sa-2 with ReadOnlyScopes", pinnedTok)
	}
}

func TestMintedServiceAccountReadOnly(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	var mu sync.Mutex
	var jwtScopes []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		assertion := r.PostForm.Get("assertion")
		parts := strings.Split(assertion, ".")
		if len(parts) != 3 {
			http.Error(w, "bad assertion", http.StatusBadRequest)
			return
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		var claims struct {
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(payload, &claims)
		mu.Lock()
		jwtScopes = append(jwtScopes, claims.Scope)
		n := len(jwtScopes)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("tok-jwt-%d", n),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(tokenSrv.Close)

	saJSON, err := json.Marshal(map[string]string{
		"type":            "service_account",
		"project_id":      "sa-proj",
		"private_key_id":  "k1",
		"private_key":     string(pemKey),
		"client_email":    "bot@sa-proj.iam.gserviceaccount.com",
		"token_uri":       tokenSrv.URL,
		"universe_domain": "googleapis.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	adcPath := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(adcPath, saJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adcPath)
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "quota-sa")

	baseCtx := context.WithValue(context.Background(), oauth2.HTTPClient, tokenSrv.Client())
	reqCtx, cancelReq := context.WithCancel(baseCtx)
	m, err := NewMinted(reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel reqCtx to prove lazy roTS construction uses WithoutCancel(baseCtx)
	// and doesn't break future token refreshes after the first request ends.
	cancelReq()

	_, _, roTok, err := ForRun(context.Background(), m, true)
	if err != nil {
		t.Fatalf("ForRun(Minted SA, readOnly=true): %v", err)
	}
	if roTok.Token != "tok-jwt-1" || roTok.Email != "bot@sa-proj.iam.gserviceaccount.com" || roTok.Project != "sa-proj" || !slices.Equal(roTok.Scopes, ReadOnlyScopes) {
		t.Errorf("roTok = %+v", roTok)
	}

	roMinted, err := NewReadOnlyMinted(baseCtx)
	if err != nil {
		t.Fatalf("NewReadOnlyMinted: %v", err)
	}
	if id, _ := roMinted.Identity(baseCtx); !id.ReadOnly {
		t.Errorf("NewReadOnlyMinted Identity = %+v, want ReadOnly=true", id)
	}
	_, _, pinnedTok, err := ForRun(baseCtx, roMinted, false)
	if err != nil || pinnedTok.Token != "tok-jwt-2" || !slices.Equal(pinnedTok.Scopes, ReadOnlyScopes) {
		t.Errorf("NewReadOnlyMinted ForRun = %+v, %v", pinnedTok, err)
	}

	mu.Lock()
	wantScope := strings.Join(ReadOnlyScopes, " ")
	for i, s := range jwtScopes {
		if s != wantScope {
			t.Errorf("jwtScopes[%d] = %q, want %q", i, s, wantScope)
		}
	}
	mu.Unlock()
}

func TestMintedFailsClosedOnMetadataAndAuthorizedUser(t *testing.T) {
	ctx := context.Background()

	// Metadata-backed credentials have empty JSON and cannot narrow scopes.
	metaCreds := &google.Credentials{
		ProjectID:   "meta-proj",
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok-meta", Expiry: time.Now().Add(time.Hour)}),
	}
	if _, err := newMinted(ctx, metaCreds, "", true); err == nil || !strings.Contains(err.Error(), "got metadata") {
		t.Errorf("newMinted(metadata, readOnly=true) err = %v, want metadata rejection", err)
	}
	mMeta, err := newMinted(ctx, metaCreds, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ForRun(ctx, mMeta, true); err == nil || !strings.Contains(err.Error(), "got metadata") {
		t.Errorf("ForRun(metadata, readOnly=true) err = %v, want metadata rejection", err)
	}

	// authorized_user refresh tokens ignore requested scopes and return the
	// full-scope grant from initial gcloud login.
	userADC := filepath.Join(t.TempDir(), "user.json")
	if err := os.WriteFile(userADC, []byte(`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", userADC)
	if _, err := NewReadOnlyMinted(ctx); err == nil || !strings.Contains(err.Error(), `ADC type "authorized_user"`) {
		t.Errorf("NewReadOnlyMinted(authorized_user) err = %v, want authorized_user rejection", err)
	}
	mUser, err := NewMinted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ForRun(ctx, mUser, true); err == nil || !strings.Contains(err.Error(), `ADC type "authorized_user"`) {
		t.Errorf("ForRun(authorized_user, readOnly=true) err = %v, want authorized_user rejection", err)
	}
}

func TestForwardedAndOAuthUserReadOnly(t *testing.T) {
	ctx := context.Background()

	// Forwarded with missing ADC (Mode=none) forwards zero credentials.
	missing := NewForwarded(filepath.Join(t.TempDir(), "missing.json"), []string{"GOOGLE_CLOUD_PROJECT"})
	t.Setenv("GOOGLE_CLOUD_PROJECT", "proj-none")
	files, env, tok, err := ForRun(ctx, missing, true)
	if err != nil || len(files) != 0 || tok != nil || env["GOOGLE_CLOUD_PROJECT"] != "proj-none" {
		t.Errorf("ForRun(Forwarded missing, readOnly=true) = %v, %v, %v, %v", files, env, tok, err)
	}

	// Forwarded with present ADC file fails closed and never exposes adc.json.
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"service_account","client_email":"sa@p.iam.gserviceaccount.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	present := NewForwarded(adc, nil)
	if files, _, _, err := ForRun(ctx, present, true); err == nil || len(files) != 0 || !strings.Contains(err.Error(), "forwarded mode") {
		t.Errorf("ForRun(Forwarded present, readOnly=true) = files=%v, err=%v; want fail-closed", files, err)
	}

	// Full-scope OAuthUser fails closed when readOnly=true is requested (both user and client_credentials).
	fullOAuth := &OAuthUser{Token: "tok-full", Expiry: time.Now().Add(time.Hour), Email: "u@example.com"}
	if _, _, _, err := ForRun(ctx, fullOAuth, true); err == nil || !strings.Contains(err.Error(), "sign-in") {
		t.Errorf("ForRun(full OAuthUser, readOnly=true) err = %v, want fail-closed", err)
	}
	fullClientCreds := &OAuthUser{Token: "tok-cc", Expiry: time.Now().Add(time.Hour), Email: "bot@p.iam.gserviceaccount.com", ServiceAccount: true}
	if _, _, _, err := ForRun(ctx, fullClientCreds, true); err == nil || !strings.Contains(err.Error(), "client_credentials") {
		t.Errorf("ForRun(full client_credentials OAuthUser, readOnly=true) err = %v, want client_credentials fail-closed", err)
	}

	// Read-only OAuthUser succeeds and reports ReadOnlyScopes.
	roOAuth := &OAuthUser{Token: "tok-ro", Expiry: time.Now().Add(time.Hour), Email: "u@example.com", ReadOnly: true}
	if _, _, got, err := ForRun(ctx, roOAuth, true); err != nil || got.Token != "tok-ro" || !slices.Equal(got.Scopes, ReadOnlyScopes) {
		t.Errorf("ForRun(ro OAuthUser, readOnly=true) = %+v, %v", got, err)
	}
	if id, _ := roOAuth.Identity(ctx); !id.ReadOnly {
		t.Errorf("roOAuth Identity = %+v, want ReadOnly=true", id)
	}
}

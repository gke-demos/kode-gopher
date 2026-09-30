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
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// credentials is the optional "credentials" field of an /execute request:
// a short-lived Google access token the command runs as. It lives only in
// this process's memory, for the one command (docs/design-in-cluster.md,
// section 4).
type credentials struct {
	AccessToken  string    `json:"access_token"`
	Expiry       time.Time `json:"expiry"`
	Email        string    `json:"email,omitempty"`
	Project      string    `json:"project,omitempty"`
	QuotaProject string    `json:"quota_project,omitempty"`
}

func (c *credentials) validate(now time.Time) error {
	switch {
	case c.AccessToken == "":
		return errors.New("credentials.access_token must not be empty")
	case c.Expiry.IsZero():
		return errors.New("credentials.expiry is required")
	case !c.Expiry.After(now):
		return errors.New("credentials.expiry is in the past")
	}
	return nil
}

// cloudPlatformScope is what the emulator reports for the token's scopes.
// kode-gopher only forwards cloud-platform tokens.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// metadataEmulator serves one command's credentials in the shape of the
// GCE metadata server, on its own 127.0.0.1 port. Client libraries find
// it through GCE_METADATA_HOST. Closing it closes the port, so nothing
// the command leaves behind can fetch the token afterwards.
type metadataEmulator struct {
	creds *credentials
	addr  string
	srv   *http.Server
}

func startMetadataEmulator(c *credentials) (*metadataEmulator, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e := &metadataEmulator{creds: c, addr: ln.Addr().String()}
	e.srv = &http.Server{Handler: e, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

func (e *metadataEmulator) Close() error { return e.srv.Close() }

// env is what the command needs to use the emulator. GCE_METADATA_HOST
// is read by Go's cloud.google.com/go/compute/metadata (and so ADC) and
// by Python's google-auth, which pings GCE_METADATA_IP.
func (e *metadataEmulator) env() []string {
	env := []string{"GCE_METADATA_HOST=" + e.addr, "GCE_METADATA_IP=" + e.addr}
	if e.creds.Project != "" {
		env = append(env, "GOOGLE_CLOUD_PROJECT="+e.creds.Project)
	}
	if e.creds.QuotaProject != "" {
		env = append(env, "GOOGLE_CLOUD_QUOTA_PROJECT="+e.creds.QuotaProject)
	}
	return env
}

func (e *metadataEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Metadata-Flavor", "Google")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" {
		// google-auth's ping checks only the response header.
		_, _ = w.Write([]byte("computeMetadata/\n"))
		return
	}
	// Like the real server: no header, no answer. It keeps a browser or
	// a naive proxy from fetching the token by accident.
	if r.Header.Get("Metadata-Flavor") != "Google" {
		http.Error(w, "missing Metadata-Flavor: Google header", http.StatusForbidden)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/computeMetadata/v1/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	c := e.creds
	switch path {
	case "project/project-id":
		e.text(w, r, c.Project)
		return
	case "universe/universe-domain":
		e.text(w, r, "googleapis.com")
		return
	}
	rest, ok := strings.CutPrefix(path, "instance/service-accounts/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	// google-auth reads default/?recursive=true, then asks for the token
	// by the account's email.
	account, item, _ := strings.Cut(rest, "/")
	if account != "default" && (c.Email == "" || account != c.Email) {
		http.NotFound(w, r)
		return
	}
	switch item {
	case "":
		if r.URL.Query().Get("recursive") != "true" {
			e.text(w, r, "aliases\nemail\nscopes\ntoken\n")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"aliases": []string{"default"},
			"email":   c.Email,
			"scopes":  []string{cloudPlatformScope},
		})
	case "email":
		e.text(w, r, c.Email)
	case "scopes":
		e.text(w, r, cloudPlatformScope+"\n")
	case "token":
		// ?scopes= is ignored: the token's scopes were fixed at consent.
		secs := int(time.Until(c.Expiry).Seconds())
		if secs <= 0 {
			http.Error(w, "token expired", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": c.AccessToken,
			"expires_in":   secs,
			"token_type":   "Bearer",
		})
	default:
		// identity (ID tokens) and anything else: not served.
		http.NotFound(w, r)
	}
}

func (e *metadataEmulator) text(w http.ResponseWriter, r *http.Request, s string) {
	if s == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/text")
	_, _ = w.Write([]byte(s))
}

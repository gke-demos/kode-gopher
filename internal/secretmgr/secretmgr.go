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

// Package secretmgr resolves secret payloads from local files or Google Cloud
// Secret Manager (`sm://...` URIs) over the v1 REST API using Application
// Default Credentials / GKE Workload Identity. Never logs or formats secret
// payload bytes into errors.
package secretmgr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

const (
	// Scheme prefixes Secret Manager URIs accepted by server secret flags.
	Scheme = "sm://"

	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	defaultEndpoint    = "https://secretmanager.googleapis.com"
	defaultMaxBytes    = 1 << 20 // 1 MiB response envelope cap (Secret Manager max payload is 64 KiB)
	requestTimeout     = 30 * time.Second
)

var (
	projectSegmentRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
	locationSegmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	secretSegmentRE   = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,254}$`)
	versionSegmentRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
	castagnoliTable   = crc32.MakeTable(crc32.Castagnoli)
)

// Ref is a validated Google Cloud Secret Manager secret version reference.
type Ref struct {
	Project  string
	Location string // empty for global secrets
	Secret   string
	Version  string // defaults to "latest" when omitted from the URI
}

// ResourceName returns the canonical Secret Manager v1 resource path
// (projects/<p>/secrets/<s>/versions/<v> or
// projects/<p>/locations/<l>/secrets/<s>/versions/<v>).
func (r Ref) ResourceName() string {
	if r.Location != "" {
		return fmt.Sprintf("projects/%s/locations/%s/secrets/%s/versions/%s", r.Project, r.Location, r.Secret, r.Version)
	}
	return fmt.Sprintf("projects/%s/secrets/%s/versions/%s", r.Project, r.Secret, r.Version)
}

// URI returns the normalized sm:// URI for r.
func (r Ref) URI() string {
	return Scheme + r.ResourceName()
}

// IsURI reports whether source starts with the sm:// scheme (case-sensitive).
func IsURI(source string) bool {
	return strings.HasPrefix(source, Scheme)
}

// ParseURI validates an sm:// URI:
//   - sm://projects/<project>/secrets/<secret>[/versions/<version>]
//   - sm://projects/<project>/locations/<location>/secrets/<secret>[/versions/<version>]
//
// When /versions/<version> is omitted, Version defaults to "latest".
func ParseURI(raw string) (Ref, error) {
	if !IsURI(raw) {
		return Ref{}, fmt.Errorf("secret manager URI %q must start with %s", raw, Scheme)
	}
	rest := strings.TrimPrefix(raw, Scheme)
	if strings.ContainsAny(rest, "?# \t\r\n") {
		return Ref{}, fmt.Errorf("secret manager URI %q must not contain query parameters, fragments, or whitespace", raw)
	}
	parts := strings.Split(rest, "/")
	ref, ok := parseGlobalParts(parts)
	if !ok {
		ref, ok = parseRegionalParts(parts)
	}
	if !ok {
		return Ref{}, fmt.Errorf(
			"secret manager URI %q must match sm://projects/<p>/secrets/<s>[/versions/<v>] or sm://projects/<p>/locations/<l>/secrets/<s>[/versions/<v>]",
			raw,
		)
	}
	return ref, validateSegments(raw, ref)
}

func parseGlobalParts(parts []string) (Ref, bool) {
	switch {
	case len(parts) == 4 && parts[0] == "projects" && parts[2] == "secrets":
		return Ref{Project: parts[1], Secret: parts[3], Version: "latest"}, true
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "secrets" && parts[4] == "versions":
		return Ref{Project: parts[1], Secret: parts[3], Version: parts[5]}, true
	default:
		return Ref{}, false
	}
}

func parseRegionalParts(parts []string) (Ref, bool) {
	switch {
	case len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "secrets":
		return Ref{Project: parts[1], Location: parts[3], Secret: parts[5], Version: "latest"}, true
	case len(parts) == 8 && parts[0] == "projects" && parts[2] == "locations" && parts[4] == "secrets" && parts[6] == "versions":
		return Ref{Project: parts[1], Location: parts[3], Secret: parts[5], Version: parts[7]}, true
	default:
		return Ref{}, false
	}
}

func validateSegments(raw string, r Ref) error {
	if !projectSegmentRE.MatchString(r.Project) || strings.Contains(r.Project, "..") {
		return fmt.Errorf("secret manager URI %q has invalid project %q", raw, r.Project)
	}
	if r.Location != "" && !locationSegmentRE.MatchString(r.Location) {
		return fmt.Errorf("secret manager URI %q has invalid location %q", raw, r.Location)
	}
	if !secretSegmentRE.MatchString(r.Secret) {
		return fmt.Errorf("secret manager URI %q has invalid secret ID %q", raw, r.Secret)
	}
	if !versionSegmentRE.MatchString(r.Version) {
		return fmt.Errorf("secret manager URI %q has invalid version %q", raw, r.Version)
	}
	return nil
}

// Loader reads secret bytes from local files or sm:// Secret Manager URIs.
// A zero-value Loader lazily initializes an ADC-authenticated HTTP client on
// the first sm:// read, so file-only callers never touch Google credentials.
type Loader struct {
	// Client is authorized to call secretmanager.googleapis.com (ADC by default).
	Client *http.Client
	// Endpoint overrides the Secret Manager base URL for tests. When empty,
	// global secrets use https://secretmanager.googleapis.com and regional
	// secrets use https://secretmanager.<location>.rep.googleapis.com.
	Endpoint string
	// MaxBytes caps the HTTP response body read from Secret Manager (1 MiB if <= 0).
	MaxBytes int64

	mu         sync.Mutex
	autoClient *http.Client
}

// Read returns the raw secret bytes from source, which may be a local file
// path or an sm:// Secret Manager URI.
func (l *Loader) Read(ctx context.Context, source string) ([]byte, error) {
	if !IsURI(source) {
		return os.ReadFile(source) // #nosec G304 -- operator-supplied CLI config path
	}
	ref, err := ParseURI(source)
	if err != nil {
		return nil, err
	}
	if l == nil {
		l = &Loader{}
	}
	return l.accessVersion(ctx, ref)
}

func (l *Loader) httpClient(ctx context.Context) (*http.Client, error) {
	if l.Client != nil {
		return l.Client, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.autoClient != nil {
		return l.autoClient, nil
	}
	// oauth2.NewClient retains the context inside its TokenSource for future
	// token refreshes; detach cancellation so a short-lived first-call ctx
	// cannot poison subsequent Read calls while preserving context values.
	hc, err := google.DefaultClient(context.WithoutCancel(ctx), cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("secret manager ADC client: %w", err)
	}
	l.autoClient = hc
	return hc, nil
}

func (l *Loader) endpointFor(ref Ref) string {
	if l.Endpoint != "" {
		return strings.TrimRight(l.Endpoint, "/")
	}
	if ref.Location != "" {
		return fmt.Sprintf("https://secretmanager.%s.rep.googleapis.com", ref.Location)
	}
	return defaultEndpoint
}

func (l *Loader) accessVersion(ctx context.Context, ref Ref) ([]byte, error) {
	hc, err := l.httpClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("access %s: %w", ref.URI(), err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	reqURL := l.endpointFor(ref) + "/v1/" + ref.ResourceName() + ":access"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("access %s: %w", ref.URI(), err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("access %s: %w", ref.URI(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	capBytes := l.MaxBytes
	if capBytes <= 0 {
		capBytes = defaultMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, capBytes+1))
	if err != nil {
		return nil, fmt.Errorf("access %s: read response: %w", ref.URI(), err)
	}
	if int64(len(body)) > capBytes {
		return nil, fmt.Errorf("access %s: response exceeds %d bytes", ref.URI(), capBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, formatAPIError(ref, resp.StatusCode, body)
	}
	return decodeAccessPayload(ref, body)
}

func formatAPIError(ref Ref, code int, body []byte) error {
	var apiErr struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &apiErr)
	msg := strings.TrimSpace(apiErr.Error.Message)
	if len(msg) > 256 {
		msg = msg[:256]
	}
	if msg != "" {
		return fmt.Errorf("access %s: HTTP %d: %s", ref.URI(), code, msg)
	}
	return fmt.Errorf("access %s: HTTP %d", ref.URI(), code)
}

func decodeAccessPayload(ref Ref, body []byte) ([]byte, error) {
	var out struct {
		Payload *struct {
			Data       string          `json:"data"`
			DataCRC32C json.RawMessage `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("access %s: invalid JSON response", ref.URI())
	}
	if out.Payload == nil {
		return nil, errors.New("access " + ref.URI() + ": response has no payload")
	}
	raw, err := decodeProto3Bytes(out.Payload.Data)
	if err != nil {
		return nil, fmt.Errorf("access %s: payload data isn't valid base64", ref.URI())
	}
	if wantCRC, hasCRC, err := parseOptionalCRC32C(out.Payload.DataCRC32C); err != nil {
		return nil, fmt.Errorf("access %s: %w", ref.URI(), err)
	} else if hasCRC {
		if gotCRC := crc32.Checksum(raw, castagnoliTable); gotCRC != wantCRC {
			return nil, fmt.Errorf("access %s: payload CRC32C mismatch (corrupted in transit)", ref.URI())
		}
	}
	return raw, nil
}

// decodeProto3Bytes decodes proto3 JSON bytes fields, accepting both standard
// and URL-safe base64, padded or unpadded (matching google.golang.org/protobuf/encoding/protojson).
func decodeProto3Bytes(s string) ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}
	enc := base64.StdEncoding
	if strings.ContainsAny(s, "-_") {
		enc = base64.URLEncoding
	}
	if len(s)%4 != 0 {
		enc = enc.WithPadding(base64.NoPadding)
	}
	return enc.DecodeString(s)
}

func parseOptionalCRC32C(raw json.RawMessage) (uint32, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false, nil
	}
	s := string(trimmed)
	if inner, ok := strings.CutPrefix(s, `"`); ok {
		var closed bool
		s, closed = strings.CutSuffix(inner, `"`)
		if !closed || s == "" {
			return 0, false, errors.New("invalid dataCrc32c value")
		}
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false, errors.New("invalid dataCrc32c value")
	}
	return uint32(v), true, nil
}

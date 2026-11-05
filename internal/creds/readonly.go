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
	"errors"
	"fmt"
	"slices"
)

// CloudPlatformReadOnlyScope is Google's umbrella read-only Cloud scope.
const CloudPlatformReadOnlyScope = "https://www.googleapis.com/auth/cloud-platform.read-only"

// ReadOnlyScopes are the Google OAuth 2.0 scopes granted when a deployment
// or tool call opts into read-only Google Cloud credentials.
// cloud-platform.read-only covers most Cloud APIs; the service-specific
// scopes cover APIs (Cloud Storage, BigQuery, Logging, Monitoring, Trace)
// whose read methods check their own scope family.
var ReadOnlyScopes = []string{
	CloudPlatformReadOnlyScope,
	"https://www.googleapis.com/auth/devstorage.read_only",
	"https://www.googleapis.com/auth/bigquery.readonly",
	"https://www.googleapis.com/auth/logging.read",
	"https://www.googleapis.com/auth/monitoring.read",
	"https://www.googleapis.com/auth/trace.readonly",
}

// ReadOnlyTokenMinter is implemented by credential sources that can mint
// (or explicitly refuse to mint) a read-only-scoped access token for one run.
type ReadOnlyTokenMinter interface {
	Source
	ReadOnlyAccessToken(ctx context.Context) (AccessToken, error)
}

// ForRun resolves the sandbox files, run-phase environment variables, and
// optional short-lived AccessToken to forward for one snippet execution.
// When readOnly is true, ForRun enforces ReadOnlyScopes and fails closed
// rather than forwarding raw ADC files or full-scope tokens.
func ForRun(ctx context.Context, src Source, readOnly bool) (map[string][]byte, map[string]string, *AccessToken, error) {
	if src == nil {
		return nil, nil, nil, nil
	}
	if !readOnly {
		files, env, err := src.Materialize(ctx)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("materialize credentials: %w", err)
		}
		if m, ok := src.(TokenMinter); ok {
			tok, err := m.AccessToken(ctx)
			if err != nil {
				return nil, nil, nil, err
			}
			return files, env, &tok, nil
		}
		return files, env, nil, nil
	}

	if rm, ok := src.(ReadOnlyTokenMinter); ok {
		tok, err := rm.ReadOnlyAccessToken(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		files, env, err := src.Materialize(ctx)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("materialize credentials: %w", err)
		}
		if len(files) > 0 || env[appCredsEnv] != "" {
			return nil, nil, nil, errors.New("read-only credentials cannot forward credential files into the sandbox")
		}
		if len(tok.Scopes) == 0 {
			tok.Scopes = slices.Clone(ReadOnlyScopes)
		}
		return files, env, &tok, nil
	}

	if f, ok := src.(*Forwarded); ok {
		return f.materializeReadOnly()
	}

	return nil, nil, nil, fmt.Errorf("credential source %T does not support read-only credentials", src)
}

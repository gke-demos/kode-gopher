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

// offline_snippet.go is the snippet `mcp-smoketest --offline` runs (CI's
// kind e2e, which has no Google credentials). It builds a storage client
// without authentication and makes no calls, so it needs no network: it
// proves curated Google Cloud packages compile from the sandbox's
// prewarmed cache (no go mod tidy) and that the wrapper returns a
// structured result. The Logging, Monitoring and Trace clients are
// referenced too, so a curated package missing from the image's
// lockfile shows up as tidied: true. It also reports whether any
// credentials reached the sandbox, which they mustn't when kode-gopher
// has none, and whether Kubernetes service links did (the template
// turns them off).
package kode_gopher_snippet

import (
	"context"
	"os"
	"runtime"

	"cloud.google.com/go/logging/logadmin"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/storage"
	trace "cloud.google.com/go/trace/apiv1"
	"google.golang.org/api/option"
)

var (
	_ *logadmin.Client
	_ *monitoring.MetricClient
	_ *trace.Client
)

func run(ctx context.Context) (any, error) {
	c, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return map[string]any{
		"go":                 runtime.Version(),
		"bucket":             c.Bucket("kode-gopher-offline").BucketName(),
		"scope":              storage.ScopeReadOnly,
		"adc_in_environment": os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "",
		"service_links":      os.Getenv("SANDBOX_ROUTER_SVC_SERVICE_HOST") != "",
	}, nil
}

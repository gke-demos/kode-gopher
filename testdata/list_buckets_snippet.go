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

// list_buckets_snippet.go is the slice-1 demo. Same data as
// testdata/list_buckets.go, but written as a *snippet*: a non-main
// package declaring only `func run(ctx context.Context) (any, error)`.
// The normalizer rewrites the package to main and adds the wrapper,
// which calls run, marshals the returned value, and writes the
// structured result to /app/.kode-gopher/result.json. The executor
// fetches that file and surfaces it as Outcome.Result.
//
// Pass criterion (slice 1): output (from result.value, when sorted by
// timeCreated and projected to names) matches gcloud — exactly what
// the verbatim version produces in slice 0.
//
// Lives under testdata/ so the host Go toolchain ignores it.
package kode_gopher_snippet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

type bucket struct {
	Name        string    `json:"name"`
	TimeCreated time.Time `json:"timeCreated"`
}

func run(ctx context.Context) (any, error) {
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		return nil, errors.New("GOOGLE_CLOUD_PROJECT must be set in the sandbox env")
	}

	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	defer client.Close()

	out := []bucket{}
	it := client.Buckets(ctx, project)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list buckets: %w", err)
		}
		out = append(out, bucket{Name: attrs.Name, TimeCreated: attrs.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeCreated.Before(out[j].TimeCreated) })

	// Log a side note to the run phase's stderr — orthogonal to the
	// structured result, demonstrating that user logs still flow.
	fmt.Fprintf(os.Stderr, "listed %d buckets in project %s\n", len(out), project)

	return out, nil
}

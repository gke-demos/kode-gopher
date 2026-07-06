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

// multi_file_helper_snippet/main.go is the "wrapped snippet with a
// helper subpackage" demo — exercises normalize's multi-file path
// end-to-end. Root file has func run and imports a helper
// subpackage; helper subpackage exports formatting logic. Both ship
// as separate files under /app; normalize rewrites the root package
// decl to `main` and leaves the helper subpackage untouched.
//
// Import path for the helper package is kode_gopher_user/formatter —
// kode_gopher_user is the module name the executor synthesizes when
// bootstrapping /app/go.mod from the prewarm lockfile.
//
// Lives under testdata/ so the host Go toolchain ignores it.
package snippet

import (
	"context"
	"errors"
	"fmt"
	"os"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"kode_gopher_user/formatter"
)

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

	descriptions := []string{}
	it := client.Buckets(ctx, project)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list buckets: %w", err)
		}
		descriptions = append(descriptions, formatter.Describe(attrs.Name, attrs.Created))
	}

	fmt.Fprintf(os.Stderr, "described %d buckets in project %s\n", len(descriptions), project)
	return map[string]any{
		"count":   len(descriptions),
		"buckets": descriptions,
	}, nil
}

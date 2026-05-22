// PoC snippet using the REST GCS client (google.golang.org/api/storage/v1).
// Same data path as our slice-0 testdata/list_buckets.go, but written as
// "func main does everything" to fit the yaegi-poc convention.
//
// Yaegi can only call symbols from packages whose extracts we've
// loaded. Build the runner with -tags=rest before running this:
//
//	go run -tags rest . testdata/list_buckets_rest.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	storage "google.golang.org/api/storage/v1"
)

func main() {
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		fmt.Fprintln(os.Stderr, "GOOGLE_CLOUD_PROJECT must be set")
		os.Exit(1)
	}
	ctx := context.Background()
	svc, err := storage.NewService(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage.NewService: %v\n", err)
		os.Exit(1)
	}

	type bucket struct {
		Name        string `json:"name"`
		TimeCreated string `json:"timeCreated"`
	}
	out := []bucket{}
	pageToken := ""
	for {
		call := svc.Buckets.List(project).Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list: %v\n", err)
			os.Exit(1)
		}
		for _, b := range resp.Items {
			out = append(out, bucket{Name: b.Name, TimeCreated: b.TimeCreated})
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeCreated < out[j].TimeCreated })

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}

	_ = errors.New // satisfy import even if we don't use it (kept for parity with the real test)
}

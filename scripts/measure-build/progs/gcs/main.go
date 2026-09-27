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

// list_buckets.go is the canonical slice-0 demo. It connects to GCS
// using Application Default Credentials, lists the buckets in the
// project named by GOOGLE_CLOUD_PROJECT, and prints them as JSON
// sorted by creation time. Pass criterion: same data as
//
//	gcloud storage buckets list --format=json | jq '...sort_by(.creation_time) | [.[].name]'
//
// Uses cloud.google.com/go/storage (the gRPC client) on purpose — its
// transitive dep tree is what justifies the slice-0.5 prewarmed image.
// If cold `go build` of this program completes in <60s inside the
// sandbox, the prewarm is working.
//
// Lives under testdata/ so the Go toolchain ignores it during normal
// builds — this is a payload we ship *into* the sandbox.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

func main() {
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		log.Fatal("GOOGLE_CLOUD_PROJECT must be set in the sandbox env")
	}

	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("storage client: %v", err)
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
			log.Fatalf("list buckets: %v", err)
		}
		out = append(out, bucket{Name: attrs.Name, TimeCreated: attrs.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeCreated.Before(out[j].TimeCreated) })

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		log.Fatalf("encode: %v", err)
	}
	fmt.Fprintf(os.Stderr, "listed %d buckets in project %s\n", len(out), project)
}

// PoC snippet using the gRPC GCS client (cloud.google.com/go/storage).
// Same data as the REST variant; deliberately stresses Yaegi against
// the unsafe + reflect-heavy path the gRPC client takes.
//
// Build the runner with -tags=grpc before running this:
//
//	go run -tags grpc . testdata/list_buckets_grpc.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

func main() {
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		fmt.Fprintln(os.Stderr, "GOOGLE_CLOUD_PROJECT must be set")
		os.Exit(1)
	}
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage.NewClient: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	type bucket struct {
		Name        string    `json:"name"`
		TimeCreated time.Time `json:"timeCreated"`
	}
	out := []bucket{}
	it := client.Buckets(ctx, project)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "list: %v\n", err)
			os.Exit(1)
		}
		out = append(out, bucket{Name: attrs.Name, TimeCreated: attrs.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeCreated.Before(out[j].TimeCreated) })

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
}

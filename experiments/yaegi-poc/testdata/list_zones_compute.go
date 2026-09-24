// PoC snippet exercising cloud.google.com/go/compute/apiv1 — the slice-6
// gating test. Unlike the storage tests, this client is generated from
// protobuf service definitions, so it drags in protobuf message types,
// gax-go call plumbing, and grpc status/codes for error mapping.
//
// Build the runner with -tags="compute grpc" before running this
// (compute for the apiv1 + computepb extracts, grpc for the iterator one):
//
//	go run -tags "compute grpc" . testdata/list_zones_compute.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
)

func main() {
	ctx := context.Background()

	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		fail("GOOGLE_CLOUD_PROJECT is not set")
	}

	c, err := compute.NewZonesRESTClient(ctx)
	if err != nil {
		fail("NewZonesRESTClient: %v", err)
	}
	defer c.Close()

	it := c.List(ctx, &computepb.ListZonesRequest{Project: project})

	type zone struct {
		Name   string `json:"name"`
		Region string `json:"region"`
		Status string `json:"status"`
	}
	var zones []zone
	for {
		z, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			fail("Next: %v", err)
		}
		zones = append(zones, zone{
			Name:   z.GetName(),
			Region: lastSegment(z.GetRegion()),
			Status: z.GetStatus(),
		})
	}

	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })

	out, err := json.MarshalIndent(map[string]any{
		"project": project,
		"count":   len(zones),
		"zones":   zones,
	}, "", "  ")
	if err != nil {
		fail("marshal: %v", err)
	}
	fmt.Println(string(out))
}

// lastSegment turns a full resource URL into its trailing element, so
// ".../regions/us-central1" reads as "us-central1".
func lastSegment(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "snippet error: "+format+"\n", args...)
	os.Exit(1)
}

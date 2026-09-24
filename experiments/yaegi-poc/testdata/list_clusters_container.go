// PoC snippet exercising cloud.google.com/go/container/apiv1 — the real
// gRPC gating test. Unlike compute/apiv1 (REST-only despite its protobuf
// types) and storage (HTTP+JSON by default), NewClusterManagerClient
// speaks gRPC on the wire, so this is the first test that puts
// google.golang.org/grpc's transport, codec, and reflection paths under
// an interpreted caller.
//
//	go run -tags container . testdata/list_clusters_container.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	container "cloud.google.com/go/container/apiv1"
	containerpb "cloud.google.com/go/container/apiv1/containerpb"
)

func main() {
	ctx := context.Background()

	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		fail("GOOGLE_CLOUD_PROJECT is not set")
	}

	c, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		fail("NewClusterManagerClient: %v", err)
	}
	defer c.Close()

	// location "-" means every location in the project.
	resp, err := c.ListClusters(ctx, &containerpb.ListClustersRequest{
		Parent: fmt.Sprintf("projects/%s/locations/-", project),
	})
	if err != nil {
		fail("ListClusters: %v", err)
	}

	type cluster struct {
		Name     string `json:"name"`
		Location string `json:"location"`
		Status   string `json:"status"`
		Version  string `json:"version"`
		Nodes    int32  `json:"nodes"`
	}
	var clusters []cluster
	for _, cl := range resp.GetClusters() {
		clusters = append(clusters, cluster{
			Name:     cl.GetName(),
			Location: cl.GetLocation(),
			Status:   cl.GetStatus().String(),
			Version:  cl.GetCurrentMasterVersion(),
			Nodes:    cl.GetCurrentNodeCount(),
		})
	}

	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })

	out, err := json.MarshalIndent(map[string]any{
		"project":  project,
		"count":    len(clusters),
		"clusters": clusters,
		"missing":  strings.Join(resp.GetMissingZones(), ","),
	}, "", "  ")
	if err != nil {
		fail("marshal: %v", err)
	}
	fmt.Println(string(out))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "snippet error: "+format+"\n", args...)
	os.Exit(1)
}

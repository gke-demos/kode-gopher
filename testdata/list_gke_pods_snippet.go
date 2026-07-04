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

// list_gke_pods_snippet.go is the "cross-API composition" demo — the
// wedge that motivates kode-gopher in the first place. One snippet
// touches two GCP-adjacent APIs in a single agent step:
//
//  1. cloud.google.com/go/container/apiv1 — ListClusters on the
//     ambient project across all locations.
//  2. k8s.io/client-go — build a rest.Config for the picked cluster
//     (preferring the DNS endpoint uid.<region>.gke.goog, which is
//     publicly reachable via Google's control-plane routing and needs
//     no per-cluster CA; falling back to the IP endpoint + MasterAuth
//     CA cert if DNS isn't available) with a Google-authenticated
//     transport, then list pods in kube-system.
//
// Snippet compile stays fast because container/apiv1 and
// k8s.io/client-go are both in internal/curated.
//
// Cluster selection: first cluster returned by the API. Deterministic
// enough for a smoketest; no env override to avoid expanding the
// production forwardedEnv allowlist just for test-time knobs.
//
// Auth: whatever ADC the sandbox has. In forwarded mode that's the
// desktop user; in workload mode it's the sandbox pod's GSA. Either
// way, that identity needs (a) container.clusters.list on the project
// and (b) k8s RBAC in the target cluster to list pods in kube-system.
// On GKE with the auth webhook this maps automatically from Google
// identity for anyone at container.developer or above.
//
// Failure surfaces distinctly (message text, not typed error codes):
//   - "no GKE clusters in project X" — nothing to test against.
//   - "list pods ... forbidden ..." — RBAC gap on the target cluster.
//   - other wrapped errors — network / IAM / TLS / etc.
//
// Lives under testdata/ so the host Go toolchain ignores it.
package kode_gopher_snippet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"

	container "cloud.google.com/go/container/apiv1"
	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type pod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
}

type result struct {
	Cluster      string `json:"cluster"`
	Location     string `json:"location"`
	Endpoint     string `json:"endpoint"`      // the endpoint we actually connected to (DNS name if available, else IP)
	EndpointKind string `json:"endpoint_kind"` // "dns" or "ip"
	Pods         []pod  `json:"pods"`
}

func run(ctx context.Context) (any, error) {
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		return nil, errors.New("GOOGLE_CLOUD_PROJECT must be set in the sandbox env")
	}

	cm, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("container.NewClusterManagerClient: %w", err)
	}
	defer cm.Close()

	resp, err := cm.ListClusters(ctx, &containerpb.ListClustersRequest{
		Parent: fmt.Sprintf("projects/%s/locations/-", project),
	})
	if err != nil {
		return nil, fmt.Errorf("ListClusters in project %s: %w", project, err)
	}
	if len(resp.Clusters) == 0 {
		return nil, fmt.Errorf("no GKE clusters in project %s", project)
	}
	c := resp.Clusters[0]

	kc, endpoint, err := kubeClientForGKECluster(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("build clientset for cluster %s: %w", c.Name, err)
	}
	endpointKind := "ip"
	if endpoint != c.Endpoint {
		endpointKind = "dns"
	}

	pods, err := kc.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{Limit: 20})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf(
				"pod list in kube-system on cluster %s forbidden — the caller's identity has no k8s RBAC in this cluster (grant Kubernetes Engine Developer on the project, or bind a Role for the identity): %w",
				c.Name, err)
		}
		return nil, fmt.Errorf("list pods in kube-system on cluster %s (via %s endpoint %s): %w", c.Name, endpointKind, endpoint, err)
	}

	out := result{
		Cluster:      c.Name,
		Location:     c.Location,
		Endpoint:     endpoint,
		EndpointKind: endpointKind,
		Pods:         make([]pod, 0, len(pods.Items)),
	}
	for _, p := range pods.Items {
		out.Pods = append(out.Pods, pod{
			Namespace: p.Namespace,
			Name:      p.Name,
			Phase:     string(p.Status.Phase),
		})
	}

	fmt.Fprintf(os.Stderr, "cluster=%s (%s) endpoint=%s (%s) pods_in_kube_system=%d\n",
		out.Cluster, out.Location, out.Endpoint, out.EndpointKind, len(out.Pods))
	return out, nil
}

// kubeClientForGKECluster builds a kubernetes.Interface for the given
// GKE cluster, authenticated with ambient Google credentials (same ADC
// used by the container/apiv1 call above).
//
// Prefers the DNS endpoint (uid.<region>.gke.goog) when available —
// always publicly reachable via Google's control-plane routing, no
// per-cluster CA needed (Google-managed TLS on *.gke.goog), and works
// for private-endpoint clusters that would be unreachable by IP.
// Falls back to the classic IP endpoint + per-cluster CA cert if the
// DNS endpoint isn't available.
func kubeClientForGKECluster(ctx context.Context, c *containerpb.Cluster) (kubernetes.Interface, string, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, "", fmt.Errorf("google.DefaultTokenSource: %w", err)
	}
	auth := func(rt http.RoundTripper) http.RoundTripper {
		return &oauth2.Transport{Base: rt, Source: ts}
	}

	if dns := c.GetControlPlaneEndpointsConfig().GetDnsEndpointConfig().GetEndpoint(); dns != "" {
		cfg := &rest.Config{
			Host:          "https://" + dns,
			WrapTransport: auth,
		}
		kc, err := kubernetes.NewForConfig(cfg)
		return kc, dns, err
	}

	if c.MasterAuth == nil || c.MasterAuth.ClusterCaCertificate == "" {
		return nil, "", errors.New("cluster has neither a DNS endpoint nor MasterAuth.ClusterCaCertificate — cannot connect")
	}
	caPEM, err := base64.StdEncoding.DecodeString(c.MasterAuth.ClusterCaCertificate)
	if err != nil {
		return nil, "", fmt.Errorf("decode cluster CA: %w", err)
	}
	cfg := &rest.Config{
		Host:            "https://" + c.Endpoint,
		TLSClientConfig: rest.TLSClientConfig{CAData: caPEM},
		WrapTransport:   auth,
	}
	kc, err := kubernetes.NewForConfig(cfg)
	return kc, c.Endpoint, err
}

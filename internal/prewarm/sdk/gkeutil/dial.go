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

// Package gkeutil provides compact, zero-extra-dependency helpers for
// GKE diagnostics inside kode-gopher sandboxes:
//
//   - DialClientset / DialRESTConfig: build a client-go Clientset against
//     a GKE cluster's control-plane DNS endpoint using Application Default
//     Credentials.
//   - ScrubPodList / ScrubPod / ScrubNode / ScrubObject: strip managedFields,
//     applied-configuration snapshots, and credential values so Kubernetes
//     payloads stay secret-safe and well under the 256 KiB result cap.
//   - ClusterLogs / NewDrainer: Drain-style log template clustering that
//     collapses thousands of noisy pod or Cloud Logging lines into <*>
//     templates.
package gkeutil

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// ClusterInfo summarizes the resolved GKE control-plane endpoint.
type ClusterInfo struct {
	Project          string `json:"project"`
	Location         string `json:"location"`
	Cluster          string `json:"cluster"`
	Host             string `json:"host"`
	UsingDNSEndpoint bool   `json:"using_dns_endpoint"`
}

// DialClientset looks up projects/{project}/locations/{location}/clusters/{cluster}
// via the GKE Cluster Manager API and returns a ready client-go Clientset
// authenticated with Application Default Credentials. It prefers the
// cluster's DNS control-plane endpoint when external traffic is allowed and
// falls back to the IP endpoint + cluster CA certificate otherwise.
func DialClientset(ctx context.Context, project, location, cluster string) (*kubernetes.Clientset, *ClusterInfo, error) {
	cfg, info, err := DialRESTConfig(ctx, project, location, cluster)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes clientset: %w", err)
	}
	return cs, info, nil
}

// DialRESTConfig resolves a *rest.Config for the named GKE cluster using
// Application Default Credentials, suitable for kubernetes.NewForConfig
// or dynamic.NewForConfig.
func DialRESTConfig(ctx context.Context, project, location, cluster string) (*rest.Config, *ClusterInfo, error) {
	if err := validateClusterRef(project, location, cluster); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	gke, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gke cluster manager client: %w", err)
	}
	defer func() { _ = gke.Close() }()

	c, err := gke.GetCluster(ctx, &containerpb.GetClusterRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, location, cluster),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("get cluster %s/%s/%s: %w", project, location, cluster, err)
	}

	// google.DefaultTokenSource stores the passed context inside long-lived
	// token refreshers (oauth2/jwt, impersonate); detach cancellation so a
	// short dial timeout does not break later Clientset calls.
	ts, err := google.DefaultTokenSource(context.WithoutCancel(ctx), cloudPlatformScope)
	if err != nil {
		return nil, nil, fmt.Errorf("default token source: %w", err)
	}

	cfg, usingDNS, err := RESTConfigFromCluster(c, ts)
	if err != nil {
		return nil, nil, err
	}
	return cfg, &ClusterInfo{
		Project:          project,
		Location:         location,
		Cluster:          cluster,
		Host:             cfg.Host,
		UsingDNSEndpoint: usingDNS,
	}, nil
}

func validateClusterRef(project, location, cluster string) error {
	for _, part := range []struct{ label, val string }{
		{"project", project},
		{"location", location},
		{"cluster", cluster},
	} {
		if part.val == "" || strings.ContainsRune(part.val, '/') {
			return fmt.Errorf("invalid %s %q: must be non-empty and contain no '/'", part.label, part.val)
		}
	}
	return nil
}

// RESTConfigFromCluster constructs a *rest.Config from an already-fetched
// *containerpb.Cluster and oauth2.TokenSource. Exported for callers that
// already hold the Cluster proto (e.g. from ListClusters) or for offline tests.
func RESTConfigFromCluster(c *containerpb.Cluster, ts oauth2.TokenSource) (*rest.Config, bool, error) {
	if c == nil {
		return nil, false, errors.New("nil cluster")
	}
	if ts == nil {
		return nil, false, errors.New("nil token source")
	}

	cfg := &rest.Config{}
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &oauth2.Transport{Source: ts, Base: rt}
	})

	dnsCfg := c.GetControlPlaneEndpointsConfig().GetDnsEndpointConfig()
	dnsAllowed := dnsCfg != nil && (dnsCfg.AllowExternalTraffic == nil || dnsCfg.GetAllowExternalTraffic())
	if dnsAllowed && dnsCfg.GetEndpoint() != "" {
		cfg.Host = "https://" + formatEndpointHost(dnsCfg.GetEndpoint())
		return cfg, true, nil
	}

	ep := c.GetEndpoint()
	if ep == "" {
		if dnsCfg != nil && dnsCfg.GetEndpoint() != "" {
			return nil, false, fmt.Errorf("cluster %q has dnsEndpoint allow_external_traffic=false and no IP endpoint", c.GetName())
		}
		return nil, false, fmt.Errorf("cluster %q has neither reachable dnsEndpoint nor IP endpoint", c.GetName())
	}

	caB64 := c.GetMasterAuth().GetClusterCaCertificate()
	if caB64 == "" {
		return nil, false, fmt.Errorf("cluster %q IP endpoint %q has empty masterAuth.clusterCaCertificate", c.GetName(), ep)
	}
	caPEM, err := base64.StdEncoding.DecodeString(caB64)
	if err != nil || len(caPEM) == 0 {
		return nil, false, fmt.Errorf("decode cluster CA certificate: %w", err)
	}

	cfg.Host = "https://" + formatEndpointHost(ep)
	cfg.TLSClientConfig = rest.TLSClientConfig{CAData: caPEM}
	return cfg, false, nil
}

func formatEndpointHost(ep string) string {
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	if ip := net.ParseIP(ep); ip != nil && strings.ContainsRune(ep, ':') {
		return "[" + ep + "]"
	}
	return ep
}

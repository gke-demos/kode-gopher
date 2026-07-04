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

// list_k8s_version_snippet.go proves that k8s.io/client-go is prewarmed
// AND usable from the sandbox pod. Uses rest.InClusterConfig — which
// reads the KSA token + API server address from the pod filesystem — so
// the sandbox's own cluster is reachable without any host-side auth
// plumbing.
//
// Calls the API server's /version endpoint via the discovery client.
// That endpoint is unauthenticated on Kubernetes, so no RBAC needs to
// be granted to the sandbox's ServiceAccount for this snippet to
// succeed. Anything that actually needs to list/get typed resources
// (pods, deployments, ...) will additionally need a Role/RoleBinding —
// out of scope for this smoke test.
//
// Lives under testdata/ so the host Go toolchain ignores it.
package kode_gopher_snippet

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func run(ctx context.Context) (any, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("rest.InClusterConfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	info, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("Discovery.ServerVersion: %w", err)
	}
	return info, nil
}

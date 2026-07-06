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

package creds

import "context"

// Workload is the in-cluster credential source: the sandbox pod's
// KSA is bound (via Workload Identity Federation) to a GSA, and the
// GCP client libraries inside the sandbox call the metadata server
// on 169.254.169.254 to fetch tokens. No host-side file injection is
// needed — google.FindDefaultCredentials Just Works from the pod.
//
// This is a stub. Real implementation lands when kode-gopher itself
// runs in-cluster (post-slice-4). For now it exists so mcp.Config
// callers running in a mixed / conditional environment can construct
// a Source without special-casing the workload branch.
type Workload struct{}

// NewWorkload constructs a Workload source.
func NewWorkload() *Workload { return &Workload{} }

// Materialize returns empty maps: no files or env forwarded from host.
// The sandbox pod's KSA + Workload Identity binding does the auth.
func (w *Workload) Materialize(_ context.Context) (map[string][]byte, map[string]string, error) {
	return map[string][]byte{}, map[string]string{}, nil
}

// Identity returns a stub. Real impl (post-slice) hits the metadata
// server for the GSA email + project.
func (w *Workload) Identity(_ context.Context) (Identity, error) {
	return Identity{Mode: "workload", CredType: "metadata"}, nil
}

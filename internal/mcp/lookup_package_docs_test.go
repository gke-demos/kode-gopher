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

package mcp

import "testing"

func TestIsCurated(t *testing.T) {
	for pkg, want := range map[string]bool{
		// Curated packages themselves.
		"cloud.google.com/go/storage":             true,
		"cloud.google.com/go/monitoring/apiv3/v2": true,
		"k8s.io/client-go/kubernetes":             true,
		// Other packages in their modules.
		"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb": true,
		"cloud.google.com/go/trace/apiv1/tracepb":              true,
		"cloud.google.com/go/logging":                          true,
		"google.golang.org/api/iterator":                       true,
		"k8s.io/apimachinery/pkg/types":                        true,
		// Not in a curated module.
		"cloud.google.com/go/pubsub":          false,
		"cloud.google.com/go/storagetransfer": false,
		"github.com/foo/bar":                  false,
		"k8s.io/kubectl/pkg/cmd":              false,
		"":                                    false,
		// The path goes into a shell command line: none of these may pass.
		"cloud.google.com/go/storage;id":             false,
		"cloud.google.com/go/storage/$(id)":          false,
		"cloud.google.com/go/storage `id`":           false,
		"cloud.google.com/go/storage/../../etc":      false,
		"cloud.google.com/go/storage/x|cat /etc/pwd": false,
		"cloud.google.com/go/storage\nid":            false,
		"-cloud.google.com/go/storage":               false,
	} {
		if got := isCurated(pkg); got != want {
			t.Errorf("isCurated(%q) = %v, want %v", pkg, got, want)
		}
	}
}

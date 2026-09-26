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

//go:build symboldeps

// Pins the curated symbol set in go.mod.
//
// Extraction needs these packages to be real module requirements with
// complete transitive go.sum entries — `go get <module>` supplies neither,
// which is what made the first two client-go extraction attempts fail on
// missing go.sum entries after burning 24 minutes and 12 hours
// respectively. Blank imports in a file `go mod tidy` can see fixes that
// properly.
//
// Build-tagged so it never enters a normal build: nothing here is
// referenced at runtime, the packages are only ever reached through
// generated symbol tables. `go mod tidy` considers all build tags, so the
// requirements are still recorded.
package main

import (
	_ "cloud.google.com/go/compute/apiv1"
	_ "cloud.google.com/go/compute/apiv1/computepb"
	_ "cloud.google.com/go/container/apiv1"
	_ "cloud.google.com/go/container/apiv1/containerpb"
	_ "cloud.google.com/go/storage"
	_ "google.golang.org/api/iterator"
	_ "google.golang.org/api/storage/v1"
	_ "k8s.io/api/apps/v1"
	_ "k8s.io/api/batch/v1"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/api/networking/v1"
	_ "k8s.io/apimachinery/pkg/api/resource"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/rest"
	_ "k8s.io/client-go/tools/clientcmd"
)

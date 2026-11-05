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

package main

import (
	"context"
	"flag"
	"strings"
	"testing"
)

func TestCredFlagsReadOnlyValidation(t *testing.T) {
	ctx := context.Background()

	// --credentials=forwarded --read-only is rejected eagerly on exec/serve.
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := addCredFlags(fs)
	if err := fs.Parse([]string{"--credentials=forwarded", "--read-only"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cf.source(ctx, false); err == nil || !strings.Contains(err.Error(), "--read-only cannot be used with --credentials=forwarded") {
		t.Errorf("cf.source(forwarded, read-only) err = %v", err)
	}

	// Under --transport=http --auth=oauth, serveCreds skips opening --credentials=access-token
	// (which gke-server-oauth inherits from gke-server) because OAuth overrides it per request.
	fs2 := flag.NewFlagSet("test-oauth", flag.ContinueOnError)
	cf2 := addCredFlags(fs2)
	if err := fs2.Parse([]string{"--credentials=access-token", "--read-only"}); err != nil {
		t.Fatal(err)
	}
	src, err := serveCreds(ctx, "http", "oauth", true, cf2)
	if err != nil || src != nil {
		t.Errorf("serveCreds(http, oauth, access-token, read-only) = %v, %v; want nil, nil", src, err)
	}
}

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
	"flag"
	"testing"
	"time"
)

func TestClaimLease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		keep    bool
		want    time.Duration
		wantErr bool
	}{
		{"default", nil, false, 10 * time.Minute, false},
		{"explicit", []string{"--claim-lease=2m"}, false, 2 * time.Minute, false},
		{"never", []string{"--claim-lease=0"}, false, 0, false},
		{"minimum", []string{"--claim-lease=30s"}, false, 30 * time.Second, false},
		{"too short", []string{"--claim-lease=5s"}, false, 0, true},
		{"negative", []string{"--claim-lease=-1m"}, false, 0, true},
		{"keep turns it off", nil, true, 0, false},
		{"keep overrides explicit", []string{"--claim-lease=2m"}, true, 0, false},
		{"keep still validates", []string{"--claim-lease=1s"}, true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			lease := fs.Duration("claim-lease", 10*time.Minute, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := claimLease(fs, lease, tc.keep, "--keep")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && *lease != tc.want {
				t.Errorf("lease = %s, want %s", *lease, tc.want)
			}
		})
	}
}

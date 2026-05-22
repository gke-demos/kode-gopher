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

package sandbox

import (
	"strings"
	"testing"
)

func TestTruncate_PassthroughWhenWithinBudget(t *testing.T) {
	s := "small"
	got, tr := truncate(s, TruncateConfig{HeadBytes: 100, TailBytes: 100})
	if tr {
		t.Errorf("expected no truncation; got truncated=true")
	}
	if got != s {
		t.Errorf("expected passthrough; got %q", got)
	}
}

func TestTruncate_HeadTailElidesMiddle(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("b", 200) + strings.Repeat("c", 100)
	got, tr := truncate(s, TruncateConfig{HeadBytes: 10, TailBytes: 10})
	if !tr {
		t.Errorf("expected truncated=true")
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Errorf("head not preserved: %q", got[:20])
	}
	if !strings.HasSuffix(got, strings.Repeat("c", 10)) {
		t.Errorf("tail not preserved: %q", got[len(got)-20:])
	}
	if !strings.Contains(got, "bytes elided") {
		t.Errorf("elision marker missing: %q", got)
	}
}

func TestTruncate_ZeroConfigPassthrough(t *testing.T) {
	s := strings.Repeat("x", 10000)
	got, tr := truncate(s, TruncateConfig{})
	if tr || got != s {
		t.Errorf("zero config should passthrough; truncated=%v len=%d", tr, len(got))
	}
}

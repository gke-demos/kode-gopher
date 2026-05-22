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

import "fmt"

// TruncateConfig controls LLM-friendly head+tail truncation of Result
// stdout/stderr. Default head and tail are 8 KiB each.
type TruncateConfig struct {
	HeadBytes int
	TailBytes int
}

const (
	defaultHeadBytes = 8192
	defaultTailBytes = 8192
)

// truncate keeps the first HeadBytes and last TailBytes of s, with an
// elision marker in between. Returns (output, wasTruncated). If both
// HeadBytes and TailBytes are zero or s already fits, returns s
// unchanged.
func truncate(s string, c TruncateConfig) (string, bool) {
	head, tail := c.HeadBytes, c.TailBytes
	if head <= 0 && tail <= 0 {
		return s, false
	}
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	if len(s) <= head+tail {
		return s, false
	}
	elided := len(s) - head - tail
	marker := fmt.Sprintf("\n... [%d bytes elided] ...\n", elided)
	return s[:head] + marker + s[len(s)-tail:], true
}

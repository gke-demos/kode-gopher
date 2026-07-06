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

// Package formatter is the helper subpackage referenced from
// multi_file_helper_snippet/main.go. Ships to the sandbox at
// /app/formatter/format.go and stays `package formatter` — normalize
// only rewrites root-file package decls, not subdirectory files.
package formatter

import (
	"fmt"
	"time"
)

// Describe formats a bucket name and creation time as e.g.
// "my-bucket (127d old)". Deliberately trivial — the point of this
// snippet is exercising the multi-file plumbing, not the formatter.
func Describe(name string, created time.Time) string {
	if created.IsZero() {
		return name
	}
	ageDays := int(time.Since(created).Hours() / 24)
	return fmt.Sprintf("%s (%dd old)", name, ageDays)
}

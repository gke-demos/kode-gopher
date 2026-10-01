#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# mod-tidy.sh: presubmit: `go mod tidy` must be a no-op.
#
# Compares file content before and after rather than using `git diff`,
# so legitimately uncommitted go.mod edits during local development
# aren't flagged or destroyed. The working tree is left as found.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml);
# run dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

tmp=$(mktemp -d)
# shellcheck disable=SC2064
trap "rm -rf '$tmp'" EXIT
cp go.mod go.sum "$tmp/"

go mod tidy

if ! cmp -s go.mod "$tmp/go.mod" || ! cmp -s go.sum "$tmp/go.sum"; then
  diff -u "$tmp/go.mod" go.mod >&2 || true
  diff -u "$tmp/go.sum" go.sum >&2 || true
  cp "$tmp/go.mod" go.mod
  cp "$tmp/go.sum" go.sum
  echo "go.mod / go.sum are not tidy. Run 'go mod tidy' locally and commit the result." >&2
  exit 1
fi
echo "mod-tidy: OK"

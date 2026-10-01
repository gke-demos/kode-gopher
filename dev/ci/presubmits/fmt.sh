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

# fmt.sh: presubmit: gofmt -s cleanliness. golangci-lint's formatters
# check this too; this is the fast, dependency-free version.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml);
# run dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

offenders="$(gofmt -s -l cmd internal)"
if [[ -n "${offenders}" ]]; then
  echo "FAIL: the following files are not gofmt -s clean:" >&2
  sed 's/^/  - /' <<<"${offenders}" >&2
  echo "Fix with: gofmt -s -w ${offenders//$'\n'/ }" >&2
  exit 1
fi
echo "OK: cmd and internal are gofmt -s clean."

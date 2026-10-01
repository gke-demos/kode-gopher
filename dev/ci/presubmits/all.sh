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

# all.sh: run every presubmit in order, with a PASS/FAIL line per step
# and a summary at the end. Exits non-zero if any step failed (all steps
# run regardless, so one run shows every failure).
#
# CI (.github/workflows/ci.yml) runs these same scripts, split across
# parallel jobs; this is the sequential local equivalent. Adding a step
# means wiring it here AND into a ci.yml job.

set -euo pipefail

dir="$(cd "$(dirname "$0")" && pwd)"
steps=(build vet fmt lint mod-tidy go-toolchain prompts sandbox-pin test vuln)
failed=()

for step in "${steps[@]}"; do
  echo ""
  echo "=== presubmit: ${step} ==="
  if "${dir}/${step}.sh"; then
    echo "--- PASS: ${step}"
  else
    echo "--- FAIL: ${step}"
    failed+=("${step}")
  fi
done

echo ""
if ((${#failed[@]} > 0)); then
  echo "presubmits FAILED: ${failed[*]}"
  exit 1
fi
echo "presubmits PASSED: ${steps[*]}"

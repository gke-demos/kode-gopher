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

# common.sh: shared helpers for dev/tools and dev/ci scripts. Source,
# don't run. Same convention as core-agent's and mast's dev/tools/common.sh.

set -euo pipefail

repo_root() {
  (cd "$(dirname "${BASH_SOURCE[0]}")" && git rev-parse --show-toplevel)
}

# ensure_tool <binary> <go-install-target>: install a pinned Go tool on
# demand and put it on PATH. Keeps CI and laptops on the same version
# without a global install step.
ensure_tool() {
  local name="$1"
  local target="$2"
  if command -v "$name" >/dev/null 2>&1; then
    return 0
  fi
  local gobin
  gobin="${GOBIN:-$(go env GOPATH)/bin}"
  # Already installed at GOBIN but not on PATH? Just expose it.
  if [[ -x "$gobin/$name" ]]; then
    export PATH="$gobin:$PATH"
    return 0
  fi
  echo "> $name not found; installing $target into $gobin" >&2
  GOBIN="$gobin" go install "$target"
  export PATH="$gobin:$PATH"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "ensure_tool: $name still missing after install" >&2
    return 1
  fi
}

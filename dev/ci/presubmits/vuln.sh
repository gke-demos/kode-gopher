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

# vuln.sh: presubmit: govulncheck over the root module and over
# internal/prewarm, the nested module whose dependencies are baked into
# the sandbox image.
#
# The root module gets symbol-level analysis: only vulnerabilities in
# code kode-gopher reaches fail. The prewarm module gets package-level
# analysis: its main.go only blank-imports the curated packages, while
# snippets may call any of their APIs, so a vulnerability anywhere in
# an imported package counts. It's scanned with the root module's
# toolchain pin, which is what sandbox/Dockerfile builds with (the
# go-toolchain presubmit keeps them equal), so standard-library
# findings match the image.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml);
# run dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
. "$(dirname "$0")/../../tools/common.sh"
cd "$(repo_root)"

ensure_tool govulncheck golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...

toolchain="$(awk '/^toolchain/ {print $2}' go.mod)"
(cd internal/prewarm && GOTOOLCHAIN="$toolchain" govulncheck -scan package ./...)

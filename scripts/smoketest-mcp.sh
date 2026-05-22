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

#
# scripts/smoketest-mcp.sh — MCP-layer end-to-end against an
# already-deployed kode-gopher SandboxTemplate. Spawns
# `kode-gopher serve` and exercises execute_go_code via a
# programmatic MCP client (cmd/mcp-smoketest), validating the wire
# format (tool registration, structured content, IsError semantics)
# in addition to the underlying executor.
#
# Run the appropriate cluster smoketest first to bootstrap:
#   ./scripts/smoketest-kind.sh   (then: ./scripts/smoketest-mcp.sh --target=kind)
#   ./scripts/smoketest-gke.sh    (then: ./scripts/smoketest-mcp.sh --target=gke)
#
# Flags:
#   --target=kind|gke  pick context + namespace defaults (default: gke)
#   --compare          assert the wrapped-mode result.value matches `gcloud storage buckets list` chronologically
#
# Env overrides:
#   KIND_CONTEXT, GKE_CONTEXT, NS

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TARGET=gke
COMPARE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --target=*) TARGET="${1#--target=}"; shift;;
    --target)   TARGET="$2"; shift 2;;
    --compare)  COMPARE="--compare"; shift;;
    -h|--help)  sed -n '17,33p' "$0"; exit 0;;
    *)          echo "unknown arg: $1" >&2; exit 2;;
  esac
done

case "$TARGET" in
  kind) CONTEXT="${KIND_CONTEXT:-kind-kode-gopher-smoke}"; NS="${NS:-default}";;
  gke)  CONTEXT="${GKE_CONTEXT:-ap-gke-sandbox}";          NS="${NS:-codemode}";;
  *)    echo "--target must be 'kind' or 'gke'" >&2; exit 2;;
esac

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m!!  %s\033[0m\n' "$*" >&2; exit 1; }

step "preflight"
for bin in kubectl go gcloud; do
  command -v "$bin" >/dev/null 2>&1 || die "missing prerequisite: $bin"
done
: "${GOOGLE_CLOUD_PROJECT:?GOOGLE_CLOUD_PROJECT must be set in env}"
kubectl --context "$CONTEXT" get ns "$NS" >/dev/null \
  || die "namespace '$NS' not found in context '$CONTEXT' (run smoketest-${TARGET}.sh first)"
kubectl --context "$CONTEXT" -n "$NS" get sandboxtemplate go-runtime-template >/dev/null \
  || die "SandboxTemplate 'go-runtime-template' not deployed to $NS (run smoketest-${TARGET}.sh first)"
echo "context=$CONTEXT  namespace=$NS  project=$GOOGLE_CLOUD_PROJECT  target=$TARGET"

step "build binaries"
mkdir -p bin
go build -o ./bin/kode-gopher ./cmd/kode-gopher
go build -o ./bin/mcp-smoketest ./cmd/mcp-smoketest

step "switch kubectl current-context (kode-gopher serve inherits it)"
kubectl config use-context "$CONTEXT" >/dev/null

step "run cmd/mcp-smoketest"
./bin/mcp-smoketest --namespace="$NS" $COMPARE

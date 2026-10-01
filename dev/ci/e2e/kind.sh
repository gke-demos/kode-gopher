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

# kind.sh: end to end on a throwaway kind cluster, with no Google
# credentials. What .github/workflows/e2e-kind.yml runs; it works the
# same locally. Not part of presubmits/all.sh: it takes minutes and
# needs docker and kind.
#
#   1. a kind cluster, with its own kubeconfig (.e2e/kubeconfig; your
#      current context is never touched);
#   2. the agent-sandbox controller and extensions;
#   3. the sandbox image: the published
#      ghcr.io/gke-demos/kode-gopher/sandbox:<tag> when the current
#      sources' tag exists (scripts/sandbox-image-tag.sh), else built
#      from sandbox/Dockerfile; loaded into kind as
#      kode-gopher-sandbox:latest;
#   4. manifests/base (SandboxTemplate, warm pool, sandbox-router);
#   5. cmd/mcp-smoketest --offline against `kode-gopher serve`, over
#      stdio and over streamable HTTP: a snippet that compiles a
#      Google Cloud package from the prewarmed cache, a build error, a
#      panic, gcp_auth_status reporting mode=none, lookup_package_docs.
#
# The cluster is deleted on exit, unless it existed before or KEEP=1.
# On failure it prints pods, events and controller logs first.
#
# Env:
#   KIND_CLUSTER   cluster name               (kode-gopher-e2e)
#   AS_VERSION     agent-sandbox release      (v1.0.4)
#   BUILD=1        build the sandbox image even if the tag is published
#   KEEP=1         keep the cluster

set -euo pipefail
cd "$(dirname "$0")/../../.."

CLUSTER="${KIND_CLUSTER:-kode-gopher-e2e}"
AS_VERSION="${AS_VERSION:-v1.0.4}"
NS=default
IMG=kode-gopher-sandbox:latest
E2E="$PWD/.e2e"
export KUBECONFIG="$E2E/kubeconfig"

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m!!  %s\033[0m\n' "$*" >&2; exit 1; }

for bin in kind kubectl docker go; do
  command -v "$bin" >/dev/null 2>&1 || die "missing prerequisite: $bin"
done
mkdir -p "$E2E/home"

created=0
finish() {
  rc=$?
  if [[ $rc -ne 0 ]] && [[ -f "$KUBECONFIG" ]]; then
    step "diagnostics"
    kubectl get pods -A -o wide || true
    kubectl -n "$NS" get sandboxes,sandboxclaims,sandboxwarmpools || true
    kubectl get events -A --sort-by=.lastTimestamp | tail -n 50 || true
    kubectl -n agent-sandbox-system logs deployment/agent-sandbox-controller --tail=100 || true
  fi
  if [[ $created -eq 1 ]] && [[ "${KEEP:-0}" != 1 ]]; then
    step "delete kind cluster '$CLUSTER'"
    kind delete cluster --name "$CLUSTER" || true
  fi
  exit "$rc"
}
trap finish EXIT

step "kind cluster '$CLUSTER'"
if kind get clusters | grep -qx "$CLUSTER"; then
  echo "(reusing existing cluster)"
  kind export kubeconfig --name "$CLUSTER"
else
  created=1
  kind create cluster --name "$CLUSTER" --wait 120s
fi

step "agent-sandbox ${AS_VERSION}"
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AS_VERSION}/sandbox-with-extensions.yaml"
kubectl -n agent-sandbox-system get deployments -o name \
  | xargs -r -I{} kubectl -n agent-sandbox-system rollout status {} --timeout=180s

step "sandbox image"
published="ghcr.io/gke-demos/kode-gopher/sandbox:$(scripts/sandbox-image-tag.sh)"
if [[ "${BUILD:-0}" != 1 ]] && docker pull "$published"; then
  docker tag "$published" "$IMG"
else
  echo "building: $published isn't published, or BUILD=1"
  docker build -t "$IMG" -f sandbox/Dockerfile .
fi
kind load docker-image "$IMG" --name "$CLUSTER"

step "manifests/base"
kubectl apply -k manifests/base
kubectl -n "$NS" rollout status deployment/sandbox-router --timeout=180s
# A reused cluster's pool may hold sandboxes from an older image.
kubectl -n "$NS" delete sandbox -l agents.x-k8s.io/warm-pool-sandbox \
  --cascade=foreground --ignore-not-found --wait=true
ready=0
for _ in {1..90}; do
  ready=$(kubectl -n "$NS" get pods -l sandbox=kode-gopher-sandbox,agents.x-k8s.io/warm-pool-sandbox \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' | grep -c true || true)
  [[ "$ready" -ge 1 ]] && break
  sleep 2
done
[[ "$ready" -ge 1 ]] || die "warm pool not ready after 3 minutes"

step "build binaries"
go build -o ./bin/kode-gopher ./cmd/kode-gopher
go build -o ./bin/mcp-smoketest ./cmd/mcp-smoketest

# An empty HOME, so kode-gopher finds no ADC even on a workstation that
# has one.
for transport in stdio http; do
  step "mcp-smoketest --offline --transport=$transport"
  env -u GOOGLE_APPLICATION_CREDENTIALS HOME="$E2E/home" \
    ./bin/mcp-smoketest --offline --transport="$transport" --namespace="$NS"
done

printf '\n\033[1;32m✅ kind e2e passed\033[0m\n'

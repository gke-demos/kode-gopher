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
# scripts/smoketest-kind.sh — slice-0 + slice-1 end-to-end on local kind.
#
# Brings up everything kode-gopher needs to run a Go program in a sandbox:
#   1. kind cluster (created if missing; reused otherwise)
#   2. agent-sandbox controller + extensions
#   3. sandbox-router (built from kubernetes-sigs/agent-sandbox git context)
#   4. kode-gopher sandbox image (built from sandbox/Dockerfile; prewarms GCP cache)
#   5. SandboxTemplate 'go-runtime-template' (manifests/base)
# Then builds the local kode-gopher binary and runs `kode-gopher exec`
# against each TEST_FILE — by default both the verbatim variant
# (testdata/list_buckets.go) and the snippet variant
# (testdata/list_buckets_snippet.go).
#
# Flags:
#   --compare        also run `gcloud storage buckets list` and diff per file
#   --clean          delete the kind cluster on exit (otherwise reuse)
#   --file <path>    run only the given Go program (overrides the default pair)
#
# Re-runs are idempotent. To start truly clean:
#   kind delete cluster --name kode-gopher-smoke
#
# Env overrides:
#   KIND_CLUSTER      cluster name             (kode-gopher-smoke)
#   AS_VERSION        agent-sandbox release    (v0.4.6)
#   NS                target namespace         (default)
#   KODE_GOPHER_IMG   sandbox image tag        (kode-gopher-sandbox:latest)

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-kode-gopher-smoke}"
AS_VERSION="${AS_VERSION:-v0.4.6}"
NS="${NS:-default}"
KODE_GOPHER_IMG="${KODE_GOPHER_IMG:-kode-gopher-sandbox:latest}"
ROUTER_IMG="sandbox-router:${AS_VERSION}"
ROUTER_CTX="https://github.com/kubernetes-sigs/agent-sandbox.git#${AS_VERSION}:clients/python/agentic-sandbox-client/sandbox-router"
ROUTER_YAML_URL="https://raw.githubusercontent.com/kubernetes-sigs/agent-sandbox/${AS_VERSION}/clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DEFAULT_FILES=("testdata/list_buckets.go" "testdata/list_buckets_snippet.go")
TEST_FILES=("${DEFAULT_FILES[@]}")
CLEAN=0
COMPARE=0
USER_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --clean)   CLEAN=1; shift;;
    --compare) COMPARE=1; shift;;
    --file)    USER_FILE="$2"; shift 2;;
    -h|--help) sed -n '3,30p' "$0"; exit 0;;
    *)         echo "unknown arg: $1" >&2; exit 2;;
  esac
done
if [[ -n "$USER_FILE" ]]; then
  TEST_FILES=("$USER_FILE")
fi

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m!!  %s\033[0m\n' "$*"; }
die()  { printf '\033[1;31m!!  %s\033[0m\n' "$*" >&2; exit 1; }

step "preflight"
for bin in kind kubectl docker go gcloud jq curl awk diff python3; do
  command -v "$bin" >/dev/null 2>&1 || die "missing prerequisite: $bin"
done
: "${GOOGLE_CLOUD_PROJECT:?GOOGLE_CLOUD_PROJECT must be set in env}"
ADC="$HOME/.config/gcloud/application_default_credentials.json"
[[ -f "$ADC" ]] || die "no ADC at $ADC — run: gcloud auth application-default login"
for f in "${TEST_FILES[@]}"; do
  [[ -f "$f" ]] || die "test file not found: $f"
done
echo "project=$GOOGLE_CLOUD_PROJECT  test_files=${TEST_FILES[*]}"

step "ensure kind cluster '$CLUSTER'"
if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER"
else
  echo "(reusing existing cluster)"
fi
kubectl config use-context "kind-${CLUSTER}" >/dev/null

step "install agent-sandbox controller ${AS_VERSION}"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AS_VERSION}/manifest.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AS_VERSION}/extensions.yaml"
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
# Extensions may install one or more sibling deployments — wait on all of them.
kubectl -n agent-sandbox-system get deployments -o name \
  | xargs -r -I{} kubectl -n agent-sandbox-system rollout status {} --timeout=180s

step "build sandbox-router image (${ROUTER_IMG})"
if docker image inspect "$ROUTER_IMG" >/dev/null 2>&1; then
  echo "(image already built locally; skipping)"
else
  docker build -t "$ROUTER_IMG" "$ROUTER_CTX"
fi
kind load docker-image "$ROUTER_IMG" --name "$CLUSTER"

step "deploy sandbox-router into namespace '$NS'"
curl -sfL "$ROUTER_YAML_URL" \
  | sed -e "s|\${ROUTER_IMAGE}|$ROUTER_IMG|g" \
        -e "s|# imagePullPolicy: Never|imagePullPolicy: IfNotPresent|" \
  | kubectl -n "$NS" apply -f -
kubectl -n "$NS" rollout status deployment/sandbox-router-deployment --timeout=180s

step "build ${KODE_GOPHER_IMG} (extends upstream with prewarmed GCP cache)"
docker build -t "$KODE_GOPHER_IMG" -f sandbox/Dockerfile .

step "load ${KODE_GOPHER_IMG} into kind"
kind load docker-image "$KODE_GOPHER_IMG" --name "$CLUSTER"

step "apply SandboxTemplate 'go-runtime-template' (manifests/base, references ${KODE_GOPHER_IMG})"
kubectl apply -k manifests/base
for _ in {1..30}; do
  if kubectl -n "$NS" get sandboxtemplate go-runtime-template >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
kubectl -n "$NS" get sandboxtemplate go-runtime-template >/dev/null \
  || die "SandboxTemplate 'go-runtime-template' did not appear within 30s"

# Evict any leaked sandbox pods so the next claim creates a fresh one
# from our latest image.
kubectl -n "$NS" delete pods -l sandbox --ignore-not-found --wait=false >/dev/null 2>&1 || true

step "build kode-gopher binary -> ./bin/kode-gopher"
mkdir -p bin
go build -o ./bin/kode-gopher ./cmd/kode-gopher

mkdir -p .smoke
GCLOUD_NAMES=""
if [[ $COMPARE -eq 1 ]]; then
  step "fetch gcloud reference (project=$GOOGLE_CLOUD_PROJECT)"
  # gcloud's --format=json uses gsutil-style field names (creation_time, no
  # millis, +0000 offset) while the GCS REST API our test programs use emits
  # timeCreated with millisecond precision + Z suffix. We compare the sorted
  # name list — "same buckets, same chronological order" — not timestamps.
  gcloud storage buckets list --format=json --project="$GOOGLE_CLOUD_PROJECT" \
    | jq '[.[] | {name, t: .creation_time}] | sort_by(.t) | [.[].name]' \
    > .smoke/gcloud.names
  GCLOUD_NAMES=".smoke/gcloud.names"
fi

# extract_data <file>
# Pulls the user program's data out of `kode-gopher exec`'s output:
#   - if the printOutcome printed a `── result ──` block, take that JSON's
#     .value (this is the wrapped/snippet path: result.json populated by
#     the wrapper from `func run`'s return value).
#   - otherwise take the `── stdout ──` block (verbatim path: user code
#     prints its data to stdout directly).
#
# Both paths use python's JSONDecoder.raw_decode so we read exactly the
# first JSON document and ignore whatever trailing log noise the
# agent-sandbox client may have interleaved on its way out (e.g.
# `"ts"="..." "msg"="claim deleted"` after the result block).
extract_data() {
  local file="$1"
  if grep -q '^── result ──$' "$file"; then
    awk '/^── result ──$/ { flag=1; next } flag' "$file" \
      | python3 -c '
import json, sys
obj, _ = json.JSONDecoder().raw_decode(sys.stdin.read().lstrip())
print(json.dumps(obj.get("value")))
'
  else
    awk '
      /^── stdout ──$/ { flag=1; next }
      /^── stderr ──$/ { flag=0 }
      flag { print }
    ' "$file" \
      | python3 -c '
import json, sys
obj, _ = json.JSONDecoder().raw_decode(sys.stdin.read().lstrip())
print(json.dumps(obj))
'
  fi
}

overall_ok=1
for TEST_FILE in "${TEST_FILES[@]}"; do
  base="$(basename "$TEST_FILE")"
  out=".smoke/${base}.out"

  step "run: kode-gopher exec $TEST_FILE"
  ./bin/kode-gopher exec "$TEST_FILE" 2>&1 | tee "$out"
  rc=${PIPESTATUS[0]}
  if [[ $rc -ne 0 ]]; then
    warn "kode-gopher exec $TEST_FILE exited $rc — see $out"
    overall_ok=0
    continue
  fi

  if [[ $COMPARE -eq 1 ]]; then
    step "compare against gcloud: $TEST_FILE"
    extract_data "$out" \
      | jq '[.[] | {name, t: .timeCreated}] | sort_by(.t) | [.[].name]' \
      > "${out%.out}.names"
    if diff -u "${out%.out}.names" "$GCLOUD_NAMES"; then
      printf '\n\033[1;32m✅ %s: match\033[0m\n' "$TEST_FILE"
    else
      warn "$TEST_FILE outputs differ from gcloud"
      overall_ok=0
    fi
  fi
done

if [[ $CLEAN -eq 1 ]]; then
  step "deleting kind cluster '$CLUSTER'"
  kind delete cluster --name "$CLUSTER"
fi

if [[ $overall_ok -eq 1 ]]; then
  printf '\n\033[1;32m✅ smoketest complete\033[0m\n'
else
  die "smoketest had failures — see warnings above"
fi

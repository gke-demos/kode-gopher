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
# scripts/smoketest-gke.sh — end-to-end on a pre-existing GKE Autopilot
# cluster that already has the agent-sandbox addon enabled.
#
# Assumes:
#   - kubectl context $CONTEXT exists and reaches the cluster
#   - the agent-sandbox CRDs and controller are installed (addon handles this)
#   - a SandboxTemplate-hosting namespace exists ($NS)
#   - some sandbox-router deployment exists somewhere in the cluster
#     with `app=sandbox-router` labels (we use whatever's there; we
#     don't deploy our own)
#
# Applies manifests/overlays/gke (which references the GHCR image), runs
# `kode-gopher exec --namespace=$NS` against each TEST_FILE, and (with
# --compare) diffs against `gcloud storage buckets list`.
#
# Flags:
#   --compare        also run `gcloud storage buckets list` and diff per file
#   --file <path>    run only the given Go program (overrides the default pair)
#
# Env overrides:
#   CONTEXT       kubectl context name      (ap-gke-sandbox)
#   NS            namespace                 (codemode)
#   GHCR_IMG      sandbox image at GHCR     (ghcr.io/gke-demos/kode-gopher-sandbox:latest)

set -euo pipefail

CONTEXT="${CONTEXT:-ap-gke-sandbox}"
NS="${NS:-codemode}"
GHCR_IMG="${GHCR_IMG:-ghcr.io/gke-demos/kode-gopher-sandbox:latest}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DEFAULT_FILES=("testdata/list_buckets.go" "testdata/list_buckets_snippet.go")
TEST_FILES=("${DEFAULT_FILES[@]}")
COMPARE=0
USER_FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --compare) COMPARE=1; shift;;
    --file)    USER_FILE="$2"; shift 2;;
    -h|--help) sed -n '17,40p' "$0"; exit 0;;
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
for bin in kubectl go gcloud jq python3 awk diff; do
  command -v "$bin" >/dev/null 2>&1 || die "missing prerequisite: $bin"
done
: "${GOOGLE_CLOUD_PROJECT:?GOOGLE_CLOUD_PROJECT must be set in env}"
ADC="$HOME/.config/gcloud/application_default_credentials.json"
[[ -f "$ADC" ]] || die "no ADC at $ADC — run: gcloud auth application-default login"
kubectl --context "$CONTEXT" get ns "$NS" >/dev/null \
  || die "namespace '$NS' not found in context '$CONTEXT'"
kubectl --context "$CONTEXT" get crd sandboxtemplates.extensions.agents.x-k8s.io >/dev/null \
  || die "agent-sandbox CRDs not installed in cluster '$CONTEXT'"
for f in "${TEST_FILES[@]}"; do
  [[ -f "$f" ]] || die "test file not found: $f"
done
echo "context=$CONTEXT  namespace=$NS  project=$GOOGLE_CLOUD_PROJECT  test_files=${TEST_FILES[*]}"

step "switch kubectl current-context to '$CONTEXT' (kode-gopher inherits it)"
kubectl config use-context "$CONTEXT" >/dev/null

step "apply manifests/overlays/gke (SandboxTemplate + SandboxWarmPool -> '$NS')"
kubectl --context "$CONTEXT" apply -k manifests/overlays/gke
for _ in {1..30}; do
  if kubectl --context "$CONTEXT" -n "$NS" get sandboxtemplate go-runtime-template >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
kubectl --context "$CONTEXT" -n "$NS" get sandboxtemplate go-runtime-template >/dev/null \
  || die "SandboxTemplate did not appear in $NS within 30s"

step "wait for warmpool to populate (up to 8 min on a cold node)"
# Autopilot may need to provision a fresh gVisor node + pull the
# image, so first-run can take ~2-3 minutes. Subsequent invocations
# are fast because the pool is already warm.
desired=$(kubectl --context "$CONTEXT" -n "$NS" get sandboxwarmpool kode-gopher-warmpool -o jsonpath='{.spec.replicas}' 2>/dev/null)
desired=${desired:-2}
for i in {1..48}; do
  ready=$(kubectl --context "$CONTEXT" -n "$NS" get sandboxwarmpool kode-gopher-warmpool -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
  printf "  t+%03ds  ready=%s/%s\n" $((i*10)) "${ready:-0}" "$desired"
  [[ "$ready" == "$desired" ]] && break
  sleep 10
done
[[ "$(kubectl --context "$CONTEXT" -n "$NS" get sandboxwarmpool kode-gopher-warmpool -o jsonpath='{.status.readyReplicas}')" == "$desired" ]] \
  || die "warmpool did not become ready within 8 minutes"

step "wait for sandbox-router in '$NS' (deployed by the overlay)"
kubectl --context "$CONTEXT" -n "$NS" rollout status deployment/sandbox-router-deployment --timeout=180s

step "build kode-gopher -> ./bin/kode-gopher"
mkdir -p bin
go build -o ./bin/kode-gopher ./cmd/kode-gopher

mkdir -p .smoke
GCLOUD_NAMES=""
if [[ $COMPARE -eq 1 ]]; then
  step "fetch gcloud reference (project=$GOOGLE_CLOUD_PROJECT)"
  gcloud storage buckets list --format=json --project="$GOOGLE_CLOUD_PROJECT" \
    | jq '[.[] | {name, t: .creation_time}] | sort_by(.t) | [.[].name]' \
    > .smoke/gcloud.names
  GCLOUD_NAMES=".smoke/gcloud.names"
fi

# extract_data: same logic as the kind smoketest. result block when
# wrapped, stdout block when verbatim; python's JSONDecoder.raw_decode
# stops at the first complete JSON document so trailing agent-sandbox
# log lines don't break us.
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
  out=".smoke/gke.${base}.out"

  step "run: kode-gopher exec --namespace=$NS $TEST_FILE"
  ./bin/kode-gopher exec --namespace="$NS" "$TEST_FILE" 2>&1 | tee "$out"
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

if [[ $overall_ok -eq 1 ]]; then
  printf '\n\033[1;32m✅ GKE smoketest complete\033[0m\n'
else
  die "GKE smoketest had failures — see warnings above"
fi

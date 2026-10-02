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
# scripts/refresh-warm-pool.sh — make the warm pool serve the sandbox
# template's current image.
#
# agent-sandbox doesn't roll a SandboxWarmPool's unclaimed sandboxes
# when its SandboxTemplate changes, so after a new sandbox image is
# applied, new sessions keep claiming old-image sandboxes until the
# pool turns over. This deletes the unclaimed warm-pool Sandboxes
# (claimed ones lose the warm-pool label, so running sessions are
# untouched) and waits until one sandbox on the template's image is
# ready.
#
# It deletes Sandboxes, not their pods: a Sandbox whose pod was deleted
# stays Ready with the dead pod's IP for a moment, a claim can adopt it
# then, and every request 502s. Foreground cascade returns only once
# the pods are gone.
#
# By default it refreshes only if some warm sandbox runs an image other
# than the template's. --always refreshes regardless: on kind the tag
# stays kode-gopher-sandbox:latest while its content changes.
#
# Flags:
#   --always     refresh even if every warm sandbox's image matches
#
# Env:
#   NS               namespace                 (codemode)
#   CONTEXT          kubectl context           (the current context)
#   TEMPLATE         SandboxTemplate name      (go-runtime-template)
#   READY_TIMEOUT    seconds to wait for one ready sandbox (600)

set -euo pipefail

ALWAYS=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --always)  ALWAYS=1; shift;;
    -h|--help) sed -n '17,46p' "$0"; exit 0;;
    *)         echo "unknown arg: $1" >&2; exit 2;;
  esac
done

NS="${NS:-codemode}"
TEMPLATE="${TEMPLATE:-go-runtime-template}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
WARM=agents.x-k8s.io/warm-pool-sandbox

k() {
  if [[ -n "${CONTEXT:-}" ]]; then
    kubectl --context "$CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

want="$(k -n "$NS" get sandboxtemplate "$TEMPLATE" -o jsonpath='{.spec.podTemplate.spec.containers[0].image}')"
[[ -n "$want" ]] || { echo "refresh-warm-pool: SandboxTemplate $TEMPLATE in $NS has no image" >&2; exit 1; }

# One line per warm sandbox pod: "<image> <ready>".
warm() {
  k -n "$NS" get pods -l "$WARM" \
    -o jsonpath='{range .items[*]}{.spec.containers[0].image} {.status.containerStatuses[0].ready}{"\n"}{end}'
}

stale="$(warm | awk -v want="$want" '$1 != want' | wc -l)"
if [[ $ALWAYS -eq 0 && $stale -eq 0 ]]; then
  echo "warm pool already on $want"
  exit 0
fi
if [[ $ALWAYS -eq 1 ]]; then
  echo "refreshing the warm pool (--always): $want"
else
  echo "refreshing the warm pool: $stale warm sandbox(es) not on $want"
fi
k -n "$NS" delete sandbox -l "$WARM" --cascade=foreground --ignore-not-found --wait=true

deadline=$((SECONDS + READY_TIMEOUT))
while (( SECONDS < deadline )); do
  if warm | awk -v want="$want" '$1 == want && $2 == "true" { found = 1 } END { exit !found }'; then
    echo "warm pool ready on $want"
    exit 0
  fi
  sleep 5
done
echo "refresh-warm-pool: no ready sandbox on $want after ${READY_TIMEOUT}s" >&2
k -n "$NS" get sandboxes,pods -l "$WARM" >&2 || true
exit 1

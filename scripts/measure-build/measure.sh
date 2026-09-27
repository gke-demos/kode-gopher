#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# measure.sh — runs inside a kode-gopher-sandbox container and times the
# compiled path's build phases, to find where snippet latency goes.
#
#   measure.sh <progs-dir> <out-dir> [rounds]
#
# For each program in <progs-dir>/*/main.go and each round, in a fresh
# work dir bootstrapped exactly like internal/executor's tidyCmd:
#   today   go mod tidy, then go build        (what the executor does)
#   notidy  go build only, on the prewarm lockfile
#   strip   go build -ldflags='-s -w', no tidy
# Prints one TSV line per phase to stdout and writes a go -debug-trace
# per build to <out-dir> for per-action (compile/link, cache miss) detail.
set -euo pipefail

progs=$1
out=$2
rounds=${3:-2}
mkdir -p "$out"

ms() { echo $(( $(date +%s%N) / 1000000 )); }

bootstrap() {
  rm -rf "$1" && mkdir -p "$1" && cp "$2" "$1/main.go"
  cp /opt/kode-gopher-base/go.mod "$1/go.mod"
  sed -i 's|^module .*|module kode_gopher_user|' "$1/go.mod"
  cp /opt/kode-gopher-base/go.sum "$1/go.sum"
  chmod u+w "$1/go.mod" "$1/go.sum"
}

# timed <label> <cmd...>: runs cmd, prints "<label>\t<ms>\t<exit>".
timed() {
  local label=$1; shift
  local t0 rc=0
  t0=$(ms)
  "$@" >"$out/$label.log" 2>&1 || rc=$?
  printf '%s\t%d\t%d\n' "$label" $(( $(ms) - t0 )) "$rc"
}

echo "# $(go version) nproc=$(nproc) GOMAXPROCS=${GOMAXPROCS:-unset}"
echo "# cgroup cpu.max=$(cat /sys/fs/cgroup/cpu.max 2>/dev/null || echo n/a)"
printf 'label\tms\texit\n'
for r in $(seq 1 "$rounds"); do
  for p in "$progs"/*/main.go; do
    name=$(basename "$(dirname "$p")")
    for v in today notidy strip; do
      w=/tmp/mb/$name-$v-$r
      bootstrap "$w" "$p"
      cd "$w"
      id=r$r.$name.$v
      if [ "$v" = today ]; then
        timed "$id.tidy" go mod tidy
      fi
      flags=()
      [ "$v" = strip ] && flags=(-ldflags='-s -w')
      timed "$id.build" go build -debug-trace="$out/$id.trace.json" "${flags[@]}" -o bin/run .
      cd /
    done
  done
done

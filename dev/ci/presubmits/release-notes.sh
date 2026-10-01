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

# release-notes.sh: presubmit: dev/release/notes.sh extracts the right
# section of a Keep a Changelog file, and CHANGELOG.md is in a shape it
# can read (an `## [Unreleased]` heading; every released version yields
# notes). The release workflow publishes whatever notes.sh prints, so a
# break here is an empty or wrong GitHub Release. Same idea as
# go-steer/core-agent's verify-release-notes.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml);
# run dev/ci/presubmits/all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

notes=dev/release/notes.sh
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# expect <name> <version> <changelog> <grep-pattern> [<absent-pattern>]
expect() {
  local out
  out="$("$notes" "$2" "$3")" || fail "$1: notes.sh exited non-zero"
  grep -q -- "$4" <<<"$out" || fail "$1: output lacks '$4':"$'\n'"$out"
  if [[ -n "${5:-}" ]] && grep -q -- "$5" <<<"$out"; then
    fail "$1: output has '$5':"$'\n'"$out"
  fi
}

printf '%s\n' '# Changelog' '' '## [Unreleased]' '' '- unreleased change' '' \
  '## [0.2.0-rc.1] - 2026-10-02' '' '- rc change' '' \
  '## [0.10.0] - 2026-10-01' '' '- ten change' '' \
  '## [0.1.0] - 2026-09-01' '' '- first change' >"$tmp/fixture.md"

expect "exact version" v0.1.0 "$tmp/fixture.md" '- first change' 'ten change'
expect "without v" 0.10.0 "$tmp/fixture.md" '- ten change' 'first change'
expect "pre-release" v0.2.0-rc.1 "$tmp/fixture.md" '- rc change' 'unreleased change'
expect "Unreleased fallback" v0.3.0 "$tmp/fixture.md" 'Unreleased section' 'rc change'
expect "dots are literal" v0x1x0 "$tmp/fixture.md" 'Unreleased section' 'first change'

printf '%s\n' '# Changelog' '' '## [Unreleased]' '' '## [0.1.0] - 2026-09-01' '' '- x' >"$tmp/empty.md"
if "$notes" v0.2.0 "$tmp/empty.md" >/dev/null 2>&1; then
  fail "empty Unreleased with no matching section: notes.sh should exit non-zero"
fi

grep -q '^## \[Unreleased\]' CHANGELOG.md || fail "CHANGELOG.md has no '## [Unreleased]' heading"

# Every released version in CHANGELOG.md must yield its own notes.
# (grep exits 1 when there are no releases yet.)
{ grep -oE '^## \[[0-9][^]]*\]' CHANGELOG.md || true; } | tr -d '#[] ' | while read -r ver; do
  out="$("$notes" "v${ver}")" || fail "CHANGELOG.md: no notes for ${ver}"
  if grep -q 'Unreleased section' <<<"$out"; then
    fail "CHANGELOG.md: notes for ${ver} fell back to Unreleased"
  fi
done

echo "release-notes: OK"

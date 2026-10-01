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

# notes.sh <version> [changelog]: print the release notes for <version>
# (e.g. v0.1.0 or 0.1.0) from CHANGELOG.md to stdout. Adapted from
# go-steer/mast's dev/release/notes.sh.
#
# Matches a Keep a Changelog heading for the version, `## [0.1.0] - ...`
# (brackets and a leading `v` optional), and prints everything up to the
# next `## ` heading. If there is none, it falls back to the
# `## [Unreleased]` section with a note saying so, so a tag cut before
# the CHANGELOG was rolled still gets real notes. Fails if both are
# empty: release.yml must never publish a release without notes.
#
# release.yml passes the output to `goreleaser release --release-notes`.
# dev/ci/presubmits/release-notes.sh tests this script.

set -euo pipefail

version="${1:?usage: notes.sh <version> [changelog]}"
changelog="${2:-$(dirname "$0")/../../CHANGELOG.md}"

# extract <heading-regex>: the body under the first matching ## heading.
# The regex goes through ENVIRON, not -v, so awk doesn't eat its
# backslashes.
extract() {
  RE="$1" awk '
    found && /^## / { exit }
    found { print; next }
    $0 ~ ENVIRON["RE"] { found = 1 }
  ' "$changelog"
}

# Escape the dots so 0.1.0 doesn't match 0x1y0.
v="${version#v}"
v="${v//./\\.}"
body="$(extract "^## \\[?v?${v}\\]?([^0-9A-Za-z.-]|$)")"

if [[ -z "${body//[[:space:]]/}" ]]; then
  body="$(extract '^## \[?Unreleased\]?')"
  if [[ -z "${body//[[:space:]]/}" ]]; then
    echo "notes.sh: ${changelog} has no section for ${version} and an empty Unreleased section" >&2
    exit 1
  fi
  printf '> Notes taken from the Unreleased section: CHANGELOG.md had no `## [%s]` heading at tag time.\n\n' "${version#v}"
fi

printf '%s\n' "$body"

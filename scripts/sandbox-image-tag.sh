#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

# sandbox-image-tag.sh — prints the content-derived tag for the sandbox
# image: a hash of every tracked file that goes into sandbox/Dockerfile's
# build. Same inputs, same tag, so the GKE overlay can pin the image in
# the same commit that changes it, and CI can check the pin is current.
# Uncommitted edits count; untracked files don't.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
sum=$(git ls-files -z sandbox cmd/sandbox-server internal/prewarm | sort -z | xargs -0 sha256sum | sha256sum)
echo "p-${sum:0:12}"

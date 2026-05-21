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

# syntax=docker/dockerfile:1.7
#
# kode-gopher-sandbox — extends the upstream go-runtime-sandbox image by
# prewarming $GOCACHE and $GOMODCACHE with the curated GCP SDK packages
# (see internal/curated/packages.go). Without this, cold `go build` of
# any non-trivial GCP program exceeds the upstream agent-sandbox HTTP
# layer's ~60s per-call cap (see docs/decisions.md).
#
# Single-arch (linux/amd64) for slice 0.5 + local kind. Multi-arch and
# Artifact Registry push are slice 3.
#
# Build from repo root:
#   docker build -t kode-gopher-sandbox:latest -f sandbox/Dockerfile .

FROM ghcr.io/gke-demos/go-runtime-sandbox:latest

# Switch to root briefly to drop the prewarm sources owned by the
# sandbox user; then drop back to USER 1000 so the actual `go build`
# writes its caches to /home/sandbox/{.cache/go-build,go/pkg/mod} —
# the same paths the runtime sandbox reads from at exec time.
USER root
COPY --chown=1000:1000 internal/prewarm /tmp/prewarm
USER 1000

# `go mod tidy` populates $GOMODCACHE; `go build ./...` populates
# $GOCACHE with compiled artifacts for every transitive package.
# `/dev/null` output: we don't keep the binary itself.
RUN cd /tmp/prewarm \
 && go mod tidy \
 && go build -o /dev/null ./...

# Clean the sources (caches at $GOCACHE/$GOMODCACHE persist).
USER root
RUN rm -rf /tmp/prewarm
USER 1000

# Inherit everything else (CMD, EXPOSE, WORKDIR, env) from upstream.

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
# kode-gopher-sandbox — the image every sandbox pod runs: a Go toolchain,
# our own in-pod server (cmd/sandbox-server, the agent-sandbox runtime
# protocol on :8888), and $GOCACHE / $GOMODCACHE prewarmed with the
# curated GCP SDK packages (see internal/curated/packages.go). Without
# the prewarm, cold `go build` of any non-trivial GCP program exceeds
# the agent-sandbox HTTP layer's per-call cap (see docs/decisions.md).
#
# Single-arch (linux/amd64). CI (.github/workflows/sandbox-image.yml)
# publishes it to ghcr.io/gke-demos/kode-gopher/sandbox under a tag
# derived from this file, cmd/sandbox-server/ and internal/prewarm/
# (scripts/sandbox-image-tag.sh), which the GKE overlay pins. Any change
# there changes the tag; run `make sandbox-pin` to update the overlay.
#
# Build from repo root:
#   docker build -t kode-gopher-sandbox:latest -f sandbox/Dockerfile .

# Base images are pinned by digest so the toolchain (and therefore the
# prewarmed cache) can't change without a change to this file, and so
# to the image tag. The Go version here is the one snippets build with.
FROM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS toolchain

# sandbox-server is stdlib-only; building it as its own throwaway
# module keeps it independent of the root go.mod (whose changes would
# otherwise have to feed the image tag).
FROM toolchain AS server
COPY cmd/sandbox-server /src
RUN cd /src \
 && go mod init kode-gopher/sandbox-server \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/sandbox-server .

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

LABEL org.opencontainers.image.source=https://github.com/gke-demos/kode-gopher

# git: `go mod tidy` may fetch modules outside the prewarm by VCS.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git tar \
 && rm -rf /var/lib/apt/lists/*
COPY --from=toolchain /usr/local/go /usr/local/go
COPY --from=server /out/sandbox-server /usr/local/bin/sandbox-server
RUN useradd -m -u 1000 -s /bin/bash sandbox \
 && mkdir -p /app /home/sandbox/.cache/go-build /home/sandbox/go/pkg/mod \
 && chown -R 1000:1000 /app /home/sandbox

# GOTOOLCHAIN=local: never download another toolchain, which would
# miss the whole prewarmed cache.
ENV PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
    GOCACHE=/home/sandbox/.cache/go-build \
    GOMODCACHE=/home/sandbox/go/pkg/mod \
    GOTOOLCHAIN=local \
    HOME=/home/sandbox \
    CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false

# Drop the prewarm sources owned by the sandbox user, then switch to
# USER 1000 so the actual `go build` writes its caches to
# /home/sandbox/{.cache/go-build,go/pkg/mod} — the same paths the
# runtime sandbox reads from at exec time.
COPY --chown=1000:1000 internal/prewarm /tmp/prewarm
USER 1000

# The committed go.mod + go.sum in internal/prewarm/ lock the exact
# versions we want in $GOMODCACHE. `go build ./...` respects them (no
# tidy — that would allow the proxy to drift versions upward from
# what we committed). This is the version-selection artifact that
# snippet builds inherit at runtime; see /opt/kode-gopher-base/
# preservation below.
#
# Then push every cache entry's mtime into the future. On each hit, Go
# bumps the mtime of any entry more than an hour old (so its trim knows
# what's in use). In a fresh container every baked-in entry is "old",
# so the first build would chtime hundreds of files, and each chtime
# copies the whole file up out of the image layer: ~10 s on the first
# GCS build locally, before a single byte compiles. A future mtime is
# never "old" and is never trimmed.
RUN cd /tmp/prewarm \
 && go build -o /dev/null ./... \
 && find "$GOCACHE" -type f -exec touch -m -d 2100-01-01 {} +

# Preserve prewarm's tidied go.mod + go.sum at a well-known path so
# the executor can bootstrap each snippet's /app/go.mod from them.
# This is what makes snippet builds cache-hit consistently: the
# snippet inherits the exact MVS-selected versions of every shared
# transitive dep (protobuf, grpc, otel, ...) that prewarm compiled
# against. Without this bootstrap, snippet-time `go mod tidy` on an
# empty go.mod resolves DIFFERENT versions from proxy, invalidating
# the compile cache; see docs/decisions.md.
USER root
RUN mkdir -p /opt/kode-gopher-base \
 && cp /tmp/prewarm/go.mod /opt/kode-gopher-base/go.mod \
 && cp /tmp/prewarm/go.sum /opt/kode-gopher-base/go.sum \
 && chmod 0444 /opt/kode-gopher-base/go.mod /opt/kode-gopher-base/go.sum \
 && rm -rf /tmp/prewarm
USER 1000

WORKDIR /app
EXPOSE 8888
CMD ["sandbox-server"]

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
# kode-gopher-server: the kode-gopher binary for in-cluster use
# (`kode-gopher serve --transport=http --in-cluster`,
# docs/design-in-cluster.md). It only talks to the Kubernetes API, the
# sandboxes and Google APIs; snippets build in the sandbox image, so
# this image needs no Go toolchain: a static binary on distroless.
#
# Multi-arch: the builder cross-compiles for each target platform.
# CI (.github/workflows/server-image.yml) publishes it to
# ghcr.io/gke-demos/kode-gopher/server.
#
# Build from repo root:
#   docker build -t kode-gopher-server -f server/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS build
ARG TARGETOS TARGETARCH
# Extra -ldflags, e.g. version stamping from the release workflow.
ARG LDFLAGS=""
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w $LDFLAGS" -o /out/kode-gopher ./cmd/kode-gopher

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kode-gopher /kode-gopher
USER 65532:65532
ENTRYPOINT ["/kode-gopher"]
CMD ["serve", "--transport=http", "--in-cluster"]

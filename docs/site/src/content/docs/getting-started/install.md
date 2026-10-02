---
title: Install the CLI
description: Install the kode-gopher binary.
sidebar:
  order: 1
---

```bash
go install github.com/gke-demos/kode-gopher/cmd/kode-gopher@latest
kode-gopher version
```

The binary lands in `$(go env GOPATH)/bin`, usually `~/go/bin`. Add that to your `PATH`, or use the full path in your MCP client's config.

Tagged releases will also publish binaries for linux and darwin (amd64 and arm64) on the [Releases](https://github.com/gke-demos/kode-gopher/releases) page, with a cosign-signed checksums file. No release has been tagged yet.

## Clone the repository too

The cluster setup applies Kubernetes manifests from the repository:

```bash
git clone https://github.com/gke-demos/kode-gopher
cd kode-gopher
```

Next: [set up a sandbox cluster](/getting-started/sandbox-cluster/).

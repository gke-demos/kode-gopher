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

Prebuilt binaries for Linux and macOS (`amd64` and `arm64`) are also published on the [Releases](https://github.com/gke-demos/kode-gopher/releases) page, alongside `checksums.txt` and a keyless Sigstore bundle (`checksums.txt.sigstore.json`). To verify a downloaded archive with `cosign`:

```bash
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/gke-demos/kode-gopher/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

## Clone the repository too

The cluster setup applies Kubernetes manifests from the repository:

```bash
git clone https://github.com/gke-demos/kode-gopher
cd kode-gopher
```

Next: [set up a sandbox cluster](/getting-started/sandbox-cluster/).

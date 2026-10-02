---
title: Set up a sandbox cluster
description: Install agent-sandbox and kode-gopher's sandbox template on kind or GKE Autopilot.
sidebar:
  order: 2
---

Every snippet runs in a pod taken from a warm pool of sandboxes. The manifests install three things:
- a SandboxTemplate, `go-runtime-template`, that runs the kode-gopher sandbox image: a Go toolchain with the Google Cloud libraries and `client-go` precompiled;
- a SandboxWarmPool, `go-runtime-pool`;
- the agent-sandbox router, which kode-gopher reaches the sandboxes through.

Pick kind for development, or GKE Autopilot for real isolation.

## Option A: kind

```bash
kind create cluster --name kode-gopher
kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller
```

The base manifests run the sandbox image as `kode-gopher-sandbox:latest`, which kind has to have locally. Pull the published image for your checkout and load it:

```bash
TAG=$(scripts/sandbox-image-tag.sh)
docker pull ghcr.io/gke-demos/kode-gopher/sandbox:$TAG
docker tag ghcr.io/gke-demos/kode-gopher/sandbox:$TAG kode-gopher-sandbox:latest
kind load docker-image kode-gopher-sandbox:latest --name kode-gopher
```

To build the image yourself instead, run `docker build -t kode-gopher-sandbox:latest -f sandbox/Dockerfile .`. Either way it's a large image because of the precompiled libraries, so the first pull or build, and the `kind load`, take a few minutes.

Then apply the manifests, into the `default` namespace:

```bash
kubectl apply -k manifests/base
kubectl rollout status deployment/sandbox-router
```

:::caution
kind has no gVisor, so sandboxes there are ordinary containers. Use kind for development, not for running code you don't trust.
:::

## Option B: GKE Autopilot

```bash
gcloud container clusters create-auto kode-gopher \
  --location=us-central1 --release-channel=rapid --enable-agent-sandbox
kubectl create namespace codemode
kubectl apply -k manifests/overlays/gke
kubectl -n codemode rollout status deployment/sandbox-router
```

On top of the base, the GKE overlay adds:
- gVisor isolation, and a locked-down security context;
- the published sandbox image, pinned to the tag your checkout builds;
- a warm pool of 2, on C3 nodes where available;
- a network policy that admits no in-cluster traffic to the router.

kode-gopher reaches the router with `kubectl port-forward`, which network policy doesn't apply to.

## Check it from the command line

`kode-gopher exec` runs one Go file in a sandbox and prints the outcome:

```bash
# kind (namespace "default")
kode-gopher exec testdata/list_buckets_snippet.go

# GKE
kode-gopher exec --context=<gke-context> --namespace=codemode testdata/list_buckets_snippet.go
```

The first run claims a sandbox from the warm pool. On GKE a typical snippet takes about 4 seconds end to end.

Next: [connect your MCP client](/getting-started/connect/).

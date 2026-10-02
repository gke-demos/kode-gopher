# Getting started: kode-gopher on your machine

This guide runs kode-gopher as a local MCP server over stdio. Your MCP client (Claude Code, Claude Desktop, Gemini CLI) starts `kode-gopher serve` as a subprocess. Each snippet the model writes is built and run in a sandbox pod on a Kubernetes cluster, with your own Google credentials.

To run kode-gopher as a shared server in a GKE cluster instead, with Google sign-in for each user, see [deploy.md](./deploy.md).

## What you need

- **Go** 1.26 or later, to install the CLI. Go downloads the exact toolchain it needs.
- **A Kubernetes cluster with [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox).** That can be a local kind cluster (Docker required) or GKE Autopilot with the agent-sandbox addon. Steps for both are below.
- **`kubectl`**, with a context for that cluster. You need permission to create SandboxClaims and port-forward in its namespace.
- **`gcloud`**, signed in to Application Default Credentials (ADC). Snippets run as you:
  ```bash
  gcloud auth application-default login
  export GOOGLE_CLOUD_PROJECT=<your-project>
  ```

## 1. Install the CLI

```bash
go install github.com/gke-demos/kode-gopher/cmd/kode-gopher@latest
kode-gopher version
```

Tagged releases will also publish signed binaries for linux and darwin (amd64, arm64) on the [Releases](https://github.com/gke-demos/kode-gopher/releases) page; see [release-process.md](./release-process.md). No release has been tagged yet.

The cluster setup below applies manifests from this repository, so clone it as well:

```bash
git clone https://github.com/gke-demos/kode-gopher && cd kode-gopher
```

## 2. Set up a sandbox cluster

Every snippet runs in a pod from a warm pool of sandboxes. The manifests install three things:
- a SandboxTemplate (`go-runtime-template`) running the kode-gopher sandbox image: a Go toolchain with the Google Cloud SDKs and `k8s.io/client-go` precompiled;
- a SandboxWarmPool (`go-runtime-pool`);
- the agent-sandbox router that kode-gopher reaches the sandboxes through.

### Option A: kind (local)

```bash
kind create cluster --name kode-gopher
kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox-with-extensions.yaml
kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller

# The base manifests run the image as kode-gopher-sandbox:latest. Pull
# the published image for these sources (or build it with
# `docker build -t kode-gopher-sandbox:latest -f sandbox/Dockerfile .`).
docker pull ghcr.io/gke-demos/kode-gopher/sandbox:$(scripts/sandbox-image-tag.sh)
docker tag ghcr.io/gke-demos/kode-gopher/sandbox:$(scripts/sandbox-image-tag.sh) kode-gopher-sandbox:latest
kind load docker-image kode-gopher-sandbox:latest --name kode-gopher

kubectl apply -k manifests/base        # namespace "default"
kubectl rollout status deployment/sandbox-router
```

The image is large because of the precompiled SDKs, so the pull and the `kind load` take a few minutes the first time. kind has no gVisor, so sandboxes there are ordinary containers. Use kind for development, not for running untrusted code.

`scripts/smoketest-kind.sh` does all of this and then runs test snippets. It changes your current kubectl context to the kind cluster.

### Option B: GKE Autopilot

```bash
gcloud container clusters create-auto kode-gopher \
  --location=us-central1 --release-channel=rapid --enable-agent-sandbox
kubectl create namespace codemode
kubectl apply -k manifests/overlays/gke   # namespace "codemode"
kubectl -n codemode rollout status deployment/sandbox-router
```

On top of the base, the GKE overlay adds:
- gVisor isolation;
- a locked-down security context;
- the published sandbox image, pinned to the tag these sources build;
- a warm pool of 2 on C3 nodes where available;
- a network policy that admits no in-cluster traffic to the router.

kode-gopher reaches the router with `kubectl port-forward`, so local use is unaffected.

## 3. Check it from the command line

`kode-gopher exec` runs a Go file in a sandbox and prints the result. Snippets declare `func run(ctx context.Context) (any, error)`, and whatever `run` returns comes back as JSON. Full `package main` programs work as well.

```bash
# kind (namespace "default")
kode-gopher exec testdata/list_buckets_snippet.go

# GKE
kode-gopher exec --context=<gke-context> --namespace=codemode testdata/list_buckets_snippet.go
```

The first run claims a sandbox from the warm pool. On GKE a typical snippet then takes about 4 seconds end to end.

## 4. Connect your MCP client

Claude Code:

```bash
claude mcp add kode-gopher -- kode-gopher serve --context=<context> --namespace=<namespace>
```

Claude Desktop (`claude_desktop_config.json`) and most other clients:

```json
{
  "mcpServers": {
    "kode-gopher": {
      "command": "kode-gopher",
      "args": ["serve", "--context=<context>", "--namespace=<namespace>"]
    }
  }
}
```

Use the full path to `kode-gopher` (typically `~/go/bin/kode-gopher`) if your client doesn't inherit your `PATH`. Leave out `--context` to use your current kubectl context. The namespace is `default` on kind and `codemode` with the GKE overlay.

The server registers three tools:

| Tool | What it does |
|---|---|
| `execute_go_code` | Builds and runs Go in the sandbox: one file (`code`) or several (`files`, with helper packages). Returns the phase (build or run), stdout, stderr, and a structured result: ok, error or panic. |
| `gcp_auth_status` | Reports the identity snippets run as: mode, credential type, email and project. |
| `lookup_package_docs` | `go doc` for the precompiled packages, so the model can check an API before writing code. |

The tool descriptions tell the model which packages are precompiled and how to write a snippet. Try asking: "use execute_go_code to list the GKE clusters in my project".

Each MCP session holds one sandbox for its lifetime, so later calls skip the claim. kode-gopher releases the sandbox when the client disconnects.

## Credentials

In this mode kode-gopher copies your ADC file (`~/.config/gcloud/application_default_credentials.json`) and `GOOGLE_CLOUD_PROJECT` into the sandbox for each run. Snippets can do anything your account can do, so treat the model's code as running as you. `gcp_auth_status` shows who that is.

With no ADC file, snippets run without credentials and `gcp_auth_status` reports `mode=none`.

## Troubleshooting

- **`exec` hangs at "opening sandbox".** The warm pool has no ready sandbox. Check `kubectl -n <namespace> get sandboxes,pods`. On GKE, a fresh pool waits for nodes to scale up and for the image to pull, which takes a few minutes. On kind, check that the image was loaded.
- **`ErrImagePull` on GKE right after updating.** The overlay pins a sandbox image tag that CI publishes when a change merges. For a few minutes after a merge that changed the image, the tag may not exist yet.
- **Slow builds with `tidied: true`.** The snippet imports a package outside the precompiled set, so the build ran `go mod tidy` and downloaded and compiled it. It works, but it's slower. The sandbox's module and build caches survive between calls in the same session, so later calls are faster.
- **Permission errors from Google APIs.** These come from your own account. Compare `gcp_auth_status` with `gcloud auth list`, and check that `GOOGLE_CLOUD_PROJECT` is the project you mean.

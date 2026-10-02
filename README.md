# kode-gopher

A Go-native take on Cloudflare's [Code Mode](https://blog.cloudflare.com/code-mode/), for Google Cloud. kode-gopher is an MCP server (and a CLI). The model writes an ordinary Go program against the real `cloud.google.com/go/...` SDKs, and kode-gopher compiles it and runs it in a sandboxed Kubernetes pod. It returns a structured result.

Why Go: the model's tool surface already exists as importable packages, and the model has seen plenty of real code that uses them. Those packages include `cloud.google.com/go/storage`, Compute, BigQuery, GKE and `k8s.io/client-go`. So instead of generating typed stubs from tool schemas, as Cloudflare's TypeScript version does, the model writes a normal program. One agent step can make many API calls and return one result.

```
MCP client ──► kode-gopher serve ──► sandbox pod (agent-sandbox, gVisor on GKE)
 (Claude Code,    execute_go_code       go build + run, with the Google Cloud SDKs
  Claude Desktop, gcp_auth_status       and client-go precompiled; credentials are
  Gemini CLI, …)  lookup_package_docs   your own, or the signed-in user's
```

A typical snippet:

```go
package snippet

import (
	"context"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

func run(ctx context.Context) (any, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var names []string
	it := c.Buckets(ctx, "my-project")
	for {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, b.Name)
	}
	return names, nil
}
```

Whatever `run` returns comes back to the model as JSON. Errors and panics come back as structured results too, and compile errors come back as `phase=build`. On GKE with gVisor, a snippet like this takes about 4 seconds end to end.

## Two ways to run it

| | On your machine | Shared, in a GKE cluster |
|---|---|---|
| Transport | stdio: your MCP client starts `kode-gopher serve` | streamable HTTP at `https://<host>/mcp` |
| Sandboxes | in a kind or GKE cluster your kubectl reaches | in the same cluster as kode-gopher |
| Snippets run as | you (your ADC) | each signed-in Google user, a service account, or kode-gopher's own Workload Identity |
| Guide | [Getting started](https://gke-demos.github.io/kode-gopher/getting-started/) | [Deploy for a team](https://gke-demos.github.io/kode-gopher/deploy/) |

## Status

Pre-alpha, and no release has been tagged yet. Built and verified so far:
- **Local mode:** the CLI and the stdio MCP server, on kind and on GKE Autopilot with gVisor.
- **Fast builds:** a CI-published sandbox image with the SDKs precompiled.
- **In-cluster server:** a GKE Gateway with a managed certificate, a static token or OAuth 2.1 sign-in fronting Google (with Google's Agent Identity credential vault), service-account mode, and the `client_credentials` grant for automation. Real Google sign-in from Claude Code has been tested on GKE.
- **CI:** presubmits, a kind end-to-end test, and signed multi-arch images.

What's next, and the evidence behind each step, are in [docs/plan.md](./docs/plan.md) and [docs/decisions.md](./docs/decisions.md).

## Docs

**[gke-demos.github.io/kode-gopher](https://gke-demos.github.io/kode-gopher/)** has the user documentation: getting started, deploying for a team, concepts, and reference pages for the MCP tools, the CLI and the precompiled packages. Its sources are in [`docs/site`](./docs/site).

Design and project records, in this repository:

| | |
|---|---|
| [docs/design.md](./docs/design.md) | architecture, execution modes, result protocol, sandbox boundary |
| [docs/design-in-cluster.md](./docs/design-in-cluster.md) | the in-cluster server's design: sessions, OAuth, credentials in the sandbox, network policy |
| [docs/plan.md](./docs/plan.md) | slice-by-slice build plan and status |
| [docs/decisions.md](./docs/decisions.md) | the log of judgment calls and measurements |
| [docs/release-process.md](./docs/release-process.md) | versions, tags, release notes |
| [CONTRIBUTING.md](./CONTRIBUTING.md) | presubmits, CI, commit style |

## Repository layout

| path | what |
| --- | --- |
| [`cmd/kode-gopher`](./cmd/kode-gopher) | the CLI binary — subcommands `exec` and `serve` |
| [`cmd/mcp-smoketest`](./cmd/mcp-smoketest) | programmatic MCP client; spawns `kode-gopher serve` (stdio or streamable HTTP) and exercises `execute_go_code` end-to-end; `--offline` runs the checks that need no Google credentials |
| [`cmd/sandbox-server`](./cmd/sandbox-server) | the in-pod HTTP server (agent-sandbox runtime protocol on :8888), built into the sandbox image |
| [`internal/mcp`](./internal/mcp) | MCP server + tool handlers (execute_go_code, gcp_auth_status, lookup_package_docs) |
| [`internal/executor`](./internal/executor) | Build/Run/Fetch phases over a `sandbox.Session`; bootstraps `/app/go.mod` from the prewarm lockfile |
| [`internal/sandbox`](./internal/sandbox) | our thin client over `sigs.k8s.io/agent-sandbox`; `KubeContext` + `PerAttemptTimeout` options; typed `ErrSessionDead` for retry-once at MCP layer |
| [`internal/creds`](./internal/creds) | unified `Source` interface: Materialize (files+env for the executor) + Identity (mode/type/email/project for gcp_auth_status), Forwarded, Minted, Service and OAuthUser impls |
| [`internal/normalize`](./internal/normalize) | multi-file input; root vs subdirectory partitioning; same-package rewrite; optional `extra_imports` companion file |
| [`internal/wrapper`](./internal/wrapper) | the generated `func main()` shipped alongside snippets |
| [`internal/curated`](./internal/curated) | canonical list of GCP + k8s.io/client-go packages prewarmed in the sandbox image; `go:generate` drives prompts regen |
| [`internal/prewarm`](./internal/prewarm) | standalone Go module imported at image build to populate `$GOCACHE`; committed `go.mod`+`go.sum` pin versions |
| [`internal/prompts`](./internal/prompts) | generated `system.md` (LLM system prompt) + `description.go` (execute_go_code tool description); regenerate via `make prompts` |
| [`sandbox/Dockerfile`](./sandbox/Dockerfile) | the sandbox image: Go toolchain, `sandbox-server`, prewarmed cache |
| [`server/Dockerfile`](./server/Dockerfile) | the in-cluster server image: static `kode-gopher` on distroless, amd64 and arm64 |
| [`manifests/base`](./manifests/base) | kustomize base (kind-compatible): v1beta1 SandboxTemplate with network policy, SandboxWarmPool `go-runtime-pool`, per-namespace sandbox-router |
| [`manifests/overlays/gke`](./manifests/overlays/gke) | GKE Autopilot overlay: gVisor + securityContext + pinned image + pool of 2 + C3-first ComputeClass + router lock-down |
| [`manifests/overlays/gke-server`](./manifests/overlays/gke-server) | in-cluster kode-gopher (`docs/design-in-cluster.md`): Deployment, Role, Gateway with managed cert, network policy; `gke-server-oauth` adds Google sign-in. Deploy with [`scripts/deploy-gke-server.sh`](./scripts/deploy-gke-server.sh) |
| [`scripts/smoketest-kind.sh`](./scripts/smoketest-kind.sh) | local-kind direct-CLI verification |
| [`scripts/smoketest-gke.sh`](./scripts/smoketest-gke.sh) | GKE Autopilot direct-CLI verification |
| [`scripts/smoketest-mcp.sh`](./scripts/smoketest-mcp.sh) | MCP-layer verification against either substrate |
| [`scripts/smoketest-http.sh`](./scripts/smoketest-http.sh) | in-cluster kode-gopher over streamable HTTP, including negative auth cases |
| [`scripts/refresh-warm-pool.sh`](./scripts/refresh-warm-pool.sh) | replaces the warm pool's unclaimed sandboxes when they run an older image than the template (used by the deploy and kind scripts) |
| [`dev/ci/e2e/kind.sh`](./dev/ci/e2e/kind.sh) | CI's kind end-to-end test, no Google credentials: throwaway cluster, agent-sandbox, sandbox image, `mcp-smoketest --offline` over stdio and HTTP |
| [`experiments/yaegi-poc`](./experiments/yaegi-poc) | Slice 6 proof of concept (shelved) — Yaegi interpreter as an alternative runtime, plus the `kg-difftest` corpus that ruled it out |

## Built on

- [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) — SandboxClaim / SandboxTemplate / SandboxWarmPool CRDs, in-pod runtime, controller, and Go client (`sigs.k8s.io/agent-sandbox/clients/go/sandbox`).
- [gke-demos/go-runtime-sandbox](https://github.com/gke-demos/go-runtime-sandbox): where kode-gopher started. We used its image as our base until slice 7; `cmd/sandbox-server` now implements the same agent-sandbox runtime protocol.
- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) — MCP server + client SDK.
- [traefik/yaegi](https://github.com/traefik/yaegi) — only inside `experiments/yaegi-poc/` (standalone module, not pulled into the main build) as the interpreter behind the shelved Slice 6 PoC.

## License

Apache 2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

# kode-gopher

A Go-native take on Cloudflare's [Code Mode](https://blog.cloudflare.com/code-mode/), specialized for Google Cloud. An MCP server (and CLI) that ships a Go program into a sandboxed GKE pod, compiles and runs it against the real `cloud.google.com/go/...` SDKs with forwarded credentials, and returns a discriminated structured result.

The wedge: in Go, the LLM's "tool surface" already exists as importable packages — `cloud.google.com/go/storage`, `cloud.google.com/go/compute/apiv1`, BigQuery, GKE, Secret Manager — and the model has seen plenty of real code that uses them. So instead of generating typed stubs from MCP tool schemas (Cloudflare's TS approach), we let the model write a normal Go program and execute it. One agent step → many GCP API calls → one structured result back.

## Status

Pre-alpha. Shipped through Slice 2.5 of [`docs/plan.md`](./docs/plan.md):

- **CLI** (`kode-gopher exec <file.go>`) accepts either a full `package main` program (verbatim mode) or a snippet declaring `func run(ctx context.Context) (any, error)` (wrapped mode). Wrapper captures error / panic / json-marshal failure into a discriminated `result.json`. Build vs runtime errors are unambiguously attributable via `Outcome.Phase`.
- **MCP server** (`kode-gopher serve`) over stdio using `github.com/modelcontextprotocol/go-sdk`. One tool: `execute_go_code(code, extra_imports?, [runtime?])`. One long-lived `sandbox.Session` per server process, mutex-serialized tool calls, `/app` reset between calls (caches survive).
- **Sandbox backend**: own thin wrapper at `internal/sandbox/` over `sigs.k8s.io/agent-sandbox/clients/go/sandbox` directly (Slice 2.5 ownership move). Prewarmed image (`ghcr.io/gke-demos/kode-gopher-sandbox:latest`) bakes the curated GCP SDK packages into `$GOCACHE`/`$GOMODCACHE`.
- **Substrates**: verified end-to-end on local `kind` and on a real GKE Autopilot cluster with the agent-sandbox addon + gVisor isolation. Both the direct-CLI and the MCP paths diff cleanly against `gcloud storage buckets list`.

Planned slices in [`docs/plan.md`](./docs/plan.md):

- **Slice 3** — full GKE deployment story (Artifact Registry push, Workload Identity binding documentation, etc.).
- **Slice 4** — production hardening: `lookup_package_docs` + `gcp_auth_status` tools, `internal/creds` interface, NetworkPolicy + IP ranges CronJob, multi-file snippets, `--context` flag.
- **Slice 5** — HTTP/SSE transport. Scope gated on five explicit design questions (session topology, auth, per-end-user creds, streaming, deployment topology).
- **Slice 6** — alternative Yaegi (interpreter) runtime as opt-in second backend. PoC in `experiments/yaegi-poc/` shows ~700ms end-to-end for a real GCS list vs ~5-30s through the compiled path; full slice gated on testing `cloud.google.com/go/compute/apiv1` (true gRPC) under Yaegi.

## Try it locally

Prereqs: `kind`, `kubectl`, `docker`, `gcloud`, `go`, `python3`, plus a GCP project the executing user can list buckets in.

```bash
gcloud auth application-default login
export GOOGLE_CLOUD_PROJECT=your-project

# Bootstrap local kind cluster + agent-sandbox + sandbox-router +
# kode-gopher-sandbox image + SandboxTemplate, then exercise both
# verbatim and wrapped test snippets via the direct CLI, diffing each
# against `gcloud storage buckets list`.
./scripts/smoketest-kind.sh --compare

# Same exercise via the MCP layer: spawn `kode-gopher serve` and
# speak MCP over its stdio (no LLM required — the smoketest binary is
# itself an MCP client).
./scripts/smoketest-mcp.sh --target=kind --compare
```

For GKE (assumes a cluster with the agent-sandbox addon enabled + an `ap-gke-sandbox` context):

```bash
./scripts/smoketest-gke.sh --compare
./scripts/smoketest-mcp.sh --target=gke --compare
```

Once `kode-gopher serve` works locally, point any MCP client (Claude Desktop, Gemini CLI, custom) at it. Sample config snippet for Claude Desktop:

```json
{
  "mcpServers": {
    "kode-gopher": {
      "command": "/path/to/bin/kode-gopher",
      "args": ["serve", "--namespace=codemode"]
    }
  }
}
```

All smoketests are idempotent and reuse infra across runs.

## What's here

| path | what |
| --- | --- |
| [`docs/design.md`](./docs/design.md) | architecture, transport + runtime modes, auth, result protocol, sandbox boundary |
| [`docs/plan.md`](./docs/plan.md) | slice-by-slice build sequence (0-6, with 3-6 still ahead) |
| [`docs/decisions.md`](./docs/decisions.md) | append-only log of judgment calls per slice |
| [`cmd/kode-gopher`](./cmd/kode-gopher) | the CLI binary — subcommands `exec` and `serve` |
| [`cmd/mcp-smoketest`](./cmd/mcp-smoketest) | programmatic MCP client; spawns `kode-gopher serve` and exercises `execute_go_code` end-to-end |
| [`internal/mcp`](./internal/mcp) | MCP server + tool registration over `github.com/modelcontextprotocol/go-sdk` |
| [`internal/executor`](./internal/executor) | Build/Run/Fetch phases over a `sandbox.Session` |
| [`internal/sandbox`](./internal/sandbox) | our thin client over `sigs.k8s.io/agent-sandbox` (replaces the previous `pkg/goruntime` dependency, Slice 2.5) |
| [`internal/normalize`](./internal/normalize) | snippet vs full-main detection; AST-based package rewrite; optional `extra_imports` companion file |
| [`internal/wrapper`](./internal/wrapper) | the generated `func main()` shipped alongside snippets |
| [`internal/curated`](./internal/curated) | canonical list of GCP packages prewarmed in the sandbox image |
| [`internal/prewarm`](./internal/prewarm) | standalone Go module imported at image build to populate `$GOCACHE` |
| [`sandbox/Dockerfile`](./sandbox/Dockerfile) | extends `ghcr.io/gke-demos/go-runtime-sandbox:latest` with the prewarmed cache |
| [`manifests/base`](./manifests/base) | SandboxTemplate kustomize base (kind-compatible) |
| [`manifests/overlays/gke`](./manifests/overlays/gke) | GKE Autopilot overlay: gVisor + securityContext + Workload Identity + SandboxWarmPool + per-namespace sandbox-router |
| [`scripts/smoketest-kind.sh`](./scripts/smoketest-kind.sh) | local-kind direct-CLI verification |
| [`scripts/smoketest-gke.sh`](./scripts/smoketest-gke.sh) | GKE Autopilot direct-CLI verification |
| [`scripts/smoketest-mcp.sh`](./scripts/smoketest-mcp.sh) | MCP-layer verification against either substrate |
| [`experiments/yaegi-poc`](./experiments/yaegi-poc) | Slice 6 proof of concept — Yaegi interpreter as an alternative runtime |

## Built on

- [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) — SandboxClaim / SandboxTemplate / SandboxWarmPool CRDs, in-pod runtime, controller, and Go client (`sigs.k8s.io/agent-sandbox/clients/go/sandbox`).
- [gke-demos/go-runtime-sandbox](https://github.com/gke-demos/go-runtime-sandbox) — the published sandbox image (`ghcr.io/gke-demos/go-runtime-sandbox:latest`) we extend as our base in `sandbox/Dockerfile`.
- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) — MCP server + client SDK.
- [traefik/yaegi](https://github.com/traefik/yaegi) — only inside `experiments/yaegi-poc/` (standalone module, not pulled into the main build) as the interpreter behind the Slice 6 PoC.

## License

Apache 2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

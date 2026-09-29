# kode-gopher

A Go-native take on Cloudflare's [Code Mode](https://blog.cloudflare.com/code-mode/), specialized for Google Cloud. An MCP server (and CLI) that ships a Go program into a sandboxed GKE pod, compiles and runs it against the real `cloud.google.com/go/...` SDKs with forwarded credentials, and returns a discriminated structured result.

The wedge: in Go, the LLM's "tool surface" already exists as importable packages — `cloud.google.com/go/storage`, `cloud.google.com/go/compute/apiv1`, BigQuery, GKE, Secret Manager — and the model has seen plenty of real code that uses them. So instead of generating typed stubs from MCP tool schemas (Cloudflare's TS approach), we let the model write a normal Go program and execute it. One agent step → many GCP API calls → one structured result back.

## Status

Pre-alpha. Shipped through Slice 4 of [`docs/plan.md`](./docs/plan.md):

- **CLI** (`kode-gopher {exec,serve,auth status}`). `exec <file.go>` and `serve` accept a snippet declaring `func run(ctx context.Context) (any, error)` (wrapped mode) or a full `package main` program (verbatim). Wrapper captures error / panic / json-marshal failure into a discriminated `result.json`. `auth status` reports the ambient identity kode-gopher will forward. `--context` flag on both `exec` and `serve` pins a kubeconfig context instead of inheriting ambient `kubectl config current-context`.
- **MCP server** (`kode-gopher serve`) over stdio using `github.com/modelcontextprotocol/go-sdk`. Three tools:
  - `execute_go_code(code | files, extra_imports?)` — build+run Go in the sandbox. `code` is single-file; `files` is `map[path -> source]` for multi-file snippets with helper subpackages.
  - `gcp_auth_status()` — report the sandbox's credential identity (mode, credential type, email, project).
  - `lookup_package_docs(package, symbol?)` — `go doc` for curated packages against the prewarmed cache, subsecond.
  One long-lived `sandbox.Session` per server process; mutex-serialized tool calls; `/app` reset between calls (caches survive); retry-once on `ErrSessionDead` so a pod eviction mid-call self-heals.
- **Sandbox backend**: own thin wrapper at `internal/sandbox/` over `sigs.k8s.io/agent-sandbox/clients/go/sandbox`. Prewarmed image (`ghcr.io/gke-demos/kode-gopher/sandbox`, published by CI under a content-derived tag the GKE overlay pins; the in-pod server is our own `cmd/sandbox-server`) bakes GCP SDK + `k8s.io/client-go` into `$GOCACHE`/`$GOMODCACHE`; the tidied prewarm `go.mod` is preserved at `/opt/kode-gopher-base/` and the executor bootstraps each snippet's `/app/go.mod` from it so version selection is reproducible and builds cache-hit. `PerAttemptTimeout` is 3 minutes (was implicit 60 s from the upstream default).
- **Credentials** unified as `creds.Source` — Materialize forwards ADC + env into the sandbox; Identity parses ADC and calls the OAuth2 userinfo endpoint for `authorized_user` creds (cached). `Workload` implementation is stubbed until we run in-cluster.
- **Substrates**: verified end-to-end on local `kind` and on a real GKE Autopilot cluster with the agent-sandbox addon + gVisor isolation. Both the direct-CLI and the MCP paths diff cleanly against `gcloud storage buckets list`; the MCP smoketest also exercises the k8s and multi-file paths.
- **Generated LLM prompt**: `make prompts` regenerates `internal/prompts/{system.md,description.go}` from `internal/curated.Packages`, keeping the LLM system prompt and the `execute_go_code` tool description in lockstep with the curated set.

Planned slices in [`docs/plan.md`](./docs/plan.md):

- **Slice 3** — full GKE deployment story (formalize Artifact Registry push, Workload Identity binding docs; largely done opportunistically).
- **Slice 5** — HTTP/SSE transport. Scope gated on five explicit design questions (session topology, auth, per-end-user creds, streaming, deployment topology).
- **Slice 6** — Yaegi (interpreter) runtime. **Shelved**: packaging worked, but the interpreter silently produces wrong output for common Go idioms (see `docs/decisions.md`). The PoC stays in `experiments/yaegi-poc/`.
- **Slice 7** — fast compiled path. Measured: a warm build under gVisor is ~7-8 s, and the ~55 s seen on GKE came from a stale published sandbox image. Scope: CI-published, pinned sandbox image; skip `go mod tidy` for curated-only snippets; stripped links.

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
| [`docs/plan.md`](./docs/plan.md) | slice-by-slice build sequence (0-6; 0-4 shipped, 5-6 planned) |
| [`docs/decisions.md`](./docs/decisions.md) | append-only log of judgment calls per slice |
| [`cmd/kode-gopher`](./cmd/kode-gopher) | the CLI binary — subcommands `exec` and `serve` |
| [`cmd/mcp-smoketest`](./cmd/mcp-smoketest) | programmatic MCP client; spawns `kode-gopher serve` and exercises `execute_go_code` end-to-end |
| [`cmd/sandbox-server`](./cmd/sandbox-server) | the in-pod HTTP server (agent-sandbox runtime protocol on :8888), built into the sandbox image |
| [`internal/mcp`](./internal/mcp) | MCP server + tool handlers (execute_go_code, gcp_auth_status, lookup_package_docs) |
| [`internal/executor`](./internal/executor) | Build/Run/Fetch phases over a `sandbox.Session`; bootstraps `/app/go.mod` from the prewarm lockfile |
| [`internal/sandbox`](./internal/sandbox) | our thin client over `sigs.k8s.io/agent-sandbox`; `KubeContext` + `PerAttemptTimeout` options; typed `ErrSessionDead` for retry-once at MCP layer |
| [`internal/creds`](./internal/creds) | unified `Source` interface: Materialize (files+env for the executor) + Identity (mode/type/email/project for gcp_auth_status), Forwarded + Workload impls |
| [`internal/normalize`](./internal/normalize) | multi-file input; root vs subdirectory partitioning; same-package rewrite; optional `extra_imports` companion file |
| [`internal/wrapper`](./internal/wrapper) | the generated `func main()` shipped alongside snippets |
| [`internal/curated`](./internal/curated) | canonical list of GCP + k8s.io/client-go packages prewarmed in the sandbox image; `go:generate` drives prompts regen |
| [`internal/prewarm`](./internal/prewarm) | standalone Go module imported at image build to populate `$GOCACHE`; committed `go.mod`+`go.sum` pin versions |
| [`internal/prompts`](./internal/prompts) | generated `system.md` (LLM system prompt) + `description.go` (execute_go_code tool description); regenerate via `make prompts` |
| [`sandbox/Dockerfile`](./sandbox/Dockerfile) | the sandbox image: Go toolchain, `sandbox-server`, prewarmed cache |
| [`manifests/base`](./manifests/base) | SandboxTemplate kustomize base (kind-compatible) |
| [`manifests/overlays/gke`](./manifests/overlays/gke) | GKE Autopilot overlay: gVisor + securityContext + Workload Identity + SandboxWarmPool + per-namespace sandbox-router |
| [`scripts/smoketest-kind.sh`](./scripts/smoketest-kind.sh) | local-kind direct-CLI verification |
| [`scripts/smoketest-gke.sh`](./scripts/smoketest-gke.sh) | GKE Autopilot direct-CLI verification |
| [`scripts/smoketest-mcp.sh`](./scripts/smoketest-mcp.sh) | MCP-layer verification against either substrate |
| [`experiments/yaegi-poc`](./experiments/yaegi-poc) | Slice 6 proof of concept (shelved) — Yaegi interpreter as an alternative runtime, plus the `kg-difftest` corpus that ruled it out |

## Built on

- [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) — SandboxClaim / SandboxTemplate / SandboxWarmPool CRDs, in-pod runtime, controller, and Go client (`sigs.k8s.io/agent-sandbox/clients/go/sandbox`).
- [gke-demos/go-runtime-sandbox](https://github.com/gke-demos/go-runtime-sandbox): where kode-gopher started. We used its image as our base until slice 7; `cmd/sandbox-server` now implements the same agent-sandbox runtime protocol.
- [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) — MCP server + client SDK.
- [traefik/yaegi](https://github.com/traefik/yaegi) — only inside `experiments/yaegi-poc/` (standalone module, not pulled into the main build) as the interpreter behind the shelved Slice 6 PoC.

## License

Apache 2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

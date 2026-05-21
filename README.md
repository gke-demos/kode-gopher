# kode-gopher

A Go-native take on Cloudflare's [Code Mode](https://blog.cloudflare.com/code-mode/), specialized for Google Cloud. An MCP server (and CLI) that ships a Go program into a sandboxed GKE pod, compiles and runs it against the real `cloud.google.com/go/...` SDKs with forwarded credentials, and returns a discriminated structured result.

The wedge: in Go, the LLM's "tool surface" already exists as importable packages — `cloud.google.com/go/storage`, `cloud.google.com/go/compute/apiv1`, BigQuery, GKE, Secret Manager — and the model has seen plenty of real code that uses them. So instead of generating typed stubs from MCP tool schemas (Cloudflare's TS approach), we let the model write a normal Go program and execute it. One agent step → many GCP API calls → one structured result back.

## Status

Pre-alpha. Slice 1 of [`docs/plan.md`](./docs/plan.md) is complete:

- `kode-gopher exec <file.go>` CLI accepts either a full `package main` program (verbatim mode) or a snippet declaring `func run(ctx context.Context) (any, error)` (wrapped mode).
- The wrapper captures error / panic / json-marshal failure into a discriminated `result.json`.
- Executor surfaces `Outcome{Phase, ExitCode, Stdout, Stderr, Result}`; build vs runtime errors are unambiguously attributable.
- Local-kind end-to-end smoketest passes against both modes; both match `gcloud storage buckets list` chronological order.

Slices remaining: MCP server (2), GKE Autopilot deployment (3), production hardening (4 — `lookup_package_docs`, `gcp_auth_status`, NetworkPolicy, `SandboxWarmPool`).

## Try it locally

You'll need `kind`, `kubectl`, `docker`, `gcloud`, `python3`, plus a GCP project the executing user can list buckets in.

```bash
gcloud auth application-default login
export GOOGLE_CLOUD_PROJECT=your-project
./scripts/smoketest-kind.sh --compare
```

The script provisions a kind cluster, installs the agent-sandbox controller + sandbox-router, builds and loads the prewarmed `kode-gopher-sandbox` image, applies the SandboxTemplate, then runs `kode-gopher exec` against both `testdata/list_buckets.go` (verbatim) and `testdata/list_buckets_snippet.go` (wrapped) and diffs each against `gcloud storage buckets list`.

Re-runs are idempotent. `--clean` deletes the kind cluster on exit. `--file <path>` runs a single program instead of the default pair.

## What's here

| path | what |
| --- | --- |
| [`docs/design.md`](./docs/design.md) | architecture, auth model, result protocol, sandbox boundary |
| [`docs/plan.md`](./docs/plan.md) | slice-by-slice build sequence |
| [`docs/decisions.md`](./docs/decisions.md) | append-only log of judgment calls |
| [`cmd/kode-gopher`](./cmd/kode-gopher) | the CLI binary |
| [`internal/normalize`](./internal/normalize) | snippet vs full-main detection; AST-based package rewrite |
| [`internal/wrapper`](./internal/wrapper) | the generated `func main()` shipped alongside snippets |
| [`internal/executor`](./internal/executor) | Build/Run/Fetch phases over a `pkg/goruntime.Session` |
| [`internal/curated`](./internal/curated) | canonical list of GCP packages prewarmed in the sandbox image |
| [`internal/prewarm`](./internal/prewarm) | standalone Go module imported at image build to populate `$GOCACHE` |
| [`sandbox/Dockerfile`](./sandbox/Dockerfile) | extends `ghcr.io/gke-demos/go-runtime-sandbox:latest` with the prewarmed cache |
| [`manifests/base`](./manifests/base) | SandboxTemplate kustomize base |
| [`scripts/smoketest-kind.sh`](./scripts/smoketest-kind.sh) | end-to-end local-kind verification |

## Built on

- [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) — sandbox CRDs and controller
- [gke-demos/go-runtime-sandbox](https://github.com/gke-demos/go-runtime-sandbox) — in-pod Go-toolchain HTTP server, `pkg/goruntime` host client, base sandbox image

## License

Apache 2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

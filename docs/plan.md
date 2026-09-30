# kode-gopher — build plan

This document is the build sequence. See [`design.md`](./design.md) for the architecture, the *why*, and the file layout.

The work is sliced so each slice is independently testable end-to-end. Don't move to the next slice until the current one's pass criterion holds.

## Slice 0 — prove the loop locally

**Goal**: validate our understanding of `pkg/goruntime` against real infra before paying GKE setup cost. Cheap, fast, deletes most of our uncertainty.

**Scope**:
- `cmd/kode-gopher/exec.go` (~80 LOC): reads a Go file, opens a `*goruntime.Session` against a local `kind` cluster (agent-sandbox addon installed), copies local ADC into the Files map if present, runs `go run .`, prints `pkg/format.Result`.
- Hardcoded Template name, Namespace=`default`.
- No MCP, no wrapper, no normalize, no result file. User writes a full `package main`.

**Prerequisites the developer must have**:
- `kind` installed locally with a running cluster.
- `kubernetes-sigs/agent-sandbox` installed in the cluster (`kubectl apply -k ...`).
- A `SandboxTemplate` resource named `go-runtime-template` (from the upstream `gke-demos/go-runtime-sandbox` manifests).
- `gcloud auth application-default login` completed.
- A real GCP project with a few GCS buckets and `storage.buckets.list` permission on the user's account.

**Pass criterion**: `kode-gopher exec testdata/list_buckets.go` produces output matching:
```
gcloud storage buckets list --format=json | jq '[.[] | {name, timeCreated}] | sort_by(.timeCreated)'
```

## Slice 1 — result contract + normalize

**Scope**:
- `internal/wrapper/template.go.tmpl`: wraps `func run(ctx context.Context) (any, error)`; main marshals the result to `/app/.kode-gopher/result.json`; recovers panics into the same file with a discriminator (`{"kind": "panic", ...}`); respects a pre-written result.json (so users submitting full `package main` can write it themselves).
- `internal/normalize/normalize.go`: `package main` detection via `go/parser`; merges optional `extra_imports[]` into a synthesized go.mod section.
- `internal/executor/run.go`: split into `Build`, `Run`, `Fetch` phases; each returns a typed error so the caller knows whether it was a compile failure, a runtime failure, or a result-marshal failure.
- `cmd/kode-gopher/exec.go`: updated to use them.

**Pass criterion**: a `func run(ctx) (any, error)` snippet (no `package main`) round-trips through the executor and produces the same structured result as the slice-0 program. Compile errors surface with `phase: build`; panics surface with `phase: run, result.kind: panic`.

## Slice 2 — MCP server

**Scope**:
- `internal/mcp/server.go`: registers `execute_go_code` only.
- `cmd/kode-gopher/serve.go`: stdio transport.

**Pass criterion**: `mcp-inspector` connects to local `kode-gopher serve`; calling `execute_go_code` with a list-buckets snippet (no `package main` — exercises the wrapper) returns the same data as slice 0, framed as MCP structured content.

## Slice 3 — GKE deployment

**Scope**:
- `internal/curated/packages.go`: canonical list of pre-cached GCP packages — `storage`, `compute`, `container` (GKE), `bigquery`, `pubsub`, `secretmanager`, `run`, `monitoring`, `logging`, `iam`, `resourcemanager`, plus `google.golang.org/api/option`.
- `internal/prewarm/main.go`: imports each curated package, invokes a no-op constructor per service.
- `sandbox/Dockerfile`: extends upstream go-runtime-sandbox image (replaced by our own image in slice 7), copies the prewarm binary, runs it at build to populate `$GOCACHE` and `$GOMODCACHE`. Push to Artifact Registry.
- `manifests/base/sandboxtemplate.yaml`: references the new image; `runtimeClassName: gvisor`.
- `manifests/overlays/gke/workloadidentity.yaml`: KSA bound to a GSA with whatever GCP permissions the model's code should have (locked-down for v1).
- **Deferred to slice 4**: NetworkPolicy, warm pool. Keeping this slice focused on the auth + image story.

**Prerequisites**:
- GKE Autopilot cluster with agent-sandbox addon.
- Artifact Registry repository.
- A test GSA with `storage.buckets.list` on the test project.
- Workload Identity Federation configured on the cluster.

**Pass criterion (canonical demo)**:
- Claude Desktop attached via stdio to `kode-gopher serve` (running locally, pointed at the GKE cluster via kubeconfig).
- Prompt: *"List all GCS buckets in project X and return their names and creation times sorted oldest first."*
- First call completes <60s (cold).
- Second identical call completes <10s (warm caches).
- Result matches `gcloud`.

## Slice 4 — production hardening

**Scope**:
- `internal/mcp/lookup_package_docs.go` and `internal/mcp/gcp_auth_status.go` tools.
- `internal/creds/` formalized as the unified `CredentialSource` interface; `cmd/kode-gopher/auth status` subcommand.
- `manifests/warmpool.yaml` (SandboxWarmPool, replicas=2, OnReplenish).
- `internal/prompts/system.md` generated from `curated.Packages` at build time.
- **Multi-file program support**: extend `internal/normalize` to accept a `map[string][]byte` of source files and detect which one declares `func main`/`func run`; add `files?: map[string]string` as an additive field on the MCP tool args (keep `code: string` for the common single-file case). Lets the model write helper packages instead of inlining everything into one `main.go`. The agent-sandbox layer already supports multi-file materialization — this is purely a tool-surface change. Deferred from slice 2 because no canonical workflow asked for it yet.
- `cmd/kode-gopher serve --context` flag — construct the agent-sandbox client with an explicit kubeconfig context rather than inheriting ambient `kubectl config current-context`. Closes the slice-1.5 / slice-1.7 context-drift footgun.
- `internal/sandbox` recreate-on-session-death: today a fatal Execute error propagates and the next call opens fresh. Add explicit close+reopen on the well-known fatal errors so a pod dying mid-call self-heals.

**Pass criteria**:
- Snippet calling `storage.NewClient`, `secretmanager.NewClient`, `aiplatform.NewClient` in one execution must succeed without re-downloading modules (verify via `Result.Duration`).
- `gcp_auth_status` reports `mode=workload, identity=<GSA email>` in-cluster and `mode=forwarded, identity=<user email>` on desktop.
- With forwarded mode, revoking the refresh token externally → next `execute_go_code` must fail fast with a clear `needs_relogin` error, not a deep SDK 401.
- Multi-file snippet declaring a helper package and importing it from `main.go` round-trips end-to-end through both the CLI and the MCP tool.
- Passing `--context=kind-other-cluster` to `serve` reaches the right cluster regardless of ambient `kubectl config current-context`.

## Slice 5 — HTTP / SSE transport

> **2026-09-30:** the in-cluster answers to the questions below are in `docs/design-in-cluster.md` (slice 9). Streaming stays open.

Adds a second transport mode to `kode-gopher serve` so the MCP server can be reached over TCP from clients that aren't co-located. Significantly larger than the previous slices because several things that stdio sidesteps become real design questions.

**Scope (to firm up before starting; the questions below are the gate):**
- A second `--transport=http` flag on `kode-gopher serve` (default still `stdio`). Binds an `--addr` and serves the MCP streamable-HTTP endpoint via the SDK's HTTP handler.
- Per-connection or per-request session topology, depending on the answers below.
- Authentication on the HTTP listener (token, OIDC, IAP — depending on the topology).
- SSE streaming variant of the executor's Run/Fetch loop so partial stdout can flow back during a long-running tool call (rather than landing only at the end as a single response).
- GKE manifests for the HTTP path: a Service, an Ingress or Gateway, IAP or other front-end auth, NetworkPolicy ingress rules.

**Open design questions (don't pick answers until we start):**
1. **Session topology**. Three plausible shapes:
   - *Per-connection* — one sandbox session per HTTP/SSE connection, lifetime tied to the connection. Mirrors stdio's "one client one session" semantics. Connections must be long-lived or session-open cost dominates.
   - *Per-request* — sandbox claimed from a warmpool per tool call, released after. Simpler scaling. Loses the warm-`$GOCACHE` benefit between calls on the same session.
   - *Per-end-user* — one session pinned to an authenticated identity, shared across that user's requests. Best UX for multi-tenant; needs the auth model from question 2.
2. **Auth**. What identifies a caller?
   - Static bearer token (operator-issued; deployment-wide trust).
   - OIDC / IAP (per-end-user identity from the front-end; full multi-tenant).
   - mTLS (in-cluster / service-to-service).
3. **Per-end-user credentials**. If we go multi-tenant per-user, every tool call has to run under that user's GCP identity, not the server's. That's the deferred slice-1 3LO work, plus the storage layer (where do we keep per-user refresh tokens?), plus session-scoped GOOGLE_APPLICATION_CREDENTIALS injection.
4. **Streaming**. SSE specifically buys progressive output. The current executor returns one final response; the wrapper writes one final result.json. Adding streaming likely means:
   - Mid-call tool-progress events (stdout/stderr chunks as they happen).
   - A streaming variant of the result protocol (or just stdout streaming with the structured result still landing once at the end).
5. **Deployment topology**. In-cluster (a Service in `codemode` namespace), Cloud Run, both? Different auth front-ends, different NetworkPolicy ingress, different scaling.

**Pass criteria (sketch, to be refined):**
- `kode-gopher serve --transport=http --addr=:8080` accepts a connection from a stock MCP HTTP client (e.g. mcp-inspector against `http://localhost:8080/mcp`) and runs `execute_go_code` end-to-end against the same sandbox infra slice 3 uses.
- A long-running snippet (e.g. one that `time.Sleep(20*time.Second)`s with periodic `fmt.Println`s) streams its stdout to the client during the call rather than buffering to the end.
- Whatever auth model we pick is enforced: a request without valid credentials is rejected with the right MCP-level error.

## Slice 6 — Yaegi runtime: SHELVED 2026-09-24

The plan was to add an opt-in second runtime backed by the [Yaegi](https://github.com/traefik/yaegi) Go interpreter, for millisecond execution. Everything about packaging worked:
- gRPC and REST clients under the interpreter;
- a forked extractor that takes client-go extraction from never-finishing to 2 s;
- symbol packs loaded via `plugin.Open`, including under gVisor on GKE Autopilot;
- a 56 MB runner image, against 2.2 GB for the compiled image.

The interpreter itself is what failed. A differential corpus (`experiments/yaegi-poc/cmd/kg-difftest`, 32 idiomatic snippets checked against real Go) fails 16 of 32 even with our patch for a named-result bug. 7 of those failures are **silent wrong output**. The worst is `json.Marshal` of snippet-defined structs, which leaks unexported fields, ignores `MarshalJSON`, and doesn't flatten embedded structs. Upstream `master` fixes only one of the 16. A validator can't catch these classes without rejecting most real snippets.

The experiment stays in `experiments/yaegi-poc/` as the record, and as a two-minute recheck if yaegi (or another interpreter) improves. Details: `docs/decisions.md > Differential corpus`, plus the earlier slice-6 entries.

What carries forward into slice 7:
- gVisor + Autopilot behavior;
- delivering read-only artifacts to sandbox pods via initContainer → emptyDir, because Autopilot rejects `image:` volumes;
- the image-size numbers.

## Slice 7 — fast compiled path

Goal: cut compiled-path latency on GKE + gVisor without giving up the Go toolchain as the source of truth.

**Gate — measured ✅ 2026-09-27** (`docs/decisions.md > Slice 7 gate`, harness in `scripts/measure-build/`):
- **The prewarmed `$GOCACHE` already works.** Only the snippet's own package compiles.
- **A warm `tidy` + `build` costs ~7-8 s under gVisor** (~3.5 s without gVisor), mostly linking plus gVisor filesystem overhead.
- **Slice 1.7's ~55 s came from a stale GHCR image** that predates the lockfile bootstrap. The cache missed, and GCS builds took ~70 s. client-go builds OOM at 4 GiB.

**Scope:**
- ✅ **Image publishing that can't go stale, and an image that's all ours** (done 2026-09-29; `docs/decisions.md > Slice 7: our own sandbox image`). Our own `cmd/sandbox-server`, which implements the agent-sandbox runtime protocol, replaces the go-runtime-sandbox base image. Base images are pinned by digest, and the tag is derived from the image's inputs. CI publishes to `ghcr.io/gke-demos/kode-gopher/sandbox` on `main` and fails PRs with a stale overlay pin. The executor names a missing `/opt/kode-gopher-base/go.mod`.
- ✅ **Skip `go mod tidy` when the lockfile already covers every import** (done 2026-09-29; `docs/decisions.md > Slice 7: fast path`). The build phase tries `go build` first and runs tidy only when the build reports a missing module. That covers `extra_imports` and caller `go.mod`s with no special case.
- ✅ **`-ldflags='-s -w'`** on the snippet build.
- ✅ **Guard against cache-miss blowups.** When tidy moves a lockfile-pinned version, the tool result carries a warning naming each move. The memory limit stays at 2 GiB (reasoning in decisions.md).
- ✅ **Investigate, measure first: gVisor filesystem.** Filesystem access was worth ~1 s; the rest was slow CPUs. Sandbox pods now run on C3-first nodes through a custom ComputeClass: builds ~3× faster. The image also pushes `$GOCACHE` mtimes into the future, which saves ~6 s on each fresh pod's first build.
- ✅ **The GKE agent-sandbox addon's move to v1beta1**, found on the fresh cluster, is handled in the overlay (token mount, warm-pool name, Service, NetworkPolicy). The client and manifest migration followed in slice 8.

**Pass criteria** (all met 2026-09-29: GCS snippet 4.3-4.4 s wall-clock end to end):
- `scripts/smoketest-gke.sh` on a fresh cluster from the CI-published image: GCS snippet end to end under 12 s warm (today's measured build floor ~7 s under gVisor, plus the agent-sandbox round trips).
- A curated-only snippet runs no `go mod tidy`; a snippet with `extra_imports` still works.
- The published image and the executor can't drift: the image tag in the overlay matches a CI build of the same commit.

## Slice 8 — agent-sandbox v1.0

Goal: run on agent-sandbox v1.0 (v1beta1) natively instead of relying on the addon's v1alpha1 conversion, and settle what Kubernetes access a sandbox has now that GKE forbids mounting its token.

**Scope** (done 2026-09-29; `docs/decisions.md > Slice 8`):
- ✅ **Client v1.0.4.** `internal/sandbox` claims from a warm pool (`Options.WarmPool`); the CLI and MCP server use `go-runtime-pool`. Requests route through the sandbox's Service, not a cached pod IP.
- ✅ **v1beta1 manifests, one base for kind and GKE.** SandboxTemplate, SandboxWarmPool `go-runtime-pool` (claims name it; the `shadow-pool-` name is gone), and the network policy all live in `manifests/base`. The overlay keeps only GKE specifics.
- ✅ **Upstream Go sandbox-router** (`registry.k8s.io/agent-sandbox/sandbox-router-go:v1.0.4`) in the base, DNS-only, header timeout raised to 300 s. kind no longer builds a router, and GKE no longer pulls one from gke-demos' Artifact Registry.
- ✅ **No in-cluster access.** Sandboxes mount no KSA token on kind or GKE. Snippets reach any GKE cluster, their own included, through the DNS endpoint with Google credentials. `list_k8s_version_snippet.go` is gone; the prompt and design doc say so.

**Pass criteria** (met 2026-09-29):
- `scripts/smoketest-kind.sh --compare` and `scripts/smoketest-mcp.sh --target=kind --compare` pass on agent-sandbox v1.0.4.
- `scripts/smoketest-gke.sh --compare` passes on a fresh rapid-channel Autopilot cluster, with claims bound to `go-runtime-pool`.

## Slice 9 — in-cluster kode-gopher (proposed)

Goal: users add one URL to their MCP client and sign in with Google. They need no kubeconfig, no Kubernetes RBAC and no local binary, and snippets run as the signed-in user. Design: `docs/design-in-cluster.md`.

**Build order** (each step ships separately):
1. sandbox-server GCE metadata emulator (access token only in the sandbox) and in-cluster connectivity (no router).
2. HTTP transport: one sandbox per MCP session, claim leases via `spec.lifecycle.shutdownTime`, per-user session cap.
   Agent Identity vault spike, finished 2026-09-30: phases A and B passed, including silent renewal after expiry, provided the requested scopes match the stored grant exactly. Step 3 uses the vault for custody, with the sealed envelope as fallback.
3. kode-gopher as a spec-compliant OAuth 2.1 authorization server fronting Google sign-in: PKCE (S256), RFC 8707 resource indicators, CIMD + DCR + pre-registered clients; stateless sealed tokens; allow-list by Google group (required) and domain; all Google-side settings per-deployment config.
4. GKE manifests (Gateway, cert, network policies including router lock-down) and `scripts/smoketest-http.sh`.
5. Service-identity mode and the client-credentials extension (separate step).

## Critical files for MVP (slice 0 + slice 1)

These are what to create first; everything else is dead weight until the loop works.

1. **`cmd/kode-gopher/exec.go`** — slice-0 CLI. Opens `goruntime.Session`, calls `Execute`, prints `pkg/format.Result`. ~80 LOC. Validates the whole upstream API understanding in isolation, with no abstraction overhead to debug.
2. **`internal/executor/session.go`** — owns the `*goruntime.Session` + a `sync.Mutex`; lazy open with optional reattach via `ClaimName`; recreate-on-death. Choke point for every later feature.
3. **`internal/executor/run.go`** — orchestrates Build phase (`go build`), Run phase (`./bin/run`), Fetch phase (`cat result.json`). Returns `Outcome{Phase, Stdout, Stderr, Result, ExitCode}`. This is the contract every MCP tool will surface.
4. **`internal/wrapper/template.go.tmpl`** — wrapper template. Will be edited dozens of times as the model contract is tuned. Must support `run(ctx) (any, error)`, panic recovery, `json.Marshal` failure handling (write an error-typed discriminated result.json), and respect a `result.json` already written by user code.
5. **`internal/normalize/normalize.go`** — snippet vs full-main detection and wrapping. Where `extra_imports` get merged into the synthesized `go.mod`. Pure functions; unit-testable without a cluster.

## Open items / decisions deferred

- **Native 3LO OAuth flow** (`kode-gopher auth login`). Deferred until a use case requires it (e.g., headless server with no developer workstation).
- **OAuth client distribution** if 3LO is added. Likely "BYO required, plus `--use-gcloud-adc` escape hatch".
- **Multi-tenancy**. Current design is one MCP server process per credential context. A SaaS deployment would need per-request session pools and per-end-user credential plumbing. **Folded into slice 5** if/when HTTP transport gets a multi-tenant shape.
- **`$GOCACHE` PVC** for cross-pod persistence. Prewarm covers most of the value; slice 7's measurements show the shipped `GOCACHE` already hits fully, so a PVC would only help cache-miss builds (`extra_imports` moving versions).
- **Sandbox egress filtering**. Delegated to the agent-sandbox controller: `SandboxTemplate.spec.networkPolicy` (with `networkPolicyManagement: Managed`) generates a shared NetworkPolicy per template. On GKE Autopilot's Dataplane V2 (Cilium), FQDN-based egress rules are natively supported — no ipranges-refresh CronJob needed. Non-Cilium CNIs get CIDR-only rules; document CNI requirements in README rather than shipping our own filtering layer.
- **Non-GCP tool surface**. If we ever want to give the sandbox access to capabilities outside the GCP SDK (e.g., a "secrets" tool), we'll need to add the host↔sandbox RPC bridge we deliberately skipped. Add only when there's a real reason.

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
- `sandbox/Dockerfile`: extends upstream go-runtime-sandbox image, copies the prewarm binary, runs it at build to populate `$GOCACHE` and `$GOMODCACHE`. Push to Artifact Registry.
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
- `manifests/overlays/gke/networkpolicy.yaml` + `ipranges-refresh.yaml` (CronJob refreshes a ConfigMap of allowed CIDRs from `gstatic.com/ipranges/goog.json`).
- `manifests/warmpool.yaml` (SandboxWarmPool, replicas=2, OnReplenish).
- `internal/prompts/system.md` generated from `curated.Packages` at build time.

**Pass criteria**:
- Snippet doing `http.Get("https://example.com")` must fail (egress blocked).
- Snippet calling `storage.NewClient`, `secretmanager.NewClient`, `aiplatform.NewClient` in one execution must succeed without re-downloading modules (verify via `Result.Duration`).
- `gcp_auth_status` reports `mode=workload, identity=<GSA email>` in-cluster and `mode=forwarded, identity=<user email>` on desktop.
- With forwarded mode, revoking the refresh token externally → next `execute_go_code` must fail fast with a clear `needs_relogin` error, not a deep SDK 401.

## Slice 5 — HTTP / SSE transport

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
- **`$GOCACHE` PVC** for cross-pod persistence. Prewarm covers most of the value; revisit if cold-start latency stays painful after slice 4.
- **L7 egress filtering** (e.g., transparent proxy that allowlists by hostname). L3 IP allowlist is the v1 approximation.
- **Non-GCP tool surface**. If we ever want to give the sandbox access to capabilities outside the GCP SDK (e.g., a "secrets" tool), we'll need to add the host↔sandbox RPC bridge we deliberately skipped. Add only when there's a real reason.

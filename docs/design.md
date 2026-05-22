# kode-gopher — design

## Problem

Multi-step Google Cloud workflows from an LLM agent are awkward today. The two common options both have real costs:

1. **Chain many MCP tool calls.** One MCP tool per service action (`list_buckets`, `get_bucket_iam`, `compose_object`, ...). Each step round-trips through the model's context, intermediate results are paid for in tokens, and the model has to glue them with prose. Multi-call workflows balloon in latency and cost.
2. **Hand-write a bespoke "GCP super-tool" MCP server.** Maintain N tools, one per useful gcloud-equivalent action. High maintenance, never covers the long tail.

Cloudflare's [Code Mode](https://blog.cloudflare.com/code-mode/) showed a third option: give the model *one* tool that takes code, expose the rest of the tools as a typed SDK the code can call, run the code in a sandbox, and return only the final result. Their bet — that LLMs are meaningfully better at writing code (which they've seen tons of in training) than at emitting tool-call tokens (which they've seen only synthetic traces of) — held up.

## The wedge insight for Go + GCP

For the GCP-on-Go case, **the "SDK" the model would write against already exists as a real, importable thing**: `cloud.google.com/go/storage`, `cloud.google.com/go/compute`, `cloud.google.com/go/container`, BigQuery, Pub/Sub, etc. The model has been trained on heaps of real Go using these packages.

That collapses Cloudflare's design significantly. We don't need to generate typed SDK stubs from MCP tool schemas, and we don't need a host-side RPC bridge to mediate tool calls. We just:

1. Let the model write a normal Go program that imports real `cloud.google.com/go/...` packages.
2. Compile and run that program in a sandboxed pod.
3. Return stdout + a structured result.

One agent step → many GCP API calls in real code → one structured result back to the model.

## Architecture

```
┌─────────────┐   stdio MCP   ┌──────────────────────┐    k8s API    ┌──────────────────┐
│ MCP client  │ ────────────► │   kode-gopher serve     │ ────────────► │  GKE Autopilot   │
│ (Claude,    │               │   (Go binary)        │               │  + agent-sandbox │
│  agent)     │ ◄──────────── │                      │               │                  │
└─────────────┘    result     │ ┌──────────────────┐ │  port-fwd     │ ┌──────────────┐ │
                              │ │ tool: exec_go    │ │  ───────────► │ │ sandbox pod  │ │
                              │ │ tool: lookup_doc │ │               │ │ (gVisor)     │ │
                              │ │ tool: auth_stat  │ │               │ │ + Go SDK     │ │
                              │ └──────────────────┘ │               │ │ + GOCACHE    │ │
                              │ ┌──────────────────┐ │               │ │ + WI / token │ │
                              │ │ creds: ADC fwd   │ │               │ └──────────────┘ │
                              │ │       / WI       │ │               │ NetworkPolicy:   │
                              │ └──────────────────┘ │               │ *.googleapis.com │
                              └──────────────────────┘               └──────────────────┘
```

Three MCP tools, one execution backend, one credential abstraction.

### Components

- **`kode-gopher serve`** — an MCP server (stdio transport) built on `github.com/modelcontextprotocol/go-sdk`. Hosts the tool registrations.
- **Executor** — owns one long-lived `*goruntime.Session` (from `github.com/gke-demos/go-runtime-sandbox/pkg/goruntime`) for the lifetime of the server process. Serializes calls with a mutex. Calls `Reset` between invocations to clear `/app` while preserving `$GOCACHE` and `$GOMODCACHE`.
- **Wrapper / normalizer** — accepts either a snippet (a `func run(ctx) (any, error)` body) or a full `package main`. If a snippet, wraps it in a template that handles the result-file write, panic recovery, and JSON marshaling. If a full main, uses it verbatim and trusts the program to write the result file itself.
- **Credential source** — abstraction over two implementations: `forwarded` (host reads local ADC JSON, materializes it into the sandbox Files map under `/app/.kode-gopher/creds/adc.json`, sets `GOOGLE_APPLICATION_CREDENTIALS` for the run command) and `workload` (no-op materialize; rely on the pod's metadata server).
- **Sandbox image** — extends the upstream `go-runtime-sandbox` image. A build-time `prewarm` program imports every curated GCP package once so `$GOCACHE` and `$GOMODCACHE` are populated before first user execution.
- **GKE manifests** — `SandboxTemplate` (referencing our image, `runtimeClassName: gvisor`), Workload Identity binding (KSA ↔ GSA), NetworkPolicy (Google IP allowlist), `SandboxWarmPool` (replicas=2).

### MCP tools

| Tool | Purpose | Shape |
| --- | --- | --- |
| `execute_go_code` | The main event. Compiles + runs Go in the sandbox. | `{code, extra_imports?[]}` → `{phase, stdout, stderr, result, exit_code, duration_ms, truncated?}` |
| `lookup_package_docs` | `go doc <import-path> [symbol]` inside the sandbox. Read-only, shares the Session, no Reset. | `{import_path, symbol?}` → `{docs}` |
| `gcp_auth_status` | Host-side. Reports which credential mode the next exec will run under, the identity, and (for forwarded) when the token expires. | `{}` → `{mode, identity, expires_at?}` |

OAuth login is **not** exposed to the model — it's operator-driven (run `gcloud auth application-default login` on the host).

## Transport modes

The MCP server speaks the standard MCP wire protocol; the transport underneath is pluggable. Two modes:

**stdio (today).** `kode-gopher serve` reads JSON-RPC frames on stdin and writes them on stdout. Spawning a `kode-gopher serve` subprocess is how MCP clients (Claude Desktop, Gemini CLI, the SDK's `CommandTransport`) connect. Point-to-point: one MCP client per server process; the server identity is the spawning user's identity. Every section below — authentication, session lifecycle, sandbox boundary — describes this mode.

**HTTP / SSE (planned, slice 5).** Streamable HTTP transport per the MCP spec: clients connect over TCP, the server fans out to many clients, and tool results can stream back via SSE rather than landing as a single final response. Several things that are simple in stdio become design questions in HTTP mode — surfaced in [`plan.md`](./plan.md)'s slice 5 rather than answered here. The big ones:

- **Session topology**: one sandbox per HTTP connection (mirrors stdio), per request (pooled, smaller blast radius but $GOCACHE benefit gone), or per authenticated end-user (multi-tenant)?
- **Auth**: a TCP server can't inherit identity from a parent process. Bearer token? OIDC? GCP IAP if hosted behind an Ingress?
- **Per-end-user credentials**: stdio sidesteps this (sandbox runs as whoever launched the server). HTTP multi-tenant deployments need per-request credential context — likely the 3LO flow we deferred in slice 1.
- **Streaming**: SSE specifically enables progressive tool output. Wrapper / executor protocol would need a streaming variant of the result-fetch step.

Until that slice lands, everything in this document assumes stdio.

## Authentication model

Two modes resolved at server start from `--auth-mode={auto,forwarded,workload}` (default `auto`):

### `forwarded` (desktop / personal use)
The host reads `~/.config/gcloud/application_default_credentials.json` — an `authorized_user`-typed file produced by `gcloud auth application-default login` containing `client_id`, `client_secret`, `refresh_token`. The executor copies this file into the sandbox Files map at `/app/.kode-gopher/creds/adc.json` for each invocation and runs the user's program with `GOOGLE_APPLICATION_CREDENTIALS` pointing at it.

Inside the sandbox, `google.FindDefaultCredentials(ctx)` reads that file, exchanges the refresh token at `oauth2.googleapis.com` for a short-lived access token, and **all GCP calls happen as the user**, scoped by whatever IAM that user has. The pod's Workload Identity binding (if any) is inert — `GOOGLE_APPLICATION_CREDENTIALS` takes precedence over the metadata server.

We fail fast if the refresh fails (deleted ADC file, revoked token, malformed JSON) with a clear `needs_relogin` error rather than letting a stale token reach the SDK.

### `workload` (in-cluster / service deployments)
No credentials are materialized into the sandbox. The pod is bound to a GSA via Workload Identity Federation; `google.FindDefaultCredentials(ctx)` inside the sandbox hits the metadata server on `169.254.169.254` and gets short-lived tokens for that GSA. Calls happen as the GSA.

### Why not 3LO in our own binary?
A native `kode-gopher auth login` flow would require us to either embed an OAuth client ID/secret (which means we own a GCP project and assume any operational responsibility that comes with it) or require every user to register their own client. Forwarding ADC sidesteps both: `gcloud` already solved this. We can add our own flow later if a real use case demands it (e.g., headless server deployment without a developer workstation).

## Result protocol

User code (or the wrapper, when wrapping a snippet) writes a JSON document to `/app/.kode-gopher/result.json`. The host fetches it via a second, cheap `Execute` with `cat`.

Why a file instead of a stdout sentinel:
- Stdout stays clean — every byte is a user log.
- No escaping problems (no risk of user output containing the sentinel by accident).
- The wrapper can encode richer discriminated states: `{"kind": "ok", "value": ...}`, `{"kind": "error", "message": ...}`, `{"kind": "panic", "stack": ...}`, `{"kind": "marshal_error", "type": "..."}`.
- The contract is symmetric: a user submitting a full `package main` writes their own `/app/.kode-gopher/result.json`; the wrapper case writes the same shape for them.

## Build model

Inside the sandbox, every invocation is:

```
go build -o /app/.kode-gopher/bin/run .
/app/.kode-gopher/bin/run
cat /app/.kode-gopher/result.json
```

Splitting build from run separates compile errors from runtime errors at the protocol level. The executor surfaces `Outcome{Phase, Stdout, Stderr, Result, ExitCode}` where `Phase ∈ {build, run, fetch}`, so the model gets unambiguous error attribution. A model that sees `phase: build` knows to fix syntax/types; a model that sees `phase: run` knows the program compiled but something happened at runtime.

## Session lifecycle

**Applies to stdio mode.** HTTP/SSE mode introduces per-connection or per-request sessions; see the Transport modes section above.

One `*goruntime.Session` per MCP server process for its entire lifetime. Serialized by `sync.Mutex` — concurrent tool calls are processed one at a time. In stdio mode, an MCP server has exactly one client (the spawning process), so this also means "one session per client."

- **Lazy open** — first tool call triggers `goruntime.Open`. Subsequent calls reuse the session.
- **Reset between calls** — ephemeral default. `Session.Reset` clears `/app` but preserves `$GOCACHE` / `$GOMODCACHE`, so the second call against the same packages reuses cached build artifacts.
- **Recreate on death** — if `Execute` returns a session-fatal error, close and reopen.
- **Reattach option** — `--claim-name` allows reattaching to a long-lived sandbox claim across server restarts. Useful for development; off by default in production.

`lookup_package_docs` shares the same Session but doesn't Reset (it's read-only), so it doesn't pollute the workspace.

## Sandbox boundary

| Layer | Mechanism |
| --- | --- |
| Process isolation | `runtimeClassName: gvisor` on GKE Autopilot with the agent-sandbox addon. User-space syscall interception. |
| Filesystem | Per-pod scratch only; nothing persists across pod lifetimes unless explicitly written via a PVC. We don't grant one. |
| Network egress | NetworkPolicy allowlist: Google's published IP ranges (`gstatic.com/ipranges/goog.json`) + metadata server (`169.254.169.254`) + `oauth2.googleapis.com` + `sts.googleapis.com`. Refreshed weekly via CronJob → ConfigMap. **L3 only** — not L7. A determined attacker who finds a non-Google service hosted on a Google IP could exfiltrate, but it raises the floor substantially over open egress. |
| Credential scope | Forwarded mode: scoped to the human user's IAM. Workload mode: scoped to the bound GSA's IAM — we recommend least-privilege. |
| Code review | None — by design. The whole point is the model writes arbitrary code. Containment is the security model. |

## File layout

```
kode-gopher/
├── cmd/kode-gopher/
│   ├── main.go              # cobra root: serve | exec | auth
│   ├── serve.go             # MCP server entrypoint (stdio)
│   ├── exec.go              # CLI: kode-gopher exec <file.go> — useful dev tool; born in slice 0
│   └── auth.go              # `auth status` only in v1
├── internal/
│   ├── mcp/
│   │   ├── server.go               # tool registration on modelcontextprotocol/go-sdk
│   │   ├── execute_go_code.go
│   │   ├── lookup_package_docs.go
│   │   └── gcp_auth_status.go
│   ├── executor/
│   │   ├── session.go       # owns *goruntime.Session + sync.Mutex; lazy open; recreate-on-death
│   │   ├── run.go           # Build/Run/Fetch phases; returns Outcome
│   │   └── files.go         # materialize main.go, go.mod, creds/adc.json into Files map
│   ├── wrapper/
│   │   ├── template.go      # //go:embed of template.go.tmpl
│   │   └── template.go.tmpl # the func run(ctx) wrapper; panic recovery; marshal-error handling
│   ├── normalize/
│   │   └── normalize.go     # package-main detection; extra_imports merge; pure & unit-testable
│   ├── creds/
│   │   ├── source.go        # CredentialSource interface: Mode/IdentityHint/MaterializeForSandbox/Expiry
│   │   ├── forwarded.go     # reads ~/.config/gcloud/application_default_credentials.json
│   │   ├── workload.go      # no-op materialize; identity from metadata server
│   │   └── resolve.go       # auto/forwarded/workload mode resolution
│   ├── curated/
│   │   └── packages.go      # single source of truth: pre-cached GCP packages
│   ├── prewarm/
│   │   └── main.go          # imports every curated package; run once during image build
│   └── prompts/
│       ├── prompts.go       # //go:embed system.md; templates in curated.Packages
│       └── system.md
├── sandbox/
│   └── Dockerfile           # FROM upstream go-runtime-sandbox image; COPY prewarm; RUN it
├── manifests/
│   ├── base/
│   │   ├── sandboxtemplate.yaml
│   │   └── kustomization.yaml
│   ├── overlays/gke/
│   │   ├── workloadidentity.yaml   # KSA <-> GSA binding template
│   │   ├── networkpolicy.yaml      # Google IP ranges + metadata + oauth2/sts
│   │   ├── ipranges-refresh.yaml   # weekly CronJob; updates a ConfigMap
│   │   └── kustomization.yaml
│   └── warmpool.yaml        # SandboxWarmPool: replicas 2, OnReplenish
├── docs/
│   ├── design.md            # this document
│   └── plan.md              # the build sequence
├── go.mod
├── Makefile
└── README.md
```

## Locked decisions

- **MCP SDK**: `github.com/modelcontextprotocol/go-sdk` (official, v1.5.0+).
- **Sandbox client**: depend on `github.com/gke-demos/go-runtime-sandbox/pkg/goruntime` (no fork).
- **Auth**: `forwarded` + `workload`, resolved by `--auth-mode={auto,forwarded,workload}`. Native 3LO deferred.
- **Result protocol**: `/app/.kode-gopher/result.json`, fetched via a second `Execute`.
- **Build model**: `go build` then `./bin/run` (separate from build); phase-tagged `Outcome`.
- **Session**: one per server process, mutex-serialized, Reset between calls, recreate on death.
- **NetworkPolicy**: Google IP allowlist + metadata + oauth2/sts, weekly CronJob refresh.
- **Sliced delivery**: slice 0 on local `kind` to prove the API wiring; GKE Autopilot from slice 3 onward.
- **Pre-warm**: image build runs a `prewarm` program that imports every curated GCP package.

## Non-goals (for v1)

- A native `kode-gopher auth login` 3LO flow. We forward gcloud ADC. (Multi-tenant HTTP mode in slice 5 may force revisiting this.)
- Per-end-user credential isolation in multi-tenant SaaS deployments. The stdio MCP server is single-tenant per process; multi-tenancy belongs to slice 5's HTTP/SSE work.
- L7 egress filtering. NetworkPolicy is L3 IP allowlist only.
- cgo support. Upstream sandbox image deliberately excludes `gcc`.
- Persistent `$GOCACHE` across pod lifetimes (e.g. PVC-backed). Prewarm covers most of the value.
- Capabilities outside the GCP SDK. The model is told to use `cloud.google.com/go/*` only; we don't try to expose arbitrary tools through a host RPC bridge.
- Streaming results in stdio mode. One `execute_go_code` call returns one final response; long programs should log progress to stdout. Slice 5's SSE transport unlocks streaming.

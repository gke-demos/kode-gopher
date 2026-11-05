# `kode-gopher` vs. Code Mode (`philschmid.de/code-mode`)

**Reference:** Philipp Schmid, [*Why Agents Are Soon Only Going to Have 1 Tool*](https://www.philschmid.de/code-mode) (Nov 1, 2026)

> **Status (`v0.5.0`, Nov 5, 2026):** All six optimizations in the [Priority Roadmap](#suggested-priority-roadmap) below ([#66](https://github.com/gke-demos/kode-gopher/issues/66)–[#71](https://github.com/gke-demos/kode-gopher/issues/71)) shipped in **`v0.5.0`** via PRs [#73](https://github.com/gke-demos/kode-gopher/pull/73)–[#78](https://github.com/gke-demos/kode-gopher/pull/78). The analysis below is preserved as the baseline comparison that motivated `v0.5.0`.

## Verdict: Same Trajectory — and Ahead on Two Hard Problems

Your read is spot-on: **`kode-gopher` is already built on the exact architectural bet Schmid describes**, and in two places (`SDK fidelity` and `credential isolation`) it is meaningfully ahead of the generic JS/Python `exec(code)` proxy pattern in the post.

In Schmid's framing, **Code Mode** collapses dozens of granular MCP tools into a single `exec(code)` meta-tool so the model chains calls, runs parallel fan-outs (`Promise.all`), filters payloads in sandbox memory, and returns only a distilled summary to the LLM context window. Compare that with [`docs/design.md`](./design.md):

> *"One agent step → many GCP API calls in real code → one structured result back to the model."*

Where `kode-gopher` **leapfrogs** Schmid's generic proxy architecture:

1. **Zero synthetic `.d.ts` glue or RPC proxy tax:** Schmid's generic harness must compile MCP `inputSchema` + `outputSchema` into synthetic TypeScript `declare namespace tools { ... }` stubs and marshal every `await tools.<server>.<tool>()` call across a host bridge. `kode-gopher`'s wedge ([`docs/design.md`](./design.md)) skips synthetic schemas entirely — models write against real `cloud.google.com/go/*` and `k8s.io/client-go/*` packages they already know from pre-training, checked by the real Go typechecker at `phase=build` before a single network call fires.
2. **Production-grade identity & containment:** Schmid sketches host-side OAuth injection conceptually (`vm.runInNewContext` / QuickJS). Through Slice 9 ([`docs/design-in-cluster.md`](./design-in-cluster.md)), `kode-gopher` already runs gVisor pods on C3 nodes with OAuth 2.1 + Agent Identity Vault custody and a per-run ephemeral `127.0.0.1` GCE metadata emulator ([`cmd/sandbox-server/metadata.go`](../cmd/sandbox-server/metadata.go)) so refresh tokens never enter the pod and access tokens never touch disk or `go build`.

Where `kode-gopher` **trails or pays avoidable friction** is not the paradigm — it's **per-turn execution overhead, cross-turn sandbox state, doc-lookup turn taxes, output token efficiency, and policy guardrails**.

---

## Pillar-by-Pillar Scoreboard

| Code Mode Pillar (Schmid) | `kode-gopher` Today | Gap / Distance |
|---|---|---|
| **1. Collapse tool catalog into `exec(code)`** | 3 tools total: `execute_go_code` + 2 read-only helpers (`lookup_package_docs`, `gcp_auth_status`) in [`internal/mcp/server.go`](../internal/mcp/server.go). | **Aligned.** Effectively a 1-action-tool server. |
| **2. Parallel fan-out & loops in 1 turn** | Full Go concurrency (`goroutines`, `sync.WaitGroup`, `errgroup`) inside `func run(ctx)`. | **Aligned** (small prewarm polish needed for `golang.org/x/sync/errgroup`). |
| **3. Context window protection (`text()` only)** | Wrapper writes `result.json` capped at 256 KiB ([`internal/executor/executor.go`](../internal/executor/executor.go)); instructs model to filter/summarize on overflow. | **Close, needs tuning.** `renderText` still echoes `stdout` + indented JSON alongside structured output. |
| **4. Cross-turn state (`store(k, v)` / `load(k)`)** | One sandbox pod per MCP session, **but** `handleExecuteGoCode` calls `sess.Reset(ctx)` on *every* run ([`internal/mcp/execute_go_code.go`](../internal/mcp/execute_go_code.go)). | **Real gap.** `/app` is wiped every turn; no zero-token state store across turns. |
| **5. Sub-second sandbox turn latency** | ~3.3–4.4 s warm on GKE C3 ([`docs/decisions.md`](./decisions.md)); 4–5 sequential HTTP round-trips per `execute_go_code` call. | **Optimizable (~1–1.5 s win without an interpreter).** |
| **6. Zero "CLI exploration tax" via upfront types** | `lookup_package_docs` runs subsecond `go doc`, **but** costs a full extra LLM turn and rejects transitive modules (`k8s.io/api`, `protobuf`). | **Medium gap.** Extra round-trip + overly strict allowlist in `isCurated`. |
| **7. Multimodal return helpers (`image()`, `audio()`)** | Text + JSON only (`sdk.TextContent` in [`internal/mcp/execute_go_code.go`](../internal/mcp/execute_go_code.go)). | **Missing** (high value for Cloud Monitoring charts / GCS media). |
| **8. Policy proxy & destructive-call confirmation** | Direct sandbox TLS egress to `*.googleapis.com`; broad `cloud-platform` scope. No per-API-call interception. | **Intentional architectural trade-off** — needs scope/mode guardrails instead. |

---

## Concrete Optimizations (Ordered by Impact / Effort)

### 1. Cross-Turn State Persistence (`store` / `load` or `/app/.persist`)
Schmid highlights state persistence as one of Code Mode's biggest token savers:
> *"In standard tool calling, the prompt history is the only state store, so reusing a large query result in Turn 2 means keeping the raw JSON in the context window on every turn. Exposing a KV interface lets the agent persist state outside of the context window across turns."*

Right now, `kode-gopher` already pays to keep a dedicated sandbox alive per `Mcp-Session-Id` ([`internal/mcp/server.go`](../internal/mcp/server.go)), yet `handleExecuteGoCode` ([`internal/mcp/execute_go_code.go`](../internal/mcp/execute_go_code.go)) runs `Session.Reset` (`rm -rf -- * .[!.]*`) before every execution — wiping `/app` clean!

*Note:* [`docs/design-in-cluster.md`](./design-in-cluster.md) (section 2) actually states *"files persist between calls, and so does the `$GOCACHE`"*, which is currently untrue for `/app` because of `sess.Reset(ctx)`.

**Optimization:**
- Preserve a dedicated directory across `Reset()` within the same session (e.g., `/app/persist` or `/workspace`, while wiping source files and `.kode-gopher/`).
- Optionally inject a tiny helper in `kg_wrapper.go` (or a pre-generated local package) exposing typed `saveState(key string, v any) error` / `loadState(key string, dst any) error` backed by `/app/.persist/<key>.json`, and document it in [`internal/prompts/system.md`](../internal/prompts/system.md).
- **Payoff:** Turn 1 can pull 20,000 BigQuery rows or 800 GKE Pod specs into `/app/.persist/pods.json` and return a 10-line summary; Turn 2 drills into anomalies in milliseconds without re-hitting GCP APIs or stuffing megabytes into context.

---

### 2. Collapse the 4–5 Sandbox HTTP Round-Trips into One `/run-go` Call
Even after Slice 7's C3 + future-mtime optimizations, every single `execute_go_code` call executes **4 to 5 sequential HTTP requests** between `kode-gopher` and `sandbox-server`:

1. `POST /execute` (`rm -rf -- * .[!.]*` via `sess.Reset`)
2. `POST /upload` (`main.go` + `kg_wrapper.go`, or `.kg-upload.tar` + an extra `POST /execute` `tar -xf` when `files` has subdirectories)
3. `POST /execute` (`buildCmd`: copy `go.mod` + `go build`)
4. `POST /execute` (`./.kode-gopher/bin/run` with `Credentials`)
5. `POST /execute` (`cat .kode-gopher/result.json`)

Since Slice 7, **you own [`cmd/sandbox-server`](../cmd/sandbox-server/server.go)** — you are no longer constrained by `go-runtime-sandbox`'s generic primitives.

**Optimization:**
- Add a single native `POST /run-go` endpoint on `sandbox-server` accepting `{reset, files, build_cmd, env, credentials}`.
- `sandbox-server` resets source files via `os.Root` directly (no `sh -c rm`), writes `files` in-process (no multipart/tar shell-out), runs `build`, starts the metadata emulator + runs `./bin/run` if build succeeded, reads `.kode-gopher/result.json` directly from disk, and returns `{phase, build_ms, duration_ms, tidied, moved, stdout, stderr, result}` in **one HTTP round-trip**.
- **Payoff:** Eliminates 3–4 network round-trips (especially noticeable over `stdio` port-forward) and 3 shell process spawns per call, while keeping exact phase attribution (`build` vs. `run`) and credential isolation (build still runs before the metadata emulator starts).

---

### 3. Fix `lookup_package_docs` Allowlist Blind Spots & Indirect Module Tidy Penalties
Schmid emphasizes eliminating the *"CLI/tool exploration tax"* where models burn turns guessing types. In `kode-gopher`, when a model checks a type signature via `lookup_package_docs`, `isCurated` ([`internal/mcp/lookup_package_docs.go`](../internal/mcp/lookup_package_docs.go)) only permits packages whose module prefix appears in [`internal/curated/packages.go`](../internal/curated/packages.go).

Because of how `moduleRoot` is computed, **`lookup_package_docs` currently rejects core packages that are already in `$GOMODCACHE` and explicitly recommended by your own prompt**:
- `k8s.io/api/core/v1`, `k8s.io/api/apps/v1` — **rejected!** (`k8s.io/api` is a separate module from `k8s.io/client-go` and `k8s.io/apimachinery`, so the model cannot look up `Pod`, `Deployment`, or `Service` structs!)
- `google.golang.org/protobuf/types/known/timestamppb`, `durationpb`, `fieldmaskpb` — **rejected!** (Even though `system.md` tells the model *"the well-known protobuf types ... are compiled too"*.)
- `google.golang.org/genproto/...` and `cloud.google.com/go/iam` — **rejected!**
- `golang.org/x/oauth2/google` — **rejected!** (Even though `system.md` instructs the model to use `google.DefaultTokenSource`.)
- `golang.org/x/sync/errgroup` — **rejected!** (Go's idiomatic `Promise.all` equivalent.)

Even worse, check the `-mod=readonly` interaction on indirect modules: in [`internal/prewarm/go.mod`](../internal/prewarm/go.mod), `golang.org/x/oauth2` and `golang.org/x/sync` are `// indirect`. When a snippet directly imports `golang.org/x/oauth2/google` (as `system.md` recommends for GKE DNS access), `go build` sees an `// indirect` requirement imported directly by the main module, outputs `updates to go.mod needed`, and falls back to `go mod tidy` in `buildCmd` — paying unnecessary tidy latency on every GKE snippet!

**Optimization:**
1. Allow `lookup_package_docs` to query **any module present in `/opt/kode-gopher-base/go.mod`** (keep `packagePattern` regex validation so shell injection remains impossible; `go doc` inside `/opt/kode-gopher-base` already works offline against every module in `go.mod`).
2. Support **batched symbol lookups** (`queries: [{package, symbol}]` or `symbols: []string`) so the model can inspect `containerpb.ListClustersRequest`, `containerpb.Cluster`, and `corev1.Pod` in **one** tool call instead of 3 sequential turns.
3. Promote `golang.org/x/sync/errgroup`, `golang.org/x/oauth2/google`, `k8s.io/api/core/v1`, and `k8s.io/api/apps/v1` into `curated.Packages` (zero image bloat since those modules are already indirect deps in `internal/prewarm/go.mod`, and promoting them makes them direct requires so `go build` never falls back to `go mod tidy`).

---

### 4. Tighten Return Payload Hygiene (Stop Paying Twice for Output Tokens)
In Schmid's design, `exec()` returns **only** what was passed to `text()`. Look at what `handleExecuteGoCode` ([`internal/mcp/execute_go_code.go`](../internal/mcp/execute_go_code.go)) sends back today:
1. It returns both `CallToolResult.Content` (human-readable text via `renderText`) **and** `ExecuteGoCodeOutput` (`structuredContent` via `go-sdk`).
2. Inside `renderText`, `enc.SetIndent("", "  ")` pretty-prints `o.Result` with 2-space indentation — inflating token counts on nested slices/maps by 20–35% for clients reading text content.
3. On a clean `kind="ok"` snippet run (`phase=run, exit=0`), any incidental debug `fmt.Println` in `stdout` (up to 16 KiB) is still appended alongside `result`.

**Optimization:**
- Compact-encode `o.Result` in `renderText` (or omit pretty indentation past a small byte threshold, e.g. > 2 KiB).
- Nudge the prompt in [`internal/prompts/system.md`](../internal/prompts/system.md) explicitly toward the Code Mode discipline: *"Filter, join, and aggregate inside `run(ctx)` and return a compact summary struct/slice; avoid dumping raw GCP/Kubernetes API objects (`*pb.Cluster`, `corev1.Pod`) directly as `result.value`."* (One raw `corev1.PodList` with `managedFields` easily blows past 256 KiB!)

---

### 5. Multimodal Output Helpers (`image` / `artifacts` from Sandbox)
Schmid calls out a subtle superpower of Code Mode over bash:
> *"In an `exec` sandbox, calling `image(screenshot)` or `audio(clip)` tells the host runtime to attach the actual media part directly to the model's next turn."*

For GCP + GKE workflows, this is surprisingly practical:
- Rendering a PNG/SVG time-series chart from Cloud Monitoring (`monitoring/apiv3/v2`) or BigQuery query results so a multimodal model can visually spot anomalies/correlations, or
- Inspecting an image/PDF object fetched from GCS (`cloud.google.com/go/storage`).

**Optimization:**
- Allow user code (or a small helper) to drop files into `/app/.kode-gopher/attachments/` (bounded, e.g., ≤ 4 MiB, `image/png`, `image/jpeg`, `image/webp`).
- Have the executor collect them and append `&sdk.ImageContent{Data: ..., MIMEType: ...}` items to `CallToolResult.Content` in `handleExecuteGoCode`. MCP natively supports `ImageContent` and `AudioContent` on `CallToolResult`.

---

### 6. Read-Only Default / Step-Up Guardrails (Addressing the Policy Proxy Trade-off)
In Schmid's post, because every `tools.*` call transits the host proxy, the host can *"pause for user confirmation on destructive calls."* Because `kode-gopher` compiles real Go that talks directly to `*.googleapis.com`, you deliberately traded per-call RPC interception for native SDK fidelity.

That trade-off is the right one for `kode-gopher`, **except** that right now every OAuth sign-in requests full `https://www.googleapis.com/auth/cloud-platform` ([`docs/design-in-cluster.md`](./design-in-cluster.md)), meaning an exploratory *"check why my pods are crash-looping"* prompt runs with whatever `DeleteCluster` / `DeleteBucket` IAM permissions the signed-in user happens to hold.

**Optimization:**
- **Read-only credential mode / scope option:** Support a `--read-only` flag (or per-call `read_only: bool` arg on `execute_go_code`, or a separate read-only OAuth scope) that downscopes tokens using Google STS [Credential Access Boundaries (CAB)](https://cloud.google.com/iam/docs/downscoping-short-lived-credentials) or requests `cloud-platform.read-only` + `bigquery.readonly` / `devstorage.read_only` until the user/agent explicitly opts into a mutating run.
- Because `kode-gopher` already passes a per-run access token into `sandbox-server`'s metadata emulator, **CAB downscoping at Phase 2 (`run`) requires zero changes to how snippets are written**.

---

### 7. Expand Curated GCP Coverage Carefully (Cloud Run, Pub/Sub, IAM, GenAI)
To truly realize *"1 tool for all of GCP"*, compare [`internal/curated/packages.go`](../internal/curated/packages.go) against [`docs/plan.md`](./plan.md):
- **Currently prewarmed:** Storage, Compute, GKE (`container`), BigQuery, Secret Manager, Logging, Monitoring, Trace, `k8s.io/client-go`.
- **Common multi-step workflows still hitting cold `go mod tidy`:** Cloud Run (`cloud.google.com/go/run/apiv2`), Pub/Sub (`cloud.google.com/go/pubsub`), Resource Manager (`cloud.google.com/go/resourcemanager/apiv3`), IAM (`cloud.google.com/go/iam/admin/apiv1`), and Vertex AI / Gemini (`google.golang.org/genai` — enabling Schmid's *"sub-agents / specialist sub-model fan-out inside `exec`"*!).

*Caveat:* Each addition increases sandbox image size (currently ~2.73 GB uncompressed) and prewarm build time, so measure image delta per module before adding — whereas promoting already-indirect modules (`k8s.io/api`, `golang.org/x/sync`, `golang.org/x/oauth2`, `google.golang.org/protobuf`) is nearly free.

---

## Suggested Priority Roadmap

| Priority | Issue | Status | Optimization | Effort | Why First |
|---|---|---|---|---|---|
| **P0** | [#66](https://github.com/gke-demos/kode-gopher/issues/66) | Shipped ([#73](https://github.com/gke-demos/kode-gopher/pull/73), `v0.5.0`) | Promote `x/oauth2/google`, `x/sync/errgroup`, `k8s.io/api/{core,apps}/v1`, `protobuf/types/known/*` to curated + widen `lookup_package_docs` to all prewarm `go.mod` modules + batched `queries` | **Small** (~1 PR) | Fixes real friction: GKE snippets stop triggering `go mod tidy`, and `lookup_package_docs` covers `k8s.io/api` & `protobuf` types in one round-trip. |
| **P1** | [#67](https://github.com/gke-demos/kode-gopher/issues/67) | Shipped ([#74](https://github.com/gke-demos/kode-gopher/pull/74), `v0.5.0`) | Cross-turn `/app/persist` surviving `Session.Reset` | **Small** (~1 PR) | Unlocks stateful multi-turn data exploration with zero prompt-token cost; brings code in line with `design-in-cluster.md`. |
| **P1** | [#68](https://github.com/gke-demos/kode-gopher/issues/68) | Shipped ([#75](https://github.com/gke-demos/kode-gopher/pull/75), `v0.5.0`) | Single-hop `POST /run-go` on `cmd/sandbox-server` | **Medium** (~1–2 PRs) | Cuts 4–5 sequential HTTP calls + 3 shell spawns per run down to 1 hop. |
| **P2** | [#70](https://github.com/gke-demos/kode-gopher/issues/70) | Shipped ([#77](https://github.com/gke-demos/kode-gopher/pull/77), `v0.5.0`) | Opt-in `--read-only` / `read_only` OAuth scope downscoping | **Medium** | Gives `kode-gopher` a least-privilege guardrail without sacrificing direct Go SDK execution. |
| **P2** | [#69](https://github.com/gke-demos/kode-gopher/issues/69) | Shipped ([#76](https://github.com/gke-demos/kode-gopher/pull/76), `v0.5.0`) | Compact `renderText` JSON + prompt guidance on stripping `managedFields` / returning summaries | **Small** | Cuts 20–35% text token overhead and prevents 256 KiB result drops on `client-go` lists. |
| **P3** | [#71](https://github.com/gke-demos/kode-gopher/issues/71) | Shipped ([#78](https://github.com/gke-demos/kode-gopher/pull/78), `v0.5.0`) | Multimodal `ImageContent` attachments from `/app/.kode-gopher/attachments/` | **Medium** | Enables chart rendering from Monitoring/BigQuery and media inspection from GCS. |

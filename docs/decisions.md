# decisions

Append-only log of decisions made during implementation that weren't fully nailed down in [`design.md`](./design.md) or [`plan.md`](./plan.md). Each entry: what was decided, why, where it lives, and what to revisit later.

---

## Slice 0 — 2026-05-21

### Module path: initially `github.com/garisingh/kode-gopher`, renamed to `github.com/gke-demos/kode-gopher` when the repo was published
Started with the user's git-config identity as a placeholder owner because the project wasn't a git repo yet. Renamed to `github.com/gke-demos/kode-gopher` in slice 1.5 when we created the public repo — see the slice-1.5 entries below.

### CLI framework: stdlib `flag`, not `cobra`
The design doc anticipates `cobra` for the eventual `serve | exec | auth` subcommand tree. In slice 0 there's only one entrypoint, so adding `cobra` would be premature. The `flag` package handles `kode-gopher exec <file>` cleanly enough. **Revisit in slice 2** when `serve` arrives — that's the right moment to introduce `cobra` and split `main.go` into per-command files.

### Sandbox lifecycle: one-shot per CLI invocation
The slice-0 CLI opens a fresh sandbox on every `kode-gopher exec`, runs the program, and `Close`s it on exit. Two escape hatches for iterative dev work:
- `--keep` → `Disconnect` instead of `Close`, leaving the pod alive and printing its claim name.
- `--claim=<name>` → reattach to an existing claim instead of creating a new one.

This is intentionally simple. Long-lived session management is the executor's job from slice 1 onward; the CLI's job is to be a smoke test.

### go.mod synthesis: ship a minimal stub, let `go mod tidy` resolve
We materialize a placeholder `go.mod` (`module kode_gopher_slice0\ngo 1.26\n`) alongside `main.go` and prefix the command with `go mod tidy`. The toolchain inside the sandbox figures out which `cloud.google.com/go/*` (or other) packages the user's `import` block needs.

Trade-off: first invocation pays the full download cost (~10s for a fresh GCS client tree); subsequent invocations against the same Session reuse `$GOMODCACHE`. **Replaced in slice 3** by the prewarmed image, where the curated GCP packages are already in `$GOCACHE` / `$GOMODCACHE` and the synthesized `go.mod` declares them up front.

### Sandbox command shape: `go mod tidy && [ENV] go run .`
Env-var prefix applies only to `go run`, not `go mod tidy` (tidy doesn't need GCP creds). Build phase and run phase are still chained with `&&` here — they'll be split into separate `Execute` calls in slice 1 so we can tag errors by phase.

### Env forwarding: explicit allow-list, just two vars
`GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_QUOTA_PROJECT` are forwarded from the host into the sandbox command line if set on the host. Picked these two because the canonical demo (`testdata/list_buckets.go`) reads `GOOGLE_CLOUD_PROJECT`, and because billing-quota separation comes up in any non-trivial GCP workflow. Anything else (e.g. `CLOUDSDK_CORE_PROJECT`, `GOOGLE_CLOUD_REGION`) is **not** forwarded for slice 0. **Revisit in slice 4** as part of a real env-forwarding policy — likely "forward `GOOGLE_*` and `CLOUDSDK_*`, scrubbed of anything that looks like a secret."

### Missing local ADC: warn and proceed, don't hard-fail
If `~/.config/gcloud/application_default_credentials.json` doesn't exist, the CLI logs a warning and runs the program without setting `GOOGLE_APPLICATION_CREDENTIALS`. Rationale: even with no ADC, the sandbox might have its own creds (e.g. if a future kind-on-GKE setup binds Workload Identity), and the program might not need GCP at all. Failing fast in the CLI would be an over-correction for slice 0.

The plan's "fail fast if 3LO refresh fails" requirement applies once we have the `creds.CredentialSource` abstraction (slice 4) — there, an explicitly-configured `forwarded` mode with a missing/expired token should fail loudly. In slice 0 there's no abstraction yet, just an opportunistic copy.

### Shell quoting: single-quote with `'\''` escape
Tiny helper, no dependency. Adequate for any env-var value that doesn't contain control characters. Documented inline in `cmd/kode-gopher/main.go`.

### ADC path inside sandbox: `/app/.kode-gopher/creds/adc.json`
Matches the design doc. The `.kode-gopher/` prefix namespaces all of our scratch state (creds today; `result.json`, `bin/run` later) so it doesn't collide with user-generated files in `/app`.

### Test program lives under `testdata/`
The Go toolchain skips `testdata/` directories by convention, so the program in there isn't part of our module's build graph — which it can't be anyway, since it imports `cloud.google.com/go/storage` and we don't carry that dep. Slice-0 test programs are *payloads we ship into the sandbox*, not part of the kode-gopher binary.

### Exit-code propagation
`kode-gopher exec` exits with the user program's exit code on success of the sandbox round-trip. Errors *opening* or *executing* the sandbox itself (kubeconfig missing, template not found, network timeout) print to stderr and exit 1 via `log.Fatalf`. This separation lets CI distinguish "your code crashed" from "infrastructure broke."

### Found: hard ~60s cap on a single `Execute` call
End-to-end test on local kind surfaced a structural limit: the upstream agent-sandbox HTTP layer times out a `/execute` POST after ~60s of `awaiting response headers`. The in-pod server runs each command synchronously and returns when it finishes, so any single command that takes >60s wallclock fails with "retries exhausted". This isn't a knob we can turn from `pkg/goruntime`.

**Workaround in slice 0**: split the work into ≤60s phases. `cmd/kode-gopher/main.go` now runs three separate `Execute` calls — `go mod tidy`, then `go build -o .kode-gopher/bin/run .`, then the binary. State persists across calls in `/app`, so files only ship once. This previews Slice 1's Build/Run/Fetch shape.

**Workaround in test program**: switched `testdata/list_buckets.go` from `cloud.google.com/go/storage` (gRPC) to `google.golang.org/api/storage/v1` (REST). Cold `go build` of the gRPC client tree exceeded 60s on its own even after splitting; the REST client fit in ~25s. Documented inline in the test program.

**Real fix (slice 3)**: prewarm `$GOCACHE` and `$GOMODCACHE` in our sandbox image for the curated GCP packages. Two implications this run promoted to higher priority:
- The "curated set" is now load-bearing for correctness, not just performance. Anything outside the prewarmed set hits the 60s cap.
- A `$GOCACHE` PVC mounted at the cache path (cross-pod persistence) becomes a stronger nice-to-have — it would degrade-gracefully for non-curated imports too. Was a deferred decision; moved up.

### Kind smoketest delegates to upstream where possible
`scripts/smoketest-kind.sh` does the full slice-0 round-trip: kind cluster, agent-sandbox controller + extensions, sandbox-router (built from `kubernetes-sigs/agent-sandbox` git context), GHCR-pulled `ghcr.io/gke-demos/go-runtime-sandbox:latest`, SandboxTemplate via `kubectl apply -k <upstream-git-url>`, then `kode-gopher exec`. Re-runs are idempotent; `--clean` deletes the cluster, `--compare` diffs the output against `gcloud storage buckets list`.

We deliberately don't fork/embed the upstream Dockerfile, manifests, or sandbox-router source — `docker build` from git context, `kubectl apply -k` from git URL, and pre-built GHCR image cover all three. Drift risk is minimal because we pin the agent-sandbox version via `$AS_VERSION` (default `v0.4.6`) and the SandboxTemplate via `?ref=main` (deliberately tracking head — revisit if it breaks).

### Smoketest output extraction: awk between `── stdout ──` / `── stderr ──`
For `--compare` mode, the user program's stdout is sandwiched between `format.Result`'s headers. A tiny awk extractor pulls it out and pipes through `jq` before `diff -u` against `gcloud`. Fragile if `format.Result`'s header style changes — but it's defined in [upstream `pkg/format/format.go`](https://github.com/gke-demos/go-runtime-sandbox/blob/main/pkg/format/format.go) and unlikely to drift.

### Smoketest comparison: chronologically-sorted name list, not full record diff
First attempt at `--compare` diffed full `{name, timeCreated}` records. Two real format gaps got in the way: `gcloud storage buckets list --format=json` emits `creation_time` (not `timeCreated`) and uses `2024-10-23T09:55:39+0000` precision (no millis, `+0000` offset) while the GCS REST API emits `2024-10-23T09:55:39.709Z`. Rather than write a normalizer for two different timestamp formats, the smoketest now extracts only the name list, sorted by each source's native timestamp field, and diffs that. This is the actual data-fidelity check we care about ("same buckets, same chronological order"); the timestamp formats are equivalent semantically.

### Empty bucket list: `out := []bucket{}` not `var out []bucket`
A nil slice encodes as `null` via `json.Encoder.Encode`, which would make the smoketest's diff spuriously fail in projects with zero buckets. Init as empty slice so it always encodes as `[]`. Trivial but worth recording — same trap will hit any future test program.

---

## Slice 0.5 — 2026-05-21

Pulled forward from Slice 3 because the Slice 0 smoketest proved that without GCP-SDK prewarming, any non-trivial GCP program exceeds the upstream agent-sandbox HTTP layer's ~60s per-call cap (see Slice 0 entry on the 60s cap finding). Slice 0.5 builds just enough infra to unblock Slice 1+ — not the full Slice 3 deployment.

### Curated set: 5 packages to start
`internal/curated/packages.go` lists `storage`, `compute/apiv1`, `container/apiv1`, `bigquery`, `secretmanager/apiv1`, plus `google.golang.org/api/option`. Picked for breadth-of-coverage on common workflows (data, compute, GKE, secrets) without bloating the image. Easy to extend — and the `curated.Packages` file is the single source of truth that prompt generation and `lookup_package_docs` (slice 4) will also read from.

### Prewarm uses blank imports, not constructor calls
`internal/prewarm/main.go` does `_ "cloud.google.com/go/storage"` for each curated package and has an empty `main()`. Blank imports are sufficient: the Go compiler fully compiles + caches imported packages regardless of whether their symbols are referenced. Avoids needing to know each package's correct constructor signature, which differs across the GCP SDK (`storage.NewClient(ctx)` vs `compute.NewInstancesRESTClient(ctx)` vs `container.NewClusterManagerClient(ctx)` vs ...).

### Prewarm is a standalone Go module
`internal/prewarm/go.mod` exists separately from the parent `github.com/gke-demos/kode-gopher` module. Reason: we don't want the full GCP SDK in the kode-gopher binary's dep graph — kode-gopher is the host-side CLI / MCP server, it has no business linking the GCS gRPC client. The standalone module is built only at image-build time and ignored by `go build ./...` from the repo root.

**Keep-in-sync warning**: the prewarm's imports must mirror `internal/curated/packages.go`. Five packages is small enough that manual sync is fine; if the list grows past ~15, generate `prewarm/main.go` from `curated.Packages` via `go generate`.

### Sandbox image extends upstream, no multi-stage cross-compile
`sandbox/Dockerfile` is six layers: `FROM ghcr.io/gke-demos/go-runtime-sandbox:latest`, `COPY --chown=1000:1000 internal/prewarm /tmp/prewarm`, `USER 1000`, `RUN go mod tidy && go build`, `USER root`, `RUN rm -rf /tmp/prewarm`. Runs the prewarm `go build` as USER 1000 so caches land at `/home/sandbox/.cache/go-build` and `/home/sandbox/go/pkg/mod` — the same paths the runtime sandbox (also USER 1000) reads from at exec time.

Single-arch (linux/amd64). The upstream does a more elaborate `BUILDPLATFORM`-pinned cross-compile so cache entries are valid for both amd64 and arm64; we punt that to Slice 3 when Artifact Registry + multi-arch matters.

### Smoketest builds + loads our image; manifests/base is now our manifest
Updated `scripts/smoketest-kind.sh`: dropped the `docker pull ghcr.io/.../go-runtime-sandbox:latest` + `kind load` + `kubectl apply -k <upstream-git-url>` sequence. Replaced with `docker build -t kode-gopher-sandbox:latest -f sandbox/Dockerfile .`, `kind load kode-gopher-sandbox:latest`, `kubectl apply -k manifests/base`. Our SandboxTemplate keeps the upstream name `go-runtime-template` so the kode-gopher binary's hardcoded reference still works.

Also added `kubectl delete pods -l sandbox --ignore-not-found --wait=false` after template apply to evict any stale pods from a previous image — `imagePullPolicy: IfNotPresent` + an unchanged tag means kubelet would otherwise hold onto the old image's container.

### Numbers — gRPC `cloud.google.com/go/storage`

| phase | slice 0 (REST client, no prewarm) | slice 0.5 first run | slice 0.5 steady state |
|---|---|---|---|
| tidy | 11.4 s | 4.8 s | 1.8 s |
| build | 25.3 s | 23.7 s | 7.2 s |
| run | 0.76 s | 1.2 s | 1.1 s |

Tidy improvement is unambiguous (`$GOMODCACHE` hit, no module downloads). Build improvement looks modest on the first run but jumps on the second; very likely host-kernel page cache: the first pod-after-image-change reads `$GOCACHE` files from disk, subsequent pods read them from page cache. Self-correcting after one warm-up run on a given host.

Net: gRPC GCS client compiles + runs in <10 s end-to-end at steady state, well under the 60 s cap. Headroom for programs that import 3-4 curated packages at once — which the previous "lean REST workaround" wouldn't have given us.

### What slice 0.5 deliberately doesn't do
- No multi-arch image (single linux/amd64).
- No Artifact Registry push (`kind load` only).
- No GKE manifests, Workload Identity binding, NetworkPolicy.
- No SandboxWarmPool.
- No prompt-system.md generation from `curated.Packages` (slice 4).
- No `lookup_package_docs` allow-list enforcement (slice 4).

All of the above remain Slice 3/4 work. Slice 0.5 is just "make the substrate usable for Slice 1+".

---

### No LICENSE, no README, no .gitignore in slice 0
- LICENSE: defer to the project owner. Upstream `gke-demos/go-runtime-sandbox` uses Apache 2.0; we'll probably want to match.
- README: design.md + plan.md + decisions.md cover orientation. A user-facing README waits for slice 2 when there's a binary worth telling someone how to run.
- .gitignore: project isn't a git repo yet. When it becomes one, `kode-gopher` (the stray binary `go build ./...` drops in the project root) and `vendor/` are the things to ignore.

---

## Slice 1 — 2026-05-21

### Snippet convention: any non-`main` package triggers wrapping
The normalizer parses with `go/parser` and branches on `f.Name.Name`. If it's `main`, the file passes through verbatim (`ModeVerbatim`). Anything else → `ModeWrapped`: rewrite the package decl to `main`, ship the wrapper alongside. Convention used in `testdata/list_buckets_snippet.go` and `testdata/panic_snippet.go` is `package kode_gopher_snippet`. The name itself is irrelevant; only "isn't main" matters.

**Why this rather than a special header comment or file extension**: snippets stay valid Go that gofmt/govet/IDE tooling understands, while still being unmistakably distinguishable from a "real" `package main` program. No new convention to remember.

### Wrapper as a separate file, not AST-merged into the user's source
The plan called for a single normalized `main.go`. Switched to a two-file output (`main.go` + `kg_wrapper.go`, both `package main`) because:
- Merging the wrapper's helper imports (`context`, `encoding/json`, `os`, `path/filepath`, `runtime/debug`) into the user's import list via `go/ast` invites collisions if the user already imports the same path under a different alias.
- A separate file gets its own import block; the Go compiler is happy as long as both files declare `package main` and don't define duplicate symbols.
- The wrapper file is plain Go — gofmt/govet on it locally just works, and reading it doesn't require mental template-substitution.

The wrapper source is `internal/wrapper/wrapper.go.tmpl` (the `.tmpl` suffix is purely so the host toolchain ignores it when building this module — its contents are valid Go). `internal/wrapper/wrapper.go` brings it in via `//go:embed wrapper.go.tmpl`. Materialized into the sandbox as `kg_wrapper.go` — distinctive prefix so users can't easily collide.

### Anonymous-function-with-recover inside `main()`
The wrapper's `func main()` calls the user's `run` inside an inline `func() (r map[string]any) { defer ... recover ... }()`. Avoids exporting any helper symbols (e.g. `kgInvoke`) that could collide with user-defined identifiers in the same `package main`. Slightly nested but idiomatic Go for one-time deferred recover.

### Result schema with `kind` discriminator
The wrapper writes `/app/.kode-gopher/result.json` with one of four shapes:
- `{"kind": "ok", "value": <user-returned value as raw JSON>}`
- `{"kind": "error", "message": <err.Error()>}` — when `run` returns a non-nil error
- `{"kind": "panic", "message": <fmt.Sprintf("%v", recovered)>, "stack": <runtime/debug.Stack()>}`
- `{"kind": "marshal_error", "type": <"%T" of value>, "message": <json error>}` — when the returned value isn't json-serializable

`Result.Value` uses `json.RawMessage` so the user's data is preserved byte-for-byte (no encode/decode round-trip).

### Pre-written `result.json` respected only in verbatim mode
The wrapper does `os.Stat(resultPath)` early and returns immediately if the file already exists. Practical effect:
- **Wrapped mode**: wrapper always wins (the snippet doesn't write the file; the wrapper materializes it).
- **Verbatim mode**: if the user's `package main` writes `/app/.kode-gopher/result.json` themselves, that's the result. If they don't, no Result is set (executor's fetch step finds an empty file and the Outcome's Result is nil). The verbatim path doesn't ship the wrapper anyway — the guard is defense-in-depth for the case where a user copies a `package main` program and *also* expects the wrapper to fill in.

### Executor phases: just Build and Run (no Fetch)
`Phase` enum has only `PhaseBuild` and `PhaseRun`. The plan mentioned Build/Run/Fetch but Fetch isn't a user-facing failure mode: if `cat result.json` returns empty (no file) or the bytes don't parse as JSON, the Outcome's `Result` is just nil. The caller distinguishes "no result was produced" from "result was {kind: ok, value: ...}" by the pointer. Spilling Fetch into the enum would force every caller to handle a phase that's never actionable on its own.

### `extra_imports[]` deferred to slice 2
The plan mentioned merging an `extra_imports[]` parameter into the synthesized go.mod. There's no shape for that parameter to flow through in slice 1 (CLI takes one positional path, nothing else). The MCP `execute_go_code` tool in slice 2 is where the parameter belongs. Adding plumbing for it now would be speculative.

### `go.mod` stays in the CLI, not in the normalizer
Normalize's output is `{main.go, [kg_wrapper.go]}` — source files only. The CLI synthesizes `module kode_gopher_user\n\ngo 1.26\n` separately and adds it to the file map before handing off to executor. Keeps normalize pure and unit-testable without dragging in module-resolution concerns. When slice 2 introduces extra_imports, that parameter still gets merged into the synthesized go.mod at the CLI/MCP layer, not in normalize.

### Smoketest result extraction uses `python3` + `json.JSONDecoder.raw_decode`
First attempt used `awk … | jq '.value'` to pull the result block out. Failed because `printOutcome` writes to stdout while the agent-sandbox client logs to stderr — both are tee'd through `2>&1 | tee`, and the client's "claim deleted" log line (`"ts"="..." "msg"=...`) lands AFTER the result block. `jq` choked on the trailing non-JSON.

Fix: pipe through `python3 -c 'json.JSONDecoder().raw_decode(...)'` which parses exactly the first JSON document and ignores trailing bytes. Added `python3` to the preflight check. Long-term cleaner fix would be redirecting stdout vs stderr separately in the smoketest so the extraction only ever sees printOutcome's output; punted for now — `raw_decode` is robust enough and adds no maintenance burden.

### Smoketest default: run both files
`./scripts/smoketest-kind.sh --compare` (no `--file`) now runs both `testdata/list_buckets.go` (verbatim) and `testdata/list_buckets_snippet.go` (wrapped) and asserts each matches gcloud's bucket order. `--file <path>` still overrides to a single file. Re-running both takes ~10s total at steady state.

### Panic path verified ad-hoc, not in default smoketest
`testdata/panic_snippet.go` exercises the `result.kind = "panic"` path; run with `./bin/kode-gopher exec testdata/panic_snippet.go`. Output confirms `phase=run, exit=0, result.kind=panic, result.message="kode-gopher demo panic"`, full stack trace from `runtime/debug.Stack()` showing the recovery site at `kg_wrapper.go:42`. Not in the default smoketest because it doesn't correspond to a gcloud diff — it'd need bespoke assertion logic. Worth adding to a future `go test` integration suite.

---

## Slice 1.5 — 2026-05-21 — publish the repo

### Module path renamed `garisingh` → `gke-demos`
`go mod edit -module=github.com/gke-demos/kode-gopher` + bulk sed across `.go` and `.md` files. 5 files touched. Build + tests + smoketest unaffected.

### Repo published at `https://github.com/gke-demos/kode-gopher`
Public, Apache 2.0. `gh repo create gke-demos/kode-gopher --public --source=. --remote=origin --push`. Single initial commit `4375b23` authored as `Gari Singh <garisingh@google.com>` (set as local git config in the repo so it doesn't depend on global config drift).

### No Claude/Anthropic attribution in commits or code
User-specified policy: no `Co-Authored-By` trailer on commits, no "generated by Claude" comments anywhere. Verified by `git grep` — only Claude references are (a) `.gitignore`'s `/.claude/` entry (ignoring the local-state dir), (b) "Claude Desktop" mentioned in `docs/plan.md` as a named MCP client target for slice 3 (product reference, not attribution), and (c) the wrapper's "Code generated by kode-gopher" (by our own tool).

### Apache 2.0 headers on every applicable source file
15 files: every `.go`, `.tmpl`, `.yaml`, `.sh` (after shebang), and `Dockerfile`. Block-comment form for Go (matches upstream `go-runtime-sandbox`); `#` line-comment form for everything else. Skipped: `go.mod`, `go.sum` (tool-managed; convention is no header), `.gitignore`, `*.md`, `LICENSE`, `NOTICE`. Headers added via `/tmp/add-headers.sh` (idempotent — checks for "Copyright 2026 Google LLC" in head before prepending).

Folded into the initial commit via `git commit --amend --no-edit` — we hadn't pushed yet, so amending was clean. **Going forward**, every new source file we add should get the same header at creation time; revisit by adding a CI lint in slice 4.

### Found (again): `kode-gopher exec` inherits kubectl current-context
Caught during slice 1.5 smoketest verification — first run's snippet test timed out at 3 minutes because the kubeconfig's `current-context` had drifted from `kind-kode-gopher-smoke` to a GKE Autopilot cluster between the verbatim and snippet invocations (visible in `kubectl get events` showing `gk3-ap-gke-sandbox-pool-3-*` nodes). The smoketest script switches to the kind context once at the start, but `kode-gopher exec` itself just inherits whatever context is current at the moment it runs. **Add `--context` flag in slice 2** so each `kode-gopher exec` (and the future `kode-gopher serve`) pins its target cluster explicitly. Mentioned this risk in conversation earlier in slice 0 ("same footgun as any kubectl-shaped tool") — now we've actually hit it. Re-running the smoketest with stable context succeeded both files.

### Repo polish files: minimal README, NOTICE, .gitignore
- `README.md`: one paragraph + Status + Try-it-locally + table of what's where. No marketing.
- `NOTICE`: standard Apache 2.0 NOTICE (`kode-gopher / Copyright 2026 Google LLC / This product includes software developed at Google.`) plus a note about the agent-sandbox + go-runtime-sandbox dependencies.
- `.gitignore`: `/bin/`, `/kode-gopher` (stray binary), `/.smoke/`, `/vendor/`, `/.claude/`, IDE files, OS noise, and explicit `/resume` (a local file the user keeps in the project dir).

### Smoketest script executable bit
The header-prepending script overwrote `scripts/smoketest-kind.sh` via `cat | mv`, which created the new file with default `644` instead of preserving `755`. Caught by next smoketest run failing with `Permission denied`. Fixed in working tree with `chmod +x` and in git index with `git update-index --chmod=+x scripts/smoketest-kind.sh`. **Note for future bulk-rewrites**: prefer in-place tools (`sed -i`) that preserve mode, or use `install -m 755` instead of `mv`.

---

## Slice 1.7 — 2026-05-21 — GKE Autopilot smoketest

Pulled forward from Slice 3 because the user had a GKE cluster ready and we wanted to validate the production-realistic substrate (Autopilot + gVisor + Artifact Registry / GHCR) before piling on Slice 2's MCP-server complexity. Each `kode-gopher exec` runs end-to-end in ~55s against the cluster; the loop verified passes (`docs/plan.md` slice 3's canonical-demo pass criterion).

### Image distribution: GHCR (`ghcr.io/gke-demos/kode-gopher-sandbox`)
User picked GHCR over Artifact Registry. Pushed 2.23 GB image; required `gh auth refresh -h github.com -s write:packages` to mint a token with the right scope, then `gh auth token | docker login ghcr.io`. **GHCR packages start private by default** — even though the upstream `ghcr.io/gke-demos/go-runtime-sandbox` is public, new packages under the same org default private and need a manual visibility flip via the web UI (https://github.com/orgs/gke-demos/packages/container/kode-gopher-sandbox/settings → Change visibility → Public). No GitHub REST API for this; UI-only.

### CLI: subcommand-aware FlagSet, `--namespace` flag
Stdlib `flag.Parse()` stops parsing flags at the first positional argument, so `kode-gopher exec --namespace=codemode <file>` treated `--namespace=...` as positional and the validator rejected it. Restructured `cmd/kode-gopher/main.go` to dispatch on `os.Args[1]` and parse the rest with a per-subcommand `flag.NewFlagSet`. Also closes the slice-1.5 "kubectl context drift" item: the smoketest now `kubectl config use-context ap-gke-sandbox` upfront, and within a single `kode-gopher exec` invocation the kubeconfig is read once. Full `--context` flag still deferred — would require constructing the agent-sandbox client with explicit kubeconfig loading.

### GKE overlay: `manifests/overlays/gke/`
Three resources composed via kustomize:
- **SandboxTemplate** (patched from `manifests/base/`): strips `/spec/service` (GKE-bundled CRD rejects it), sets `/spec/networkPolicyManagement: Managed` (required by the Autopilot addon), swaps image to GHCR with `imagePullPolicy: Always`, adds `runtimeClassName: gvisor` + matching `nodeSelector` + `toleration` for the gVisor node pool, and adds pod + container `securityContext` (non-root uid 1000, drop ALL, `seccompProfile: RuntimeDefault`).
- **SandboxWarmPool** (replicas=2): **load-bearing on Autopilot**. Without one, SandboxClaims sit at `Ready=False` indefinitely — the addon's controller appears to require a warmpool to back claims rather than spinning up sandboxes on demand. (Manual confirmation: a claim against the existing upstream template in `go-runtime-sandbox-mcp-poc` — which has a warmpool — resolves in 5 s; a claim against our template without a warmpool sat 6+ minutes with no pod, no events.)
- **sandbox-router** (Deployment + Service): the goruntime client looks for a Service named `sandbox-router-svc` *in the SandboxClaim's own namespace*, not cluster-wide. So every namespace where we run claims needs its own router. Uses the AR image already present in the project (`us-central1-docker.pkg.dev/gke-demos-345619/agent-repo/sandbox-router:v0.4.6`) — same project, same default node SA, pulls without setup. 2 replicas with zonal topology spread.

All three are inside the overlay, applied in one `kubectl apply -k`.

### Smoketest design: pares down to the deltas
`scripts/smoketest-gke.sh` is ~110 LOC vs the kind script's ~170. Skipped because the cluster already has them: cluster provisioning, agent-sandbox controller install, sandbox-router image build. Added: warmpool readiness wait (up to 8 min on cold), router rollout-status wait. Reuses the kind script's `extract_data` helper for output parsing (Python `JSONDecoder.raw_decode` to ignore trailing log noise).

### Timings — Autopilot vs kind
| Phase | kind (slice 0.5 steady-state) | GKE Autopilot (slice 1.7 steady-state) |
|---|---|---|
| warmpool initial fill | n/a | ~130 s (one-time per cluster) |
| `kode-gopher exec` (verbatim) | ~5 s | ~55 s |
| `kode-gopher exec` (wrapped) | ~5 s | ~55 s |

GKE is ~10× slower per exec at steady state. Suspect: gVisor syscall-interception overhead + Autopilot's per-pod resource accounting + warmpool-pod cold filesystem state. Worth investigating in slice 3/4 (PVC for `$GOCACHE`, larger pod resources, warmpool replenishment tuning). For slice 1.7's purpose — proving the loop runs on production-realistic infra — 55 s is fine.

### Found: GKE-bundled SandboxClaim schema is stricter than upstream's
Upstream `pkg/goruntime` (via `sigs.k8s.io/agent-sandbox@v0.4.6` client) appears to create SandboxClaims with `spec.template.name` — works on kind, but the GKE-bundled CRD declares only `spec.sandboxTemplateRef.name` (verified via `kubectl explain sandboxclaim.spec --recursive`). Our calls succeed because the goruntime client is up-to-date with the newer schema. Worth noting as a long-term coupling: if we ever pin to an older `agent-sandbox` version that emits the old shape, GKE Autopilot will silently reject the claim spec and the controller will sit idle.

### Decision deferred (again): `--context` flag and `--kubeconfig`
The smoketest pins context via `kubectl config use-context` before invoking the binary. A proper `--context` flag on `kode-gopher exec` requires constructing the agent-sandbox `*sandbox.Client` explicitly (rather than letting it inherit ambient kubeconfig) and threading it into `goruntime.Options.Client`. Defer to Slice 2 along with `cobra` introduction.

---

## Slice 2 — 2026-05-22 — MCP server

The MCP server is live. `kode-gopher serve` runs over stdio, advertises one tool (`execute_go_code`), and holds a long-lived `*goruntime.Session` for the lifetime of the process — exactly the design's "one server, one session, lazy-open, mutex-serialized" shape. Verified end-to-end against both kind (`scripts/smoketest-mcp.sh --target=kind --compare`) and the real GKE Autopilot cluster (`--target=gke --compare`); both diff cleanly against `gcloud storage buckets list`.

### MCP SDK: `modelcontextprotocol/go-sdk`
Already a transitive dep via `gke-demos/go-runtime-sandbox` (their `cmd/mcp-server` uses it). Promoted to a direct dep on `go mod tidy`. v1.6.0 currently. Same SDK that the user-side MCP clients (Claude Desktop, etc.) speak.

### `CredentialHook` callback rather than baking gcloud paths into `internal/mcp`
`internal/mcp` exposes a `Config.Credentials CredentialHook` that returns `(files, env)` to fold into every tool call. `cmd/kode-gopher/serve.go` wires in the same `readLocalADC` + `collectForwardedEnv` logic the `exec` subcommand uses. Keeps `internal/mcp` as a generic "compile + run Go in a goruntime sandbox" service with no host-OS coupling — when the slice-4 `internal/creds` package lands with the full `CredentialSource` interface, the hook signature stays.

### `/app` is Reset between tool calls; `$GOCACHE` survives
Mirrors upstream `cmd/mcp-server`'s ephemeral default. One snippet can't leak files into the next, but the per-package compile cache persists, so back-to-back calls against the same imports are near-instant. (Wrapped call in our smoketest landed at ~6s on kind, ~26s on GKE — vs the verbatim cold call's 15s / 40s.)

### Wire-form `Result` decouples from the executor's `json.RawMessage`
First MCP smoketest run failed with `validating /properties/result/properties/value/items: type "object", want "integer"`. Cause: the SDK auto-generates a JSON schema from the handler's Output type; `executor.Result.Value` is `json.RawMessage` which is `[]byte` which the schema generator describes as "array of integer". When the actual value (a bucket array) goes over the wire, it doesn't match.

Fix: introduce `mcp.Result{Value any, ...}` as the wire form and `toWireResult(*executor.Result) *Result` that decodes the bytes via `json.Unmarshal(raw, &v)`. The executor keeps its raw-bytes-preserving shape (correct for "read user's exact bytes from sandbox"); the MCP layer presents a typed-any view (correct for "let the SDK schema-validate and let clients use it"). The handoff is one line of code and reads cleanly.

### Tool semantics
- `execute_go_code(code, extra_imports?)` → `{phase, mode, exit_code, duration_ms, stdout?, stderr?, result?}` as structured content, plus a human-readable text rendering for clients that only do text.
- `IsError` is true iff `exit_code != 0` OR `result.kind != "ok"`. So a compile failure, a non-zero exit, an `error` return from `run()`, a panic, and a marshal failure all surface as tool errors to the LLM. An OK run with stdout but no structured result (verbatim mode) is *not* IsError.
- Text body matches the CLI's `printOutcome` shape (the `── stdout ──` / `── stderr ──` / `── result ──` block markers) so existing extractors in our bash smoketests still work.

### `--extra-imports` shipped as both an MCP tool arg and a CLI flag
The normalizer gets an `Options{ExtraImports []string}`. Non-empty list emits a generated `kg_extra_imports.go` companion file that blank-imports each path, forcing `go mod tidy` to resolve them into the synthesized `go.mod`. Useful when the model knows it'll need a prewarmed package but hasn't declared the import in source yet. CLI surface: `kode-gopher exec --extra-imports='cloud.google.com/go/bigquery,cloud.google.com/go/secretmanager/apiv1' file.go`.

### `cmd/mcp-smoketest` + `scripts/smoketest-mcp.sh`
The smoketest is a Go binary that spawns `./bin/kode-gopher serve` as a subprocess, speaks MCP over its stdio via the SDK's `CommandTransport`, exercises both `testdata/list_buckets.go` (verbatim) and `testdata/list_buckets_snippet.go` (wrapped), and (with `--compare`) asserts the wrapped-mode `result.value` matches `gcloud storage buckets list` chronologically. No LLM, no `mcp-inspector` dependency — pure programmatic validation of the wire format AND the executor.

The bash wrapper (`scripts/smoketest-mcp.sh --target={kind,gke}`) just builds the binaries, picks the right context + namespace, and invokes `mcp-smoketest`. Doesn't bootstrap a cluster — run `smoketest-kind.sh` or `smoketest-gke.sh` first.

### Timings (steady state, second call onward)
| substrate | first call (cold session open + run) | subsequent (warm session, wrapped) |
|---|---|---|
| kind     | ~15 s | ~6 s |
| GKE Autopilot | ~40 s | ~26 s |

GKE-warm slower than expected (vs kind-warm). Suspect: gVisor syscall-interception overhead + the `Reset` between calls forcing a re-tidy/re-build each time (the cache survives so it's incremental, but the linker re-runs). Worth investigating in slice 4 alongside the `$GOCACHE` PVC question.

### Still deferred to slice 2+: `cobra`, `--context` flag
Subcommand dispatch in `main.go` plus per-subcommand `flag.NewFlagSet` works cleanly enough that `cobra` would add weight without UX win. Revisit when `auth` / future subcommands land. The `--context` flag also still deferred — both kind and GKE smoketests pin context via `kubectl config use-context` before invoking, and the MCP server inherits that. A real `--context` requires constructing the agent-sandbox client with an explicit kubeconfig and threading it into `goruntime.Options.Client`. Slice 3 or 4.

---

## Slice 2.5 — 2026-05-22 — replace pkg/goruntime with internal/sandbox

Slice 2's wrap-up noted that we were tightly coupled to `gke-demos/go-runtime-sandbox` — a small upstream we extend, not a project we own. The risk was concrete: that team is one demo group's-worth of bus factor, and the constraints we kept blaming "upstream" for (60s cap, namespace-scoped router, schema drift) were mostly bleeding through from `sigs.k8s.io/agent-sandbox` *under* goruntime, not from goruntime itself.

This slice removes the `pkg/goruntime` layer. We now own the equivalent code at `internal/sandbox/`.

### Chose Option B (rewrite) over A (vendor) or C (replace agent-sandbox)
Three plausible scopes were on the table — vendor the upstream code verbatim into our tree, rewrite a thin client of our own on `sigs.k8s.io/agent-sandbox/clients/go/sandbox` directly, or go further and replace agent-sandbox itself. Picked B because:
- Cost is ~1-2 days (came in at ~half a day in practice — most of the heavy lifting was already done by the goruntime team and is recognizable in our wrapper).
- We get a cleanly-shaped API surfaced to *our* needs from the start: e.g. exposed `Options.SandboxReadyTimeout` (the field goruntime didn't surface but the GKE Autopilot smoketest in slice 1.7 needed) is now a first-class option.
- We don't take on the agent-sandbox project's surface area (C). The 60s cap and namespace-scoped router come from there — different fix, different slice if/when it matters.
- A would have given us the same ownership without the API cleanup. B is barely more work for materially better fit.

### What stayed the same
- `Session.Open / Execute / Reset / Disconnect / Close / ClaimName` — same names, same semantics. `internal/executor` and `internal/mcp` and `cmd/kode-gopher` migrated with a search-replace of the import + type names; no logic changed.
- The tar-packing heuristic — `needsTar` picks tar when any key has a `/`, otherwise individual writes. Same code path, same tradeoffs (the agent-sandbox client's `Write` doesn't accept path separators).
- Truncation defaults — 8 KiB head + 8 KiB tail, same as goruntime.
- The `.kg-upload.tar` staging filename for multi-file uploads (renamed from `.goruntime-upload.tar` for branding only; behavior identical).
- Per-Execute timeout default of 5 min (capped by the agent-sandbox HTTP layer at ~60s regardless).

### What's new
- `Options.SandboxReadyTimeout` is now caller-controllable. Slice 1.7 noted the default 180 s is sometimes tight on cold Autopilot nodes pulling a 2.2 GB image; we now have a knob.

### What we deliberately didn't pull through
- `Options.Client` (BYO agent-sandbox client). Not needed today; can add when a real caller wants kubeconfig-context override.
- The agent-sandbox `Files()` rich API (`Read`, `List`, `Exists`). Our executor uses `cat result.json` via `Run`; we don't need the typed file API. Add when slice 4's `lookup_package_docs` or multi-file work asks for it.
- Gateway-mode connection (`GatewayName` / `APIURL` Options). All current deployments use port-forward; gateway mode is a slice 5 / production-deployment story.

### Tests
Added `internal/sandbox/{tar_test,truncate_test}.go` — pure-function coverage for the helpers we ported, since they're the failure-prone parts (tar header ordering, path validation, head+tail truncation arithmetic). The Session itself isn't unit-testable without a real cluster; the kind + GKE smoketests are its coverage.

### What this didn't fix (and why that's fine)
- **60s HTTP cap on a single Execute call.** That's an agent-sandbox in-pod server constraint, not a goruntime constraint. Our slice 0.5 prewarm + slice 0's tidy/build/run split are still the load-bearing workarounds. Option C would touch this; we explicitly chose not to.
- **Namespace-scoped sandbox-router lookup.** Same — agent-sandbox client behavior. Slice 1.7's per-namespace router deployment is still the workaround. Could file an upstream issue, but not a kode-gopher fix.
- **Multi-file support on the tool surface.** Folded into slice 4 (the agent-sandbox layer already materializes multi-file; what's missing is the `internal/normalize` + MCP-tool-args plumbing).

### What this *does* set up
- Bug-fix autonomy. If we hit a goruntime-shaped issue (tar handling, retry, truncation), we change it in our tree.
- A clean place to add things slice 4 needs — like exposing `SandboxReadyTimeout` per-call (we already started), or wiring a `--context` flag through `Options` via a custom `*sb.Client`.
- A test boundary. Adding pure-function coverage was trivial because we own the package; for goruntime we'd have had to upstream or fork.
- Honest dep accounting. `go.mod` now says `sigs.k8s.io/agent-sandbox v0.4.6` directly (not as an indirect through goruntime). The relationship is visible.

End-to-end verification: kind direct CLI ✅, kind MCP ✅, GKE MCP ✅ (~6 s warm wrapped on kind, ~30 s warm wrapped on GKE — identical to slice 2 numbers; pure refactor with no perf change).

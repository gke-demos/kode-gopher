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

## Curated-set expansion — 2026-07-04 — k8s.io/client-go

Slice 0.5 shaped the curated set as "the GCP SDK packages the model will import." That framing was too narrow given the project's positioning as "awesome for GKE and Kubernetes." A snippet that lists GKE clusters via `container/apiv1` can't then reach into any of those clusters — there's no k8s client-go in the prewarm, so the snippet either pays a 60s+ cold compile (blowing the agent-sandbox HTTP cap) or fails outright.

### What got added to `internal/curated/packages.go`
Five entries, all under `k8s.io/...`:
- `k8s.io/client-go/kubernetes` — typed clientset (Pods, Deployments, Services, …). Pulls most of client-go transitively.
- `k8s.io/client-go/tools/clientcmd` — kubeconfig parsing (for snippets that receive one inline; no host-side kubeconfig plumbing today).
- `k8s.io/client-go/dynamic` — CRD-aware dynamic client. Useful for third-party operators the model can't have compiled-in typed clients for.
- `k8s.io/client-go/tools/watch` — watch helpers with retry-on-connection-reset semantics.
- `k8s.io/apimachinery/pkg/apis/meta/v1` — `ListOptions`, `GetOptions`, `ObjectMeta`, needed by essentially every call.

`k8s.io/client-go/rest` isn't listed explicitly because it comes along transitively — anything blank-importing `kubernetes` gets rest compiled and cached. The `list_k8s_version_snippet.go` smoketest imports it directly and compiles fine.

### Image-size impact
Measured on the local rebuild: **kode-gopher-sandbox went from 2.23 GB → 2.73 GB (+500 MB)**. Bigger than the "150-250 MB" estimate that got floated when scoping the change — the k8s.io module graph is heavier than typical GCP client trees (apimachinery, api, kube-openapi, structured-merge-diff, gnostic-models, cbor, cel-expr, all pulled in transitively). Uncompressed. Compressed layer for GHCR push is roughly half that.

Prewarm `go build ./...` took 261 s on the first fresh build (whole prewarm, not just the delta). Not directly comparable to the previous baseline since versions have drifted, but in the same order of magnitude.

Accepted as-is. The curated-set doc comment now says "Add sparingly; each entry adds image size + build time. The k8s.io/client-go tree in particular pulls a large transitive graph" — future curated-set additions from other ecosystems (e.g. `sigs.k8s.io/controller-runtime`) get evaluated against this new baseline.

### Follow-ups that fell out
1. **RBAC for typed calls.** Today the sandbox KSA has no bindings, so `ServerVersion()` works (unauthenticated `/version`) but `Pods("kube-system").List(...)` would 403. When a real use case appears, add a Role/RoleBinding in the manifests — per-namespace, not cluster-admin.
2. **In-cluster vs remote-cluster reachability doc.** Added a "Kubernetes API access" section in `docs/design.md` covering `rest.InClusterConfig()` for the sandbox's own cluster and the `container/apiv1 GetCluster → build rest.Config` pattern for other GKE clusters.
3. **The curated set is no longer "GCP packages"** — updated the `internal/curated/packages.go` package doc to say "GCP SDK and Kubernetes client packages". Slice 4's generated system prompt needs to reflect this too when it lands.
4. **Smoketest coverage.** Two snippets added, covering the two branches of the design.md "Kubernetes API access" section:
   - `testdata/list_k8s_version_snippet.go` — `rest.InClusterConfig()` → `Discovery().ServerVersion()`. Tests the sandbox's own cluster reachability. Unauthenticated `/version` endpoint, so no RBAC needed. Small footprint (just proves the wire works).
   - `testdata/list_gke_pods_snippet.go` — the compose demo. `container/apiv1.ListClusters` picks the first GKE cluster in `$GOOGLE_CLOUD_PROJECT`, then builds a `rest.Config` from its endpoint + `MasterAuth.ClusterCaCertificate` + a `google.DefaultTokenSource`-backed `oauth2.Transport`, then `CoreV1().Pods("kube-system").List(...)`. Two GCP-adjacent APIs, one snippet, one round-trip. This is the wedge insight rendered as a test. Distinguishes Forbidden (RBAC gap) from other errors via `apierrors.IsForbidden`.
   
   Both wired into `smoketest-kind.sh`, `smoketest-gke.sh`, and `cmd/mcp-smoketest` (the latter with a `gke-compose` assertion: cluster field populated, ≥1 pod returned, first pod has non-empty name+namespace). The gcloud compare stays gated to `list_buckets*` files. Env forwarding remained the existing 2-var allowlist — no test-only var pollution.

Not verified: end-to-end kind/GKE smoketest runs (needs `GOOGLE_CLOUD_PROJECT` + user's call on cluster). Local docker build succeeded, which is what proves the prewarm imports resolve and compile.

## Version-pinning and PerAttemptTimeout — 2026-07-04

The k8s.io curated-set expansion above shipped a working image, but the *first* run of `smoketest-kind.sh --compare` failed on `list_buckets.go` with `net/http: timeout awaiting response headers` inside the build phase — 65s to compile a snippet that historically took <10s. This entry documents diagnosis and fix.

### Root cause: MVS drift between prewarm and snippet
Adding `k8s.io/client-go` to `internal/prewarm` shifted Go's minimum-version-selected versions of shared transitive deps upward. The prewarm cached `google.golang.org/protobuf@v1.36.12-0.<pseudo>` (pulled up by a k8s.io requirement), but a GCP-only snippet's own `go mod tidy` on an empty go.mod resolved `v1.36.11` (what the storage client alone wants). Cache miss on protobuf → cascading cache miss on everything that transitively depends on it (grpc, otel, genproto, ...). Net: **full recompile from scratch on every snippet build**. Confirmed by timing `go build` inside a warm image: 57 s (empty snippet go.mod) vs 5 s (snippet go.mod pinned to prewarm versions).

The problem is fundamentally that the prewarm's `$GOCACHE` is a *version-selection artifact* — its usefulness depends on the snippet's tidy resolving the *same* versions. That coupling was invisible before the k8s expansion because with only cloud.google.com/go/* in prewarm, MVS was a fixed point.

### Fix 1: prewarm lockfile as source of truth for snippet versions
- **Committed `internal/prewarm/{go.mod,go.sum}`** to git (previously gitignored / regenerated on every image build). Now versions are frozen; developers see exactly what the sandbox will use.
- **`sandbox/Dockerfile`** drops `go mod tidy` (would drift from committed versions) and preserves `go.mod` + `go.sum` at `/opt/kode-gopher-base/` before deleting `/tmp/prewarm`.
- **`internal/executor/executor.go`** bootstraps the snippet's `/app/go.mod` + `/app/go.sum` from `/opt/kode-gopher-base/` at tidy time (with `sed` rewriting the module line to `kode_gopher_user`). Gated on `[ ! -f go.mod ]` so a slice-4 multi-file snippet that ships its own go.mod overrides the pinning — the agent is signaling "I know what versions I want" and taking on the recompile cost.
- **`cmd/kode-gopher/main.go`** and **`internal/mcp/execute_go_code.go`** stopped synthesizing `module kode_gopher_user\ngo 1.26\n` — the executor now owns that setup.

Result: snippet `go build` after pinning: **~5 s** (list_buckets_snippet wrapped mode). 12x faster; well under any reasonable HTTP cap.

### Fix 2: expose PerAttemptTimeout, default 3 min
The "~60s per-call HTTP cap" quoted in slice 0/0.5 decisions is `sigs.k8s.io/agent-sandbox/clients/go/sandbox.defaultPerAttemptTimeout = 60 * time.Second`. Overridable via `sb.Options.PerAttemptTimeout`. Our `internal/sandbox.Options` didn't surface it, so we were stuck at the default.

- Added `internal/sandbox.Options.PerAttemptTimeout`, plumbed to `sb.Options`.
- Default 3 minutes — comfortably above any real prewarmed build but short enough to fail-fast on a truly hung pod.
- Removed stale "~60s cap" language from `internal/sandbox`, `internal/executor`, and the MCP tool description.

Even with the version fix (fast common case), the wider cap protects against unusual conditions (cold Autopilot node, snippet importing an uncurated package that needs to actually resolve on first use).

### Smoketest results (kind, --compare)
- `list_buckets.go` (verbatim): pass, 54s. First cold pod; subsequent snippets in warmed pods much faster.
- `list_buckets_snippet.go` (wrapped): pass, **8.3s**. Steady-state number that validates the pinning worked.
- `list_k8s_version_snippet.go`: **FAIL** at runtime — `rest.InClusterConfig: open /var/run/secrets/kubernetes.io/serviceaccount/token: no such file or directory`. Root cause: `sigs.k8s.io/agent-sandbox/extensions/controllers/utils.go:28` explicitly sets `AutomountServiceAccountToken = false` as its "secure by default" policy. Our `manifests/base/sandboxtemplate.yaml` doesn't override this. My `docs/design.md > Kubernetes API access` claim that in-cluster reachability "Just Works" was wrong. Requires a manifest change (opt-in, per-template) — deferred to a follow-up because it's a security-relevant decision, not a mechanical fix.
- `list_gke_pods_snippet.go`: **FAIL** at runtime — `dial tcp 136.112.103.246:443: i/o timeout`. Snippet picked cluster `devops-bench-small`, whose control plane isn't reachable from a laptop kind (private endpoint or MasterAuthorizedNetworks restriction). Fix: filter for publicly-reachable clusters in the snippet — deferred.

Both k8s failures are downstream of a working compile path. Task 9's fix (pinning + timeout) is complete; the k8s runtime issues are separate follow-ups.

## k8s runtime fixes — 2026-07-04

Two fixes for the runtime failures uncovered by the smoketest above.

### 1. Enable KSA token mount in the SandboxTemplate
`manifests/base/sandboxtemplate.yaml` now explicitly sets `automountServiceAccountToken: true` on the podTemplate. The agent-sandbox extensions controller's secure-by-default policy (`extensions/controllers/utils.go:28`) sets it to false unless overridden. Without the override, `rest.InClusterConfig()` fails at `open /var/run/secrets/kubernetes.io/serviceaccount/token: no such file`.

**Security tradeoff acknowledged**: the sandbox pod can now read its KSA token and (if any RBAC is bound) call the API server it runs on. Defense-in-depth is still there: the KSA has no RBAC by default (no Role or ClusterRoleBinding), so a snippet that tries typed calls gets Forbidden. That layer is where we'd control blast radius in a real deployment — grant sparingly, per-namespace, never `cluster-admin`. For the smoketest, `Discovery().ServerVersion()` needs no RBAC.

### 2. Prefer the GKE DNS endpoint in the compose snippet
`testdata/list_gke_pods_snippet.go` now uses `c.ControlPlaneEndpointsConfig.DnsEndpointConfig.Endpoint` (`uid.<region>.gke.goog`) when available. This is always publicly reachable via Google's control-plane routing regardless of private-cluster or MasterAuthorizedNetworks configuration, and uses Google-managed TLS (system trust store, no per-cluster CA needed). Falls back to `c.Endpoint` + `MasterAuth.ClusterCaCertificate` if DNS endpoint is unset.

Before this change, the snippet was picking `devops-bench-small` (first alphabetical) whose IP endpoint isn't reachable from a laptop kind. Post-fix: same cluster picked, but via DNS endpoint `gke-4b65741b887e41a7b879d75923e4d629e98f-...us-central1-a.gke.goog` — 20 pods listed from kube-system in 10.6s. Result includes `endpoint_kind: "dns" | "ip"` for observability.

Updated `docs/design.md > Kubernetes API access` to correct both mistakes (in-cluster "Just Works" claim and IP-endpoint-first recipe).

### Smoketest results (kind, --compare, after both fixes)
- `list_buckets.go` verbatim: 27s ✅
- `list_buckets_snippet.go` wrapped: **8.2s** ✅
- `list_k8s_version_snippet.go`: 13.5s ✅ (kind=ok, server v1.35.0)
- `list_gke_pods_snippet.go`: 10.6s ✅ (kind=ok, cluster=devops-bench-small via DNS endpoint, 20 pods)

MCP smoketest (`smoketest-mcp.sh --target=kind --compare`) also passes end-to-end: all four `execute_go_code` calls return `structuredContent` with `result.kind=ok` and the snippet's data.

## Slice 4 — 2026-07-04 — production hardening

Landed as five commits. Motivating question: what turns "demo works if you're careful" into "works predictably for a real user"? The below closes visible UX and reliability gaps without adding a slice's worth of new surface area — most of it is filling in what the earlier slices deferred.

### Scope pruned before starting
Dropped `manifests/overlays/gke/networkpolicy.yaml` + `ipranges-refresh.yaml` from the plan. Rationale: agent-sandbox already exposes `SandboxTemplate.spec.networkPolicy` for declarative egress, and on GKE Autopilot's Dataplane V2 (Cilium) FQDN egress is native — writing our own IP-list refresh CronJob would reinvent what the CNI does better. Non-Cilium CNIs get CIDR-only rules; that's a doc concern, not a kode-gopher feature. Also retired the "http.Get must fail" pass criterion since we no longer own the enforcement layer.

### 4a — `--context` flag + typed `ErrSessionDead` + retry-once
`internal/sandbox.Options.KubeContext` builds a `*rest.Config` via `clientcmd.NewNonInteractiveDeferredLoadingClientConfig` with an explicit `CurrentContext` override, passed as `sb.Options.RestConfig`. `--context` on both `exec` and `serve`. Closes the slice-1.5 / slice-1.7 context-drift footgun.

Session self-heal introduces `internal/sandbox.ErrSessionDead` sentinel + `classifySessionErr` string-corpus classifier. Classification lives in ONE place (with unit tests against a hand-curated corpus of realistic upstream errors — drift breaks tests instead of silently disabling retry). At the MCP layer, `handleExecuteGoCode` wraps Reset+Run in a `runOnce` closure; on `errors.Is(err, sandbox.ErrSessionDead)` + non-cancelled context, closes the session, opens a fresh one, retries once. On retry failure returns the ORIGINAL error so diagnostics point at the real cause. `execMu` held across both attempts so a concurrent tool call can't steal the fresh session.

### 4b — `internal/creds` unified `Source` interface
Replaces `CredentialHook func() (files, env)` with a `Source` interface that answers two questions instead of one: `Materialize(ctx)` for the executor and `Identity(ctx)` for `gcp_auth_status`. `Forwarded` implementation reads ADC JSON, dispatches on the `type` field: `service_account` returns `client_email` directly; `authorized_user` uses `google.CredentialsFromJSON` + hits `oauth2.googleapis.com/oauth2/v3/userinfo`. Identity cached via `sync.Once` (~200 ms first call, <1 ms subsequent). `Workload` is a stub until we run in-cluster. Both `serve` and `exec` refactored — behavioral no-op for Materialize.

### 4c — `gcp_auth_status` + `lookup_package_docs` tools; `auth status` subcommand
`gcp_auth_status` is a zero-arg MCP tool that reads `s.cfg.Credentials.Identity(ctx)` and returns Mode/CredType/Email/ProjectID. Returns `IsError` with partial data when identity lookup fails.

`lookup_package_docs` validates its `package` arg against `internal/curated.Packages` and its `symbol` arg against a Go-identifier regex (blocks shell metacharacters — the value flows into `sh -c`). Grabs `execMu` to serialize with `execute_go_code`. Command: `cd /opt/kode-gopher-base && go doc <pkg> [symbol]`. The `cd` is critical — `go doc` in module mode requires a `go.mod` in CWD that requires the target module; `/app` is empty post-Reset, but `/opt/kode-gopher-base/go.mod` is the prewarm's tidied lockfile with every curated package as a require. Subsecond at steady state (module cache pre-populated, no build, no network).

`kode-gopher auth status` mirrors `gcp_auth_status` on the CLI — constructs the same `creds.NewForwarded` `serve` uses and prints identity in human-readable form.

### 4d — Multi-file snippet support
`internal/normalize.Normalize` signature changed from `(src []byte, opts) → (*Result, error)` to `(files map[string][]byte, opts) → (*Result, error)`. Root files (no `/` in key) are the entry-point package; subdirectory files pass through as separate helper packages. Root files must share a package name (Go's same-directory rule) — caught with a legible error instead of a "found packages main and X" from `go build`. Verbatim mode requires exactly one root file with `func main`; wrapped mode requires exactly one root file with `func run(ctx context.Context) (any, error)` and rewrites ALL root files' package decls to `main` together (rewriting only the run-file would produce the same "found packages" error).

MCP tool arg additive: `ExecuteGoCodeArgs.Files map[string]string` alongside `Code string`. Exactly one must be set. If `files` contains a `go.mod`, the executor's `[ ! -f go.mod ]` gate lets it pass through unchanged — the caller inherits their own version pins (and any recompile cost). Documented in the tool description.

CLI unchanged: `kode-gopher exec <file.go>` stays single-file, wraps in `{"main.go": src}` at the call site. Multi-file lives on the MCP tool surface only for this slice.

Multi-file test snippet at `testdata/multi_file_helper_snippet/` — root `snippet` package imports helper subpackage `kode_gopher_user/formatter`. mcp-smoketest asserts result.kind=ok, buckets > 0, and every bucket's description carries the formatter's "d old)" suffix (proves the helper subpackage actually shipped).

### 4e — Generated system prompt + tool description
`internal/prompts/gen/main.go` reads `internal/curated.Packages` and emits both `internal/prompts/system.md` (LLM-facing) and `internal/prompts/description.go` (`const ExecuteGoCodeDescription`). `execute_go_code.go` aliases `prompts.ExecuteGoCodeDescription` instead of inlining. `//go:generate` directive on `internal/curated/packages.go`; `make prompts` is `go generate ./internal/curated` in disguise; `make prompts-check` catches drift for a future CI. Removes the two-file update risk when the curated set changes.

### Not shipped (deferred within slice-4 scope)
- **Automated smoketest for session self-heal.** Needs orchestrated mid-call `kubectl delete pod`. Manual verification for now; unit-test corpus covers the classifier.
- **Multi-file on the CLI** (`kode-gopher exec dir/`). MCP-only for slice 4.
- **Workload metadata-server lookup.** Stub until we run in-cluster.
- **Serving `system.md` as an MCP resource.** Generated file is enough for now — clients paste it into their system prompt.

### Verification numbers (kind smoketest --compare, after full slice)
- verbatim buckets: ~20-30s cold
- wrapped buckets: **8s** steady state
- k8s ServerVersion snippet: 10-15s
- GKE-compose snippet (list clusters → list pods via DNS endpoint): 10-15s
- multi-file snippet (root + formatter subpackage): ~10s
- `gcp_auth_status`: ~200ms first call, <1ms cached
- `lookup_package_docs` (storage.Client): subsecond

All six MCP tool paths return `result.kind=ok` / non-empty structured content.

## Slice 6 gating test — 2026-09-20 — yaegi vs. gRPC

`docs/plan.md` gated the whole yaegi slice on one question: does a "true gRPC-only" client work under the interpreter? Answer: **yes — ✅ branch**. But the gate's framing was wrong in two ways, and the corrections matter more than the verdict.

### Premise error 1 — `compute/apiv1` is not a gRPC client

The plan named `cloud.google.com/go/compute/apiv1` as the missing gRPC data point. It isn't one. `go doc` shows the package exposes **only** `New*RESTClient` constructors — the Compute API has no gRPC surface at all, so the GAPIC client is REST with protobuf message types. Testing it would never have answered the question the gate was asking.

The actual gRPC client in our curated set is `cloud.google.com/go/container/apiv1`, which offers both `NewClusterManagerClient` (gRPC, the default) and `NewClusterManagerRESTClient`. That's what the gate needed.

### Premise error 2 — the `unsafe` fear was misplaced

The PoC README worried that gRPC would break because "yaegi can't `unsafe`" and grpc-go uses it internally. That reasoning doesn't apply to this architecture. Yaegi interprets **only the snippet**; every symbol reached through a `yaegi extract` file is ordinary compiled code linked into the runner binary. `unsafe`, reflection, generics and assembly *inside a dependency* are irrelevant — they already compiled.

What would actually break is the inverse: interpreted code that must *satisfy* an interface consumed by reflection-heavy compiled code, or interpreted types that protobuf/grpc needs to reflect over. Our snippet shape (call the SDK, marshal the result) rarely does either. This reframes the compatibility risk for the rest of the slice: stop auditing dependencies for `unsafe`, start auditing for callbacks and user-defined types crossing back into compiled code.

### Results (live, project `gke-demos-345619`)

| test | client kind | yaegi total | vs. gcloud |
|---|---|---|---|
| `compute/apiv1` zones list | REST + protobuf | **410 ms** (setup 8, eval+run 402) | 130 zones, **identical**; gcloud itself took 1.56 s |
| `container/apiv1` cluster list | **true gRPC** | **446 ms** (setup 6, eval+run 440) | 10 clusters, name/location/version **identical** |

Interpreter setup stays in single-digit ms even with the large extracts loaded. Snippets at `testdata/list_zones_compute.go` and `testdata/list_clusters_container.go`.

### Cost signals for the slice (these are the real constraint, not compatibility)

| package | extract time | generated size |
|---|---|---|
| `compute/apiv1` | 52 s | 69 KB (605 lines) |
| `compute/apiv1/computepb` | 22 s | **2.3 MB** (18,671 lines) |
| `container/apiv1` | 48 s | 909 B |
| `container/apiv1/containerpb` | 18 s | 163 KB |
| `k8s.io/client-go/kubernetes` | 24 min, then **failed** (see below) | — |

Runner binary: **96 MB** with compute+storage+iterator symbols; 40 MB with container only. The plan's "<200 MB image" target is at risk once the full curated set is linked in — still a 10-20x win over the current 2.2 GB image, but the target number should be re-derived rather than assumed.

`k8s.io/client-go/kubernetes` extraction spent **24 minutes type-checking** and then failed — but not for the interesting reason. The error was a missing `go.sum` entry:

```
could not import k8s.io/api/apidiscovery/v2 (missing go.sum entry for module
providing package k8s.io/api/apidiscovery/v2)
```

Nothing in the PoC module imports client-go, so `go get k8s.io/client-go` never recorded its transitive sums; `yaegi extract` type-checks the real package graph and hit the gap. **A setup problem, not a verdict on client-go.** Retrying with the sums present.

What the run *does* establish regardless of outcome: client-go's type-check alone costs ~24 min of wall time (37 min user, 40 min sys — it's parallel and CPU-bound). Even on success that's a different order of magnitude from the 18-52 s GCP extracts, which shapes what `make extract` can reasonably do in CI.

### Symbol staleness stopped being hypothetical

Pulling `compute/apiv1` into the PoC module upgraded `google.golang.org/api` v0.280.0 → v0.287.1, which deleted `storage.ObjectsWatchAllCall`. The committed `google_golang_org-api-storage-v1.go` extract still referenced it, and the runner **failed to compile**:

```
./google_golang_org-api-storage-v1.go:188:79: undefined: storage.ObjectsWatchAllCall
```

Re-extracting (35 s) fixed it. Two things worth keeping from this:

- **The failure mode is build-time, not runtime.** A stale extract can't ship silently — the runner binary won't link. That's the safe direction, and it means `make extract` drift can be caught by simply building in CI, without needing a separate `prompts-check`-style comparison.
- **The blast radius of a dependency bump is every extract in the set**, not just the bumped package's. Plan open question 1 ("how aggressive on freshness?") now has a concrete answer: re-extract the whole set on any dependency upgrade, and let the build gate it.

### client-go extraction: the bottleneck is yaegi's importer, and it's a one-line fix — 2026-09-21

**`yaegi extract k8s.io/client-go/kubernetes` does not terminate.** Left running 12 hours: 10.9 GB RSS, only 16% average CPU (1h56m CPU over 12h elapsed), no output. The earlier "24 min" figure was the *failure* time of a run that died on a missing `go.sum` entry, not a completion time. There is no completion time.

**Root cause** — `yaegi/extract/extract.go:450`:

```go
pkg, err := importer.ForCompiler(token.NewFileSet(), "source", nil).Import(pkgIdent)
```

The **source importer** parses and type-checks the entire transitive dependency graph from source, in-process, on every invocation. It uses none of the Go build cache. For client-go's graph (60 direct imports, hundreds transitively, every group/version) that is effectively unbounded. This also kills the obvious optimisation: warming `GOCACHE` does nothing, so **every extract is a cold extract** — a client-go version bump would cost the same as the first run. There is no cheap update path today.

**The fix**: load types from compiled export data via `go/packages` instead. Benchmarked in an isolated module (`/tmp/benchimp`, `benchimporter/main.go` in the PoC):

| package | `go/packages` + export data | yaegi source importer |
|---|---|---|
| `k8s.io/client-go/kubernetes` | **2.0 s** | **>12 h, never completed** |
| `k8s.io/client-go/dynamic` | 1.0 s | not attempted |
| `cloud.google.com/go/container/apiv1` | 1.2 s | 48 s |
| `cloud.google.com/go/compute/apiv1` | 2.4 s | 52 s |

Compiling every one of those deps from scratch first (`go build ./...`) takes **41 s**, and is cached thereafter. So the whole cold path is ~43 s against >12 h, and the GCP packages get ~20-40x faster as a side effect. Second passes are flat (1.6-2.5 s), confirming the build cache does the heavy lifting once.

**Why this is small to adopt**: `genContent(importPath string, p *types.Package)` takes a plain `*types.Package`, so the swap is mechanical. `Extractor` exposes no importer hook and `genContent` is unexported, so it means copying `extract.go` (509 lines, yaegi is Apache-2.0 like this repo) to `internal/yaegi/extract/` and changing the one importer line. Worth upstreaming as an `Importer` field on `Extractor` afterwards.

**Consequence for the curated set**: client-go's exclusion was being argued on extraction cost. That argument is gone — 2 s.

### The forked extractor, built and verified end-to-end — 2026-09-21

`experiments/yaegi-poc/fastextract/` (copy of upstream `extract.go` + `loadpackage.go`) and `cmd/kg-extract/` (drop-in `yaegi extract` CLI).

**Equivalence.** All seven packages we had yaegi-generated baselines for re-extract **byte-identical**, including the 2.3 MB `computepb`: `container/apiv1`, `containerpb`, `compute/apiv1`, `computepb`, `cloud.google.com/go/storage`, `api/iterator`, `api/storage/v1`. Speed: `container/apiv1` 48 s → **1.3 s**, `compute/apiv1` 52 s → **3.1 s**.

**client-go extracts in 2.0 s** (vs >12 h, never completing). The full k8s set — `kubernetes`, `dynamic`, `rest`, `tools/clientcmd`, `apimachinery/.../meta/v1` — takes **~6 s** total.

**Second upstream bug, found only because we got past the first.** Generated output didn't compile:

```
k8s_io-client-go-kubernetes.go:12:2: v1alpha1 redeclared in this block
```

`genContent`'s `qualify` returns the bare `pkg.Name()` and the template emits unaliased imports. `k8s.io/client-go/kubernetes` imports 60 packages, a dozen of them named `v1`/`v1beta1`/`v1alpha1`. Nobody upstream ever saw this because the source importer never produced the file. Fixed by assigning aliases **only on collision** (sanitised full path), pre-assigned over direct imports in path order so output doesn't depend on symbol walk order. The seven baselines above are byte-identical *after* this change, which is the regression test.

**End-to-end.** `testdata/list_nodes_k8s.go` calls `clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})` — the typed clientset, interpreted:

| runner | pack | result |
|---|---|---|
| no k8s symbols linked | none | fails cleanly: `unable to find source related to: "k8s.io/apimachinery/pkg/apis/meta/v1"` |
| same binary | `k8ssyms.so` (90 MB) | **3 nodes, matches `kubectl get nodes` exactly, 61 ms total** (setup 48 ms, eval+run 13 ms) |

Verified against a live kind cluster (`kind-lookout-examples`, v1.36.1) since `ap-gke-sandbox` is gone.

So: client-go belongs in the yaegi curated set, the typed clientset works under the interpreter, and symbols ship as mountable packs. Plan open question 5 resolves to **include the full typed clientset**.

**Caveat on coverage.** The snippet names only `kubernetes`, `clientcmd` and `metav1` types. A snippet naming e.g. `corev1.Pod` needs `k8s.io/api/core/v1` extracted too — the curated k8s pack will need the common `k8s.io/api/*` group/versions, not just the clientset. Unmeasured, but they're small and fast now.

**Upstream.** Both fixes are worth PRs to yaegi: an `Importer` field on `Extractor`, and the import-alias fix (which is a plain bug, independent of the importer).

### Symbols can live outside the runner binary — verified

Question raised mid-gate: must every symbol set be linked into `kg-yaegi-runner` at build time, or can symbol sets be mounted in (OCI image volume, say) and updated on their own schedule? **They can be mounted.** Tested with Go plugins (`-buildmode=plugin`) in the PoC:

- `pluginload.go` reads `KG_SYMBOL_PLUGINS` (colon-separated dirs), `plugin.Open`s every `*.so`, looks up an exported `Symbols interp.Exports`, merges them, and hands the result to `i.Use()`.
- `plugins/containersyms/` and `plugins/computesyms/` are the same generated extract files with their build tags stripped, plus a three-line `var Symbols = interp.Exports{}`.

| build | size | result |
|---|---|---|
| runner, no SDK symbols linked | **34 MB** | control: container snippet fails with a clear "unable to find source related to cloud.google.com/go/container/apiv1" |
| `containersyms.so` | 41 MB | mounted → gRPC GKE call works, 10 clusters, 476 ms |
| `computesyms.so` (compute + computepb + iterator) | 83 MB | mounted → 130 zones, 416 ms |
| both mounted at once | 124 MB | **both snippets work**, 5 packages from 2 `.so`, setup 99-116 ms |

dlopen costs ~100 ms of interpreter setup vs ~6 ms fully-linked — irrelevant next to the 5-30 s the compiled path pays.

**What this buys.** The runner stops being a function of the curated set. Symbol packs become independently published artifacts, and the 24-minute `client-go` extraction becomes a deliberate, occasional, offline publish step rather than a cost on every runner build or CI run. That removes the main argument for excluding client-go from the yaegi set.

**Constraints, honestly.** Go plugins are strict about build compatibility, and this cuts both ways:

- Host and plugin must share identical versions of every package they *both* link, plus an identical toolchain. Here the runner links only stdlib + yaegi, and packs link the SDKs, so the shared surface is small: **bumping an SDK rebuilds one pack; bumping Go or yaegi rebuilds everything.** That asymmetry is what makes independent pack updates viable.
- Packs that share transitive deps (protobuf, grpc — GCP and k8s packs both will) must be built from **one common module graph**. The two-pack test passes because both came from the same `go.mod`. Packs are individually *regenerable*, not individually *pinnable*.
- `-buildmode=plugin` requires CGO, so the yaegi image needs a glibc base (`distroless/base`, not `static` or alpine/musl). Rules out the smallest possible image but stays far under 2.2 GB.
- **Untested:** dlopen under gVisor, and the mismatch failure mode (expected: loud error at `plugin.Open`, not silent corruption). Both need checking on a real sandbox before committing to this design.
- **Untested:** Kubernetes `image:` volume sources on our clusters — the `ap-gke-sandbox` cluster is gone (see below), so this wasn't verifiable. Fallback that needs no new API surface: an initContainer that copies `.so` files out of a symbol image into an `emptyDir`.

Non-plugin alternative if plugins prove unworkable under gVisor: ship **several runner binaries**, one per symbol profile, and select by image/template. No CGO, no dlopen, but combinatorial and coarse.

### Substrate drift during the idle period

`ap-gke-sandbox` — the GKE Autopilot cluster slices 1.7/3/4 were verified against — no longer exists. It's absent from `gcloud container clusters list` for `gke-demos-345619` and its kubeconfig context times out. The kubeconfig entry is stale. Slice 6's GKE smoketests will need a new cluster (and `scripts/smoketest-gke.sh` re-pointed) before anything can be verified on a real sandbox; the kind path is unaffected.

### Consequences for the plan

- Slice 6 proceeds on the full-scope ✅ branch for GCP packages, **as an opt-in second runtime — the compiled path stays the default and is not touched**.
- Replace the plan's `compute/apiv1` gating language with `container/apiv1`; keep compute in the curated yaegi set on its own merits.
- Add a curated-set decision for `k8s.io/client-go` before building the image.

## k8s.io/api types, and a yaegi named-result bug that returns wrong answers — 2026-09-24

### k8s.io/api extracts
`core/v1`, `apps/v1`, `batch/v1`, `networking/v1` and `apimachinery/pkg/api/resource` extract in 0.9–3.2 s each (`core/v1` is the largest at 78 KB). Added to `plugins/k8ssyms` and pinned in `symboldeps.go`. **The pack stays 90 MB** — client-go already linked the compiled `k8s.io/api` code; these files only add symbol tables. `testdata/k8s_api_types.go` names `corev1.Pod`, `appsv1.Deployment` and `resource.Quantity` directly and runs in 78 ms. Without the pack it fails cleanly on `import "k8s.io/api/apps/v1"`.

### Silent wrong results: named results persist across calls
That snippet's pod count and deployments matched `kubectl`, but it reported **9550m CPU / 4400Mi memory requests; the truth is 1250m / 590Mi**. 9550 is exactly the sum of the running totals of the per-pod values: the named results `(cpu, mem resource.Quantity)` of a helper called in a loop were never reset between calls.

Minimal repro, no SDK involved (`testdata/broken/named_result_int.go`):

```go
func f() (n int) { n++; return }
// called in a loop: go prints 1 1 1, yaegi prints 1 2 3
```

- Reproduces on stock yaegi v0.16.1 (the latest release, April 2024) and on `master` (Feb 2026). Not reported upstream as far as a search of the issue tracker shows.
- Straight-line calls are fine because each call site gets its own frame slot. Calls from the same site (loops) reuse the slot.
- `return g(n)` with a named result is wrong **even without a loop** (`named_result_forward.go`: `g(1)` returns 3, not 2).

**Cause**: `interp/run.go` `call()`, "Init return values": the callee's result slots are aliased to the caller's destination slots (`nf.data[i] = v(f)`) to avoid a copy on return, so a named result starts out holding whatever that slot already held.

**Fix** (tested on a patched copy of v0.16.1, not yet applied to the PoC): after the parameter copy, zero every aliased result slot. It must come after the copy, not at slot setup, because with `return g(n)` the aliased slot is also where the argument is read from. All six `testdata/broken/` repros then match `go run`, and yaegi's own `interp` test suite shows the identical 44 failures with and without the patch (all pre-existing on Go 1.26: loop-variable closure semantics and vendor-path tests), so no regressions.

**Consequence**: this is the first compatibility finding that produces a plausible wrong answer rather than an error, and named results in small helpers are a common Go idiom. The yaegi runtime can't ship on stock v0.16.1. Options: carry the one-hunk patch via a `replace` to a fork (and upstream it), or reject named results at snippet-validation time. The fork is the better answer; the validator is a cheap belt-and-braces check either way. It also puts yaegi's maintenance state on the record: no release in 2.5 years and 186 open issues, so we should expect to carry patches.

### In-cluster test on GKE Autopilot + gVisor: symbol packs work
New cluster `kg-sandbox` (GKE Autopilot, us-central1, rapid channel, 1.36.4, labeled `purpose=kode-gopher-slice6`) and a private Artifact Registry repo `kode-gopher-dev`. `Dockerfile.packs` builds the runner and packs in one stage, as the plugin ABI requires. **Runner image 55.9 MB** (vs 2.2 GB for the compiled sandbox image); k8s pack 93.7 MB.

- **`image:` volumes: rejected by Autopilot** at admission (`autopilot-volume-type-limitation`: allowed types are configMap/csi/downwardAPI/emptyDir/gcePersistentDisk/hostPath/nfs/persistentVolumeClaim/projected/secret/ephemeral; not bypassable via allowlist). So on Autopilot the pack ships as an initContainer image (`k8s-pack-init`, busybox + the `.so`) that copies into an `emptyDir`. Image volumes remain an option for Standard clusters only.
- **dlopen under gVisor: works.** `manifests/pack-test.yaml` runs `k8s_api_types.go` with in-cluster credentials in a gVisor pod (`runtimeClassName: gvisor`, landed on a `sandbox.gke.io/runtime: gvisor` node) and a runc control pod. Both load `k8ssyms.so (10 pkgs)` and produce identical output; the pod count (51) matches `kubectl`.

| runtime | setup (dlopen + interp) | eval+run+API | total |
|---|---|---|---|
| runc | 187 ms | 234 ms | 421 ms |
| gVisor | 398 ms | 273 ms | 671 ms |

gVisor roughly doubles pack load time (~200 ms extra for the 90 MB `.so`). Still sub-second end to end, against ~26–40 s for the compiled path on Autopilot.

Both pods ran stock yaegi, so both reported the same wrong CPU/memory totals from the named-result bug above: a consistent reproduction on real infra, not a gVisor effect.

## Differential corpus: yaegi is wrong on half of idiomatic Go — 2026-09-24

The named-result bug was found by accident, so we built a systematic check. `experiments/yaegi-poc/cmd/kg-difftest` builds each snippet in `testdata/diff/` with the Go toolchain (the oracle), runs it, then runs it under each interpreter and compares stdout and exit code. The corpus is 32 stdlib-only snippets, each targeting an idiom models write: named results, defer/recover, closures, generics, embedding, fmt/json over user types, errors.As, type switches, sort/io interfaces, goroutines, context, templates, time, local HTTP, a paging loop, panics and os.Exit.

| interpreter | wrong or failing (of 32) |
|---|---|
| yaegi v0.16.1 stock | 18 |
| yaegi `master` (Feb 2026) | 17 (fixes only Go 1.22 loop-var semantics) |
| **yaegi v0.16.1 + our named-results patch** | **16** |

The patch fixes three snippets: named results, the paging loop and local HTTP. Upgrading to `master` fixes one. Everything else fails identically on all three builds.

**Silent wrong output** (runs, exits 0, prints something plausible), with the patch applied:

| snippet | what goes wrong |
|---|---|
| 12 json_marshal | `encoding/json` on interpreted structs: **unexported fields are emitted** (as `"Xinternal"`), **embedded structs aren't flattened** (`"Meta":{...}`), **custom `MarshalJSON` is ignored** |
| 01 named_results | `%+v` prints unexported fields as `Xsum`, `Xn`. yaegi exports them under an `X` prefix in the reflect types it builds, which is also the root of the JSON leak |
| 10 stringer_fmt | `String()` on an interpreted type isn't called for slice elements: `[0 2]` instead of the names |
| 15 type_switch | a concrete `case Rect` listed before `case Shape` loses to the interface case |
| 02 defer_recover | deferred call arguments aren't evaluated at the `defer` statement: `2 1 0` prints as `3 3 3` |
| 03 loopvar | pre-Go 1.22 loop-variable capture: `3 3 3` instead of `0 1 2` (fixed on `master`) |
| 11 type_names | `%T` shows `struct { Name string; CPU int }` instead of `main.Pod` |

**Loud failures** (error, non-zero exit):
- generic funcs with two type parameters (`undefined type for U`);
- generic stdlib (`slices.Sort`, `iter`). This one is structural: generic functions can't be exported as symbols at all;
- `errors.As` with an interpreted pointer error type;
- typed `iota` expressions (`constant definition loop`);
- method values;
- `text/template` calling a method on an interpreted type;
- interface embedding.

`os.Exit(3)` also comes out as exit 1, but that one is in our runner and ours to fix.

What passes: closures with state, generic types, embedding and promotion, `json.Unmarshal`, snippet-implemented `sort`/`io` interfaces, goroutines and channels, context, value and reference semantics, control flow, strings, `time`, struct literals, unrecovered panics, the paging loop and local `net/http`.

### Other results from the same session
- **Pack/runner version mismatch fails loudly**: `plugin.Open(...): plugin was built with a different version of package github.com/traefik/yaegi/internal/unsafe2`, exit 1. That was the expected behavior; now confirmed.
- **How the patch is carried**:
  - the fix lives in `experiments/yaegi-poc/patches/yaegi-v0.16.1-named-results.patch`;
  - `yaegi-patch.sh` applies it into a gitignored `third_party/yaegi`;
  - a `go.mod` replace points yaegi at that copy.

  A pack rebuilt against it gives the right k8s totals (1250m / 590Mi, matching `kubectl`) in 116 ms. `Dockerfile.packs` doesn't apply the patch yet. The images pushed to `kode-gopher-dev` use stock yaegi.

### Consequence
This reverses the slice-6 premise. The packaging questions all came out well: extraction, packs, gVisor, and Autopilot delivery. But the interpreter is the product, and it emits wrong JSON for ordinary structs, and printing a JSON result is kode-gopher's output contract. A static validator can't realistically catch several of the silent classes (JSON of user structs, Stringer in collections, type-switch ordering, defer arguments) without rejecting most non-trivial snippets. Upstream isn't moving either: `master` fixes one of these relative to a 2.5-year-old release.

Options, pending decision:
1. **Drop yaegi and make the compiled path fast instead.** The compiled path's latency is build time, and the curated package set is fixed and known. That is the ideal case for a pre-warmed `GOCACHE` of precompiled curated packages shipped with the image. Most of slice 6's infra findings carry over: gVisor, Autopilot initContainer delivery, the image-size budget.
2. **Keep yaegi as a narrow, validated fast path.** This means a validator that rejects the known-bad patterns, with the difftest corpus in CI. The risk is that the validator either rejects most snippets or misses a silent class.
3. **Fix yaegi.** The X-prefix field export is architectural: it's how yaegi builds reflect types at runtime. High effort on a lightly maintained codebase.

Recommendation: 1, measuring the warm-cache compile latency first. If it gets to a few seconds, yaegi's remaining speed advantage doesn't justify its correctness risk.

## Slice 7 gate: where compiled-path latency goes — 2026-09-27

Slice 1.7 measured ~55 s per `kode-gopher exec` on GKE Autopilot against ~5 s on kind, and guessed at gVisor, Autopilot CPU accounting and cold filesystems. We measured the build phases directly with `scripts/measure-build/`:
- programs: stdlib-only, GCS, and client-go + GKE API;
- phases: `go mod tidy`, then `go build` with `-debug-trace` for per-action timing and cache misses;
- two rounds, in a fresh work dir bootstrapped exactly like the executor does it;
- environments: local Docker, and three GKE Autopilot pods on the sandbox image built from `sandbox/Dockerfile` at `main`.

Build time in ms, round 2 (round 1 is within noise except where noted):

| | local runc, 2 CPU | GKE runc, 2 CPU | GKE gVisor, 2 CPU | GKE gVisor, today's 100m req / 2 CPU limit |
|---|---|---|---|---|
| stdlib `tidy` + `build` | 49 + 253 | 101 + 495 | 536 + 592 | 570 + 709 |
| GCS `tidy` + `build` | 147 + 3240 | 107 + 4039 | 819 + 6059 | 831 + 5976 |
| client-go `tidy` + `build` | 93 + 3288 | 183 + 6663 | 725 + 6807 | 731 + 7113 |
| GCS build, `-ldflags='-s -w'`, no tidy | 2520 | 5327 | 5067 | 5096 |

Round 1 `tidy` under gVisor is 1-2.5 s (first touch of the module cache).

What the traces show:
- **The prewarmed `$GOCACHE` works.** In every environment only the snippet's own package is compiled; every dependency is a cache hit.
- **Linking is the biggest single cost**, 1.7-2.6 s for GCS and client-go programs, and about the same under gVisor. `-s -w` saves 0.6-1 s of it.
- **gVisor roughly doubles a warm build**, from ~3.5 to ~6.5 s. The extra time is filesystem work, not CPU: package loading goes from ~0.4 to ~1.2 s, and cache checks across ~1000 packages add ~2 s. `go mod tidy` goes from ~0.1 to ~0.7 s.
- **Today's small CPU request doesn't matter on an idle node.** It could under contention; that wasn't tested.

So a warm call on GKE + gVisor costs ~7-8 s of `tidy` + `build`, not 55 s.

**The missing ~45 s is a stale image.** `ghcr.io/gke-demos/kode-gopher-sandbox:latest`, which the GKE overlay pulls, predates the lockfile bootstrap (`/opt/kode-gopher-base/go.mod`). Without that bootstrap, `go mod tidy` on an empty `go.mod` resolves newer versions than prewarm compiled, and the cache misses. We reproduced this under gVisor with the `main` image by starting from `go mod init`:
- **GCS: 9.3-9.9 s `tidy` + 59-60 s `build`, twice.**
- **client-go: OOM-killed at both 2 GiB and 4 GiB.** Under gVisor the OOM takes down the whole sandbox container.

That matches slice 1.7's ~55 s. The current executor also runs `cp /opt/kode-gopher-base/go.mod ...` under `set -e`, so with the GHCR image as published it fails the tidy phase outright (confirmed later the same day; see the next entry).

Not measured: the agent-sandbox layer (claim, router, four `Execute` round trips, `/app` reset). On kind it's ~1.5 s (5 s total against ~3.5 s of build).

### Consequences for slice 7
1. **Republish the sandbox image, and make it impossible to go stale.** This is most of the win: 55 s → ~8 s. Build and push from CI on changes to `sandbox/`, `internal/prewarm/` or `internal/executor/`. Pin the GKE overlay to an immutable tag or digest rather than `:latest` + `Always`.
2. **Skip `go mod tidy` when every import is covered by the prewarm lockfile.** Saves 0.5-2.5 s under gVisor. Tidy stays for `extra_imports` and caller-supplied `go.mod`.
3. **Link with `-ldflags='-s -w'`.** Saves 0.6-1 s. Snippet binaries are run once and discarded, so debug info buys nothing.
4. **gVisor file-access overhead (~3 s) is the remaining floor.** Worth trying: gVisor's overlay/rootfs caching options, or keeping `$GOCACHE` on a medium gVisor handles faster. Measure before committing.
5. **Cache misses can crash the sandbox.** A snippet whose `extra_imports` move a shared dependency's version off the lockfile recompiles large trees, and client-go doesn't fit in 4 GiB. Either raise the limit or make `extra_imports` version changes visible, e.g. warn when tidy changes a lockfile-pinned version.

## Slice 7: our own sandbox image, published by CI and pinned — 2026-09-29

Fix for the stale image found at the slice 7 gate. While fixing it we dropped the last dependency on `gke-demos/go-runtime-sandbox`: its image was our base.

- **Our own in-pod server: `cmd/sandbox-server`.** The upstream image contributed a Go toolchain, a uid 1000 user, some env, and `sandbox-server`: the HTTP server on :8888 that the agent-sandbox client (through the router) calls. The protocol belongs to agent-sandbox, not go-runtime-sandbox; its reference runtime is `examples/python-runtime-sandbox`. We implement all six endpoints: `GET /`, `POST /execute`, `POST /upload`, `GET /download/`, `/list/`, `/exists/`. We only call `/execute` and `/upload`, plus `/` from the readiness probe, but the stock client's `Read`/`List`/`Exists` also work. It's stdlib only:
  - `sh -c` runs in its own process group, so a cancelled request kills compiler children too;
  - a `WaitDelay` stops a backgrounded child from holding `/execute` open;
  - a signal death reports 128+signal;
  - each stream is capped at 4 MiB, keeping the response under the client's 16 MiB limit;
  - uploads are written to a temp file and renamed into place;
  - file endpoints go through `os.Root`, so neither `..` nor symlinks leave `/app`.
  
  Tests speak the client's wire format: percent-encoded paths with `/` as `%2F`, and multipart field `file`.
- **The image is ours end to end.**
  - Base images, pinned by digest: `golang:1.26.8-bookworm` for the toolchain and `debian:bookworm-slim` for the runtime. This is the same shape as upstream's image, without its 94 MB stdlib cache, which the prewarm makes redundant.
  - The toolchain version is now a line in our Dockerfile. It used to be whatever upstream last pushed, and toolchain changes invalidate every prewarmed cache entry.
  - `sandbox-server` builds as a throwaway module, so root `go.mod` changes don't feed the image tag.
- **New package: `ghcr.io/gke-demos/kode-gopher/sandbox`.** The old `kode-gopher-sandbox` package is linked to `gke-demos/go-runtime-sandbox`: it inherited that from the base image's source label at first push, and was last updated 2026-05-21. Pushing there from this repo's `GITHUB_TOKEN` would need a UI-only "Manage Actions access" grant. A new package created by this repo's workflow is linked to this repo automatically. Like every new GHCR package it starts **private**, so flip it to public once after the first push (as in slice 1.7). The old package can be deleted after that.
- **Content-derived tag.** `scripts/sandbox-image-tag.sh` hashes every tracked file under `sandbox/`, `cmd/sandbox-server/` and `internal/prewarm/`, giving a tag like `p-aa7c324c5cee`. A commit SHA can't work: the overlay would have to name the commit it lives in. With a hash of the inputs, the PR that changes the image also updates the pin (`make sandbox-pin`).
- **`.github/workflows/sandbox-image.yml`**:
  - on `main`, builds and pushes `:<tag>` and `:latest` to GHCR; if the tag already exists (e.g. after a revert), it only moves `:latest`;
  - on PRs, builds without pushing.
- **`.github/workflows/ci.yml`** (the repo's first CI) runs build, vet, test, `make prompts-check`, and `make sandbox-pin-check`. The pin check fails any PR whose overlay doesn't pin the current tag. CI would also have caught that the slice 7 gate commit broke `go build ./...`: its measurement programs were outside a `testdata/` dir, so they became packages of the root module. They're moved.
- **The overlay pins the tag with `imagePullPolicy: IfNotPresent`**, replacing `:latest` + `Always`.
- **The executor names the failure.** Against an image without `/opt/kode-gopher-base/go.mod`, the tidy phase now says so and points at the pin. Before, it failed with a bare `cp` error. Verified against the old published `:latest`, which fails this way: the current executor never worked with the image the GKE overlay pulled.

**Known gap:** right after a merge that changes the image, the pinned tag doesn't exist until `sandbox-image` finishes (several minutes, since prewarm compiles the GCP SDK and client-go). Deploying in that window fails with an image pull error, not a wrong image.

## Slice 7: fast path — build first, fast nodes, and a GKE addon that moved — 2026-09-29

The rest of slice 7, verified with `scripts/smoketest-gke.sh` on a fresh rapid-channel Autopilot cluster (1.36.4, `--enable-agent-sandbox`), using the CI-published image `p-c158bbe9970d`.

**Result:** the GCS snippet takes **4.3-4.4 s** of CLI wall-clock end to end over three runs (target: under 12 s). That covers claim, upload, a 2.3-2.6 s build, run and fetch.

| `kode-gopher exec` on GKE | build | total (build + run) | wall-clock |
|---|---|---|---|
| `list_buckets_snippet.go`, default nodes (E2-class `ek-standard-8`) | 11.1 s | 12.5-12.7 s | ~13 s |
| `list_buckets_snippet.go`, C3 via `kode-gopher-sandbox` ComputeClass | 2.3-2.6 s | 3.5-3.8 s | 4.3-4.4 s |
| `list_gke_pods_snippet.go` (client-go + container API), C3 | 2.5 s | 3.3 s | — |
| non-lockfile import (`github.com/fatih/color`), C3 | 1.3 s incl. tidy | 1.3 s | 2.2 s |
| `extra_imports` that moves otel/logr/x/sys versions, C3 | 25.6 s (cache miss) | 26.7 s | 27.5 s, with warning |

### Build first, tidy only on a lockfile miss
The build phase is now a single `Execute`, in `internal/executor.buildCmd`:
1. Bootstrap `go.mod`/`go.sum` from the prewarm lockfile, as before.
2. Run `go build -ldflags='-s -w'` straight away. The default `-mod=readonly` either builds with exactly the lockfile's versions or fails fast.
3. Only if the error is about a missing module or `go.sum` entry: run `go mod tidy` and build again.

This covers `extra_imports` and caller `go.mod`s without special-casing them. There's one fewer round trip even when tidy does run.

Curated-only snippets never run tidy. Compile errors come straight back, without a tidy.

`Outcome`, and the MCP output, gain three fields:
- `tidied`;
- `warnings`;
- `build_ms`.

### Cache-miss guard: a warning, not more memory
When tidy moves a module that the lockfile pinned, the build reports it and the tool result carries a warning naming each `path old -> new`. Moving a pinned module means recompiling everything built against it. The warning says so and suggests curated packages instead.

We kept the 2 GiB limit. Raising it would buy client-go-scale cache misses at the cost of a much bigger Autopilot request for every sandbox. Those builds take minutes anyway. The warning is what lets the model steer back.

### Cache mtimes pushed into the future
Go's build cache bumps the mtime of any entry it uses that is more than an hour old. That's how `go clean -cache` trimming knows what's live. In a fresh container every entry baked into the image is "old". So the first build chtimes hundreds of cache files, and each chtime copies the whole file up out of the image layer.

Every claim gets a fresh warm-pool pod, so every request pays this. `sandbox/Dockerfile` now `touch`es the cache to 2100-01-01 right after the prewarm build. A future mtime is never old and never trimmed.

First GCS build in a fresh container:

| | image older than 1 h | future mtimes |
|---|---|---|
| local Docker (runc) | 13.3 s | 2.0 s |
| GKE gVisor, default nodes | 14.9 s | 9.0 s |

The GKE numbers were measured at the same time on the same node.

### gVisor "filesystem overhead" was mostly slow CPUs
We measured before changing anything, per the plan. On default Autopilot gVisor nodes, an edit rebuild of the GCS program splits like this (`-debug-trace`):

| step | time |
|---|---|
| package load | ~1.5 s |
| cache checks | ~1.5 s |
| compiling the one-file `main` | 2.2-3.5 s |
| link | 2-3 s |

That rebuild takes 6-8 s; a no-op rebuild takes 2.3-2.5 s. Experiments:
- **tmpfs:** copying the 405 MB of dependency archives to tmpfs and running `go tool compile`/`link` by hand cut compile from 3.4-4.7 s to ~2.4 s. Link was unchanged.
- **Plain file I/O is cheap:** stat of 5.7k cache files takes 0.3 s; reading 1.1 GB takes 2.3 s.
- **`GOGC`** 200/400/off: within noise.

So filesystem access is worth about 1 s. The rest is CPU under gVisor.

The same pod on a **C3** node, with the same 2-CPU limit:

| | default nodes | C3 |
|---|---|---|
| first build | 6.2 s | 2.4 s |
| edit rebuild | 6.0 s | 2.2 s |
| no-op rebuild | 2.3 s | 0.63 s |
| stat of the cache | 0.31 s | 0.10 s |

Autopilot's default class puts gVisor pods on E2-class `ek-standard` nodes. The overlay now adds `manifests/overlays/gke/computeclass.yaml`:
- the priority list is C3, then C4, N4, N2;
- `whenUnsatisfiable: ScaleUpAnyway`;
- sandbox pods select it with `cloud.google.com/compute-class`.

Unlike the built-in `Performance` class, which gives every pod its own node, a custom class bin-packs pods. N4 was stocked out in us-central1-c during the test, hence the fallbacks.

Cost: we pay C3 prices for sandbox nodes. There's nothing left to gain from gVisor filesystem options.

### The agent-sandbox addon moved to v1beta1
The same overlay worked on 2026-09-27. On a new rapid-channel cluster, the addon now serves the v1beta1 API (agent-sandbox v1.0), which broke four things. The fixes keep our v0.4.6 client working:

- **The template may not mount the KSA token.** The ValidatingAdmissionPolicy `sandbox-core-policy` rejects `automountServiceAccountToken: true`. The overlay sets it to `false`.
  - `rest.InClusterConfig()` snippets therefore don't work on GKE any more, so `scripts/smoketest-gke.sh` drops `list_k8s_version_snippet.go`.
  - The kind smoketest still runs it.
  - `list_gke_pods_snippet.go` authenticates with forwarded Google credentials and is unaffected.
- **Claims name a warm pool, not a template.** v1beta1 `SandboxClaim.spec` has a required `warmPoolRef`. A v1alpha1 claim for template `T` converts to one for pool `shadow-pool-T`, which nothing creates. Nothing picked the claims up, and opening a sandbox timed out after 3 min. We renamed our warm pool to `shadow-pool-go-runtime-template`. Older addons match claims to pools by template, so the name works on both.
- **Per-sandbox Services are back.** The template CRD has `spec.service` again, and the router resolves `<sandbox>.<ns>.svc`. The overlay no longer removes it. Without it: 502, `Name or service not known`.
- **The managed NetworkPolicy blocked our router and DNS.**
  - The default admits ingress only from `app=sandbox-router` in `agent-sandbox-system`; ours runs in the sandbox namespace.
  - Its egress (internet, no private ranges) excludes NodeLocal DNSCache at 169.254.20.10, so nothing resolved and every GCP call hung until the 90 s timeout.
  - The overlay now sets `spec.networkPolicy`: ingress on 8888 from our router, egress to the internet plus DNS to 169.254.20.10 and kube-dns.

**Follow-up:** move the client to `sigs.k8s.io/agent-sandbox` v1.0.x and the manifests to v1beta1, and retire the `shadow-pool-` naming trick. Also decide what "the sandbox's own cluster" means on GKE now that the KSA token can't be mounted. `docs/design.md` and the prompt still describe `rest.InClusterConfig()` as working.

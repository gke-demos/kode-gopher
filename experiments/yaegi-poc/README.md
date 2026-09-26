# yaegi-poc — can we use a Go interpreter instead of compile-and-run?

> **Shelved 2026-09-24.** Packaging all worked: extraction, symbol packs, gVisor, and GKE Autopilot delivery. The interpreter did not. The differential corpus below finds silent wrong output in 7 of 32 idiomatic snippets, including `json.Marshal` of snippet-defined structs. This directory is kept as the record and as a quick recheck (`kg-difftest`) if yaegi improves. Decision and details: `docs/decisions.md > Differential corpus`. Next step: slice 7, a fast compiled path, in `docs/plan.md`.

Local-only proof of concept exploring an alternative sandbox backend that uses [Yaegi](https://github.com/traefik/yaegi) (a Go interpreter from Traefik Labs) instead of the Go toolchain.

The compiled path that ships in kode-gopher today pays a ~25–30s `go build` per cold call and lives under the agent-sandbox HTTP layer's 60s per-call cap — both of which our slice 0.5 prewarmed image is structured around. Yaegi promises to make both moot: no compile step, ms startup, smaller image (no toolchain, no `$GOCACHE`).

## Setup

Standalone Go module so yaegi doesn't leak into the main kode-gopher dependency graph. From `experiments/yaegi-poc/`:

```bash
go install github.com/traefik/yaegi/cmd/yaegi@latest

# Stdlib-only baseline
go run . testdata/hello.go

# REST GCS client (google.golang.org/api/storage/v1)
go run -tags rest . testdata/list_buckets_rest.go

# "cloud.google.com/go/storage" client (HTTP-mode under the hood)
go run -tags grpc . testdata/list_buckets_grpc.go
```

Symbol files for non-stdlib packages are pre-generated and committed:

```bash
yaegi extract -name main -tag rest google.golang.org/api/storage/v1
yaegi extract -name main -tag grpc cloud.google.com/go/storage google.golang.org/api/iterator
yaegi extract -name main -tag compute cloud.google.com/go/compute/apiv1
yaegi extract -name main -tag container cloud.google.com/go/container/apiv1
```

The two protobuf-type extracts are **gitignored** (2.3 MB and 163 KB of generated
registration code); regenerate them before running the compute/container tests:

```bash
yaegi extract -name main -tag compute cloud.google.com/go/compute/apiv1/computepb
yaegi extract -name main -tag container cloud.google.com/go/container/apiv1/containerpb
```

### Symbol packs — symbols outside the binary — works ✅ 2026-09-20

Build tags link symbols *into* the runner at build time, which makes the binary a
function of the curated set. `plugins/` tests the alternative: symbol sets built as
Go plugins and loaded at startup from `KG_SYMBOL_PLUGINS`, so they can be shipped
and updated as separate artifacts (an OCI image volume, say).

**It works.** Measured against the live `gke-demos-345619` project:

| runner | packs mounted | result |
|---|---|---|
| 34 MB, no SDK symbols linked | *none* (control) | **fails**, cleanly: `unable to find source related to: "cloud.google.com/go/container/apiv1"` |
| same 34 MB binary | `containersyms.so` (41 MB) | **works** — true gRPC call, 10 GKE clusters, 476 ms total (setup 36 ms) |
| same 34 MB binary | `computesyms.so` (83 MB) | **works** — 130 zones, 416 ms total (setup 116 ms) |
| same 34 MB binary | **both** (124 MB) | **both snippets work** — 5 packages from 2 `.so`, setup 99–116 ms |

The binary never changes across those rows — only what's mounted next to it. dlopen
costs ~100 ms of interpreter setup against ~6 ms fully-linked, which is noise beside
the 5–30 s the compiled path pays.

The consequence: the runner stops being a function of the curated set, and extraction
cost (client-go's ~25 min especially) becomes an occasional offline publish of one
pack instead of a cost on every runner build.

Reproduce:

```bash
# Packs — the same generated extracts, build tags stripped, plus a
# three-line `var Symbols = interp.Exports{}`.
mkdir -p /tmp/kgsyms
go build -buildmode=plugin -o /tmp/kgsyms/containersyms.so ./plugins/containersyms
go build -buildmode=plugin -o /tmp/kgsyms/computesyms.so   ./plugins/computesyms

# Runner with NO SDK symbols linked in (34 MB).
go build -o /tmp/yaegi-poc-plugin .

# Control: fails, cleanly.
/tmp/yaegi-poc-plugin testdata/list_clusters_container.go

# With packs mounted: both work, ~100 ms of dlopen in setup.
export KG_SYMBOL_PLUGINS=/tmp/kgsyms
/tmp/yaegi-poc-plugin testdata/list_zones_compute.go
/tmp/yaegi-poc-plugin testdata/list_clusters_container.go
```

What still needs settling — none of these contradict the result above, they bound
where it applies:

- **Untested on the real substrate.** dlopen under gVisor, and Kubernetes `image:`
  volume sources. Neither was checkable because the `ap-gke-sandbox` cluster no
  longer exists. Fallback needing no new API surface: an initContainer that copies
  `.so` files out of a symbol image into an `emptyDir`.
- **Untested failure mode.** A version-mismatched pack is *expected* to fail loudly
  at `plugin.Open`, not silently misbehave — but that wasn't exercised.
- Plugins need CGO and a glibc base image (not alpine/musl, not `distroless/static`).
- Host and plugin must share a toolchain and identical versions of every package
  they *both* link. Here that's only stdlib + yaegi, so an SDK bump rebuilds one
  pack while a Go or yaegi bump rebuilds everything — that asymmetry is what makes
  independent pack updates viable.
- Packs sharing transitive deps (protobuf, grpc) must come from one common `go.mod`.
  The two-pack row above passes because both packs did. Packs are individually
  regenerable, **not** individually pinnable.

### Extracting symbols — use `kg-extract`, not `yaegi extract`

`cmd/kg-extract` is a drop-in replacement for `yaegi extract` (same
`-name -exclude -include -tag -out` flags, same output filenames). Use it for
everything:

```bash
go build -o /tmp/kg-extract ./cmd/kg-extract
/tmp/kg-extract -name main -out /tmp/kgx k8s.io/client-go/kubernetes
```

It is ~25× to >20,000× faster than upstream and produces byte-identical output.
See *The extractor* below for why.

Packages to extract are pinned in `symboldeps.go` (build-tagged `symboldeps`, so
it never enters a real build). **Add a blank import there and run `go mod tidy`
before extracting anything new** — `go get <module>` does not record transitive
`go.sum` entries, and a missing one is what killed two multi-hour client-go
attempts.

## Findings

Three real tests, all against the live `gke-demos-345619` GCS project's 21 buckets, end-to-end times measured by the runner:

| test | yaegi total | compiled-path equivalent | result |
|---|---|---|---|
| stdlib baseline (`hello.go`) | **7 ms** (setup 5, eval+run 2) | n/a | ✅ correct JSON returned |
| REST GCS — `google.golang.org/api/storage/v1` | **768 ms** (setup 6, eval+run+API 762) | ~5 s warm / ~30 s cold (kind), ~30 s warm / ~55 s cold (GKE) | ✅ 21 buckets, matches gcloud chronologically |
| HTTP-mode `cloud.google.com/go/storage` | **1093 ms** (setup 14, eval+run+API 1079) | same | ✅ 21 buckets, matches gcloud chronologically |

The interpreter itself is in single-digit ms. Most of the wall time is the actual GCS API call.

### What we verified

- **Yaegi handles the modern `cloud.google.com/go/*` client patterns.** Reflection-heavy code (iterators, typed attrs, `errors.Is` against package sentinels) works. We initially expected the "gRPC client" to fail because of `unsafe` and gRPC's internal reflect, but `storage.NewClient` defaults to HTTP+JSON transport — so this test exercised the wrapper code, not the gRPC wire layer.
- **The 60s HTTP cap stops mattering.** Total round-trips of <1s are well under any reasonable timeout.
- **Image footprint would shrink dramatically.** A Yaegi runner doesn't need the Go toolchain or `$GOCACHE`/`$GOMODCACHE` baked in — just the runner binary + compiled symbols. Plausibly 50–100 MB vs our current 2.2 GB.

### The gRPC question — answered 2026-09-20 ✅

Run these two (both hit the live `gke-demos-345619` project):

```bash
# protobuf-typed REST client
GOOGLE_CLOUD_PROJECT=gke-demos-345619 go run -tags "compute grpc" . testdata/list_zones_compute.go

# true gRPC client
GOOGLE_CLOUD_PROJECT=gke-demos-345619 go run -tags container . testdata/list_clusters_container.go
```

| test | client kind | yaegi total | result |
|---|---|---|---|
| `compute/apiv1` zones | REST + protobuf | **410 ms** | 130 zones, identical to `gcloud` (which took 1.56 s) |
| `container/apiv1` clusters | **true gRPC** | **446 ms** | 10 clusters, identical to `gcloud` |

Two corrections to what this README said before:

1. **`compute/apiv1` is not a gRPC client.** It exposes only `New*RESTClient` constructors — the Compute API has no gRPC surface. The genuine gRPC client is `container/apiv1`'s `NewClusterManagerClient`.
2. **The `unsafe` worry below was a category error.** Yaegi interprets *only the snippet*. Everything reached through a `yaegi extract` file is compiled code linked into the runner — `unsafe`, assembly and reflection inside `google.golang.org/grpc` never touch the interpreter. What can actually break is the inverse: interpreted code that has to *satisfy* an interface called by reflection-heavy compiled code, or interpreted types that protobuf needs to reflect over. Our snippet shape ("call the SDK, marshal the result") rarely does either.

### What we still didn't verify
- **CPU-heavy workloads.** Interpreted Go runs much slower than compiled at compute. Doesn't matter for "make an HTTP call, marshal a response"; matters for anything iterating large datasets in-process.
- **Generics edge cases.** Yaegi added generics support relatively recently and it's not 100% complete in v0.16.1. Some idiomatic Go 1.21+ patterns may break.

### Symbol-extraction surprises

- `google.golang.org/api/option` could not be extracted: its public types reference `google.golang.org/api/internal`, which extract-output then can't import from outside the parent package. **Workaround**: drop `option` and rely on Application Default Credentials via env vars. Probably fixable with extract's `-exclude` flag if we cared.
- `yaegi extract` for `cloud.google.com/go/storage` + `iterator` took ~30 s for the whole download + reflection walk + file write. Manageable as a one-shot CI step per curated package.
- ~~**Extraction cost is the real constraint, not compatibility.**~~ It was, until we
  looked at why. See *The extractor* below — client-go went from never-finishing to
  2 s. Generated *size* is still real: `computepb` is 2.3 MB / 18,671 lines. The
  `*pb` extracts are gitignored here and regenerated on demand (see Setup). Runner
  binary weighed 96 MB with compute+storage+iterator linked, 40 MB with container
  alone — relevant to the slice's image-size target.
- Generated symbol files are large (212 lines for storage/v1, ~thousands for the storage gRPC client) but plain `init()` registration — no runtime cost beyond startup.

### The extractor — two upstream bugs, 2026-09-21

**`yaegi extract` is slow for one reason, and it's a one-line fix.** `extract.go`
type-checks with `importer.ForCompiler(fset, "source", nil)`, which re-parses the
entire transitive source graph and ignores `GOCACHE` completely. Swapping it for
`golang.org/x/tools/go/packages` with `NeedTypes` (which reads compiled export
data) is the whole change. `fastextract/` is that fork; `fastextract/loadpackage.go`
is the replacement importer.

| package | `yaegi extract` | `kg-extract` |
|---|---|---|
| `container/apiv1` | 48 s | **1.3 s** |
| `compute/apiv1` | 52 s | **3.1 s** |
| `compute/apiv1/computepb` | 22 s | **2.4 s** |
| `k8s.io/client-go/kubernetes` | >12 h / 10.9 GB, never finished | **2.0 s** |
| whole k8s set (5 pkgs) | — | **~6 s** |

All seven packages we had yaegi-generated baselines for re-extract **byte-identical**.
That's the equivalence claim, and it's also the regression test for the second bug:

**Import alias collisions.** `qualify` returns a bare `pkg.Name()` and the template
emits unaliased imports, so `k8s.io/client-go/kubernetes` — 60 direct imports, a
dozen of them named `v1`/`v1beta1`/`v1alpha1` — generates code that doesn't compile:

```
k8s_io-client-go-kubernetes.go:12:2: v1alpha1 redeclared in this block
```

Nobody upstream hit this because the importer never got far enough to emit the file.
Fixed by aliasing **only on collision** (sanitised full path, e.g.
`k8s_io_client_go_kubernetes_typed_apps_v1`), assigned over direct imports in path
order so output doesn't depend on symbol walk order.

Both fixes are worth upstreaming: an `Importer` field on `Extractor`, and the alias
fix (a plain bug, independent of the importer).

### client-go end to end ✅ 2026-09-21

The package that could not be extracted at all now runs through the interpreter with
symbols from a mounted pack. `testdata/list_nodes_k8s.go` calls
`clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})` — the typed clientset:

```bash
go build -buildmode=plugin -o /tmp/kgsyms/k8ssyms.so ./plugins/k8ssyms   # 90 MB, 11 s
KG_SYMBOL_PLUGINS=/tmp/kgsyms KUBECONFIG=~/.kube/config \
  /tmp/yaegi-poc-plugin testdata/list_nodes_k8s.go
```

| runner | pack | result |
|---|---|---|
| no k8s symbols linked | none | fails cleanly: `unable to find source related to: "k8s.io/apimachinery/pkg/apis/meta/v1"` |
| same binary | `k8ssyms.so` | **3 nodes, matches `kubectl get nodes`, 61 ms total** (setup 48, eval+run 13) |

Caveat on coverage: the snippet names only `kubernetes`, `clientcmd` and `metav1`.
A snippet naming `corev1.Pod` also needs `k8s.io/api/core/v1` extracted. The curated
k8s pack needs the common `k8s.io/api/*` group-versions, not just the clientset —
cheap now, but not yet measured.

## Implications for kode-gopher

A Yaegi backend looks viable for GCP REST, HTTP-mode `cloud.google.com/go`, **true gRPC, and the client-go typed clientset** — i.e. the bulk of what kode-gopher snippets actually do. It is still *not* a wholesale replacement for the compiled path: anything outside the curated set, cgo, anything CPU-bound, and long-tail interpreter gaps (generics) argue for keeping both backends.

The clean shape would be:

- A second sandbox image (`kode-gopher-sandbox-yaegi:latest`) containing the yaegi runner binary + compiled curated symbols. No Go toolchain. Tiny.
- A second SandboxTemplate (`go-runtime-yaegi-template` or similar) pointing at that image.
- A `--runtime=compiled|yaegi` flag on `kode-gopher exec` and `kode-gopher serve`. Default stays `compiled` for the broadest compatibility; `yaegi` is opt-in.
- The MCP tool surface could expose `runtime?: "compiled" | "yaegi"` so the LLM picks per call. Models could learn "use yaegi for quick lookups, compiled for anything else."

What this is not:

- Free. Symbol extraction is real CI surface; package upgrades break it; some packages just don't extract; some packages run but fail at non-obvious edges. The compiled path stays the safe default.
- A path to multi-tenancy. The session-per-server-process model would still apply. (Though smaller pods + faster startup would make per-request session pooling more attractive in slice 5.)

## What to do with this

*Superseded 2026-09-20 — options 1 and 2 are spent.*

Option 2 ("one more test — extract `compute/apiv1`, that's the missing data point
for true gRPC") is **done, and its premise was wrong**. `compute/apiv1` is not a
gRPC client — it exposes only `New*RESTClient` constructors, because the Compute
API has no gRPC surface, so that test could never have answered the gRPC question.
It was run anyway (130 zones, 410 ms) and the real gRPC client, `container/apiv1`,
was run alongside it (10 clusters, 446 ms). Both match `gcloud` exactly. See
*The gRPC question* above.

So the case for promoting to a slice is as strong as option 2 promised it could be,
and the work moved to `docs/plan.md` slice 6. What's actually left is not a
compatibility question but a packaging one:

1. ~~**Extraction cost is the constraint.**~~ Fixed 2026-09-21 — see *The extractor*.
   The whole curated set, client-go included, extracts in seconds, so `make extract`
   can be a CI step rather than a manual overnight job. Generated *size* still matters
   (`computepb` is 2.3 MB) but that's a binary/image concern, not a build-time one.
2. **Symbol packs work** (see *Symbol packs* above — verified, including client-go).
   Still open: pack granularity. One pack per package is fine-grained but the
   shared-`go.mod` constraint means packs move together anyway, so 2-3 profile packs
   (gcp / k8s / both) is probably the right unit.
3. **The substrate is unverified and currently unavailable.** dlopen under gVisor
   and OCI `image:` volumes both need a cluster; `ap-gke-sandbox` and
   `kind-agent-sandbox-poc` are both gone. The client-go run above used a live
   third-party kind cluster read-only. Standing up a replacement cluster and
   re-pointing `scripts/smoketest-gke.sh` is a prerequisite for the slice.
4. **Plugin version-mismatch failure mode is unverified.** Expected: a loud error at
   `plugin.Open`. Worth confirming, since it's the pack design's main operational risk.

### Differential corpus ❌ 2026-09-24

`cmd/kg-difftest` checks each snippet in `testdata/diff/` against real Go as the oracle:

```sh
./yaegi-patch.sh && go build -o /tmp/yaegi-poc-plugin . && go build -o /tmp/kg-difftest ./cmd/kg-difftest
/tmp/kg-difftest -interp 'patched=/tmp/yaegi-poc-plugin' -interp 'stock=/tmp/yaegi-bin/yaegi run' testdata/diff
```

16 of 32 fail even with the named-result patch, and 7 of those are **silent wrong output**, including `json.Marshal` of interpreted structs, which leaks unexported fields, ignores `MarshalJSON` and doesn't flatten embedded structs. `testdata/broken/` holds the named-result repros. Full results and the options that follow are in `docs/decisions.md` ("Differential corpus").

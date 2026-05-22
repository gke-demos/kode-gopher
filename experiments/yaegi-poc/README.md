# yaegi-poc — can we use a Go interpreter instead of compile-and-run?

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
```

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

### What we didn't verify

- **True gRPC-only packages** (`cloud.google.com/go/compute/apiv1`, `cloud.google.com/go/container/apiv1`, BigQuery streaming, Pub/Sub streaming). These use gRPC for the actual wire and likely involve `unsafe` somewhere in `google.golang.org/grpc`. Yaegi can't `unsafe`. Untested in this PoC; should be the very next thing if we promote this to a slice.
- **CPU-heavy workloads.** Interpreted Go runs much slower than compiled at compute. Doesn't matter for "make an HTTP call, marshal a response"; matters for anything iterating large datasets in-process.
- **Generics edge cases.** Yaegi added generics support relatively recently and it's not 100% complete in v0.16.1. Some idiomatic Go 1.21+ patterns may break.

### Symbol-extraction surprises

- `google.golang.org/api/option` could not be extracted: its public types reference `google.golang.org/api/internal`, which extract-output then can't import from outside the parent package. **Workaround**: drop `option` and rely on Application Default Credentials via env vars. Probably fixable with extract's `-exclude` flag if we cared.
- `yaegi extract` for `cloud.google.com/go/storage` + `iterator` took ~30 s for the whole download + reflection walk + file write. Manageable as a one-shot CI step per curated package.
- Generated symbol files are large (212 lines for storage/v1, ~thousands for the storage gRPC client) but plain `init()` registration — no runtime cost beyond startup.

## Implications for kode-gopher

A Yaegi backend looks viable for the meaningful subset of "GCP REST + HTTP-mode cloud.go.com/go" workloads. It is *not* a wholesale replacement for the compiled path — true gRPC, cgo, anything CPU-bound, and likely some long-tail compatibility issues argue for keeping both backends if we go this route.

The clean shape would be:

- A second sandbox image (`kode-gopher-sandbox-yaegi:latest`) containing the yaegi runner binary + compiled curated symbols. No Go toolchain. Tiny.
- A second SandboxTemplate (`go-runtime-yaegi-template` or similar) pointing at that image.
- A `--runtime=compiled|yaegi` flag on `kode-gopher exec` and `kode-gopher serve`. Default stays `compiled` for the broadest compatibility; `yaegi` is opt-in.
- The MCP tool surface could expose `runtime?: "compiled" | "yaegi"` so the LLM picks per call. Models could learn "use yaegi for quick lookups, compiled for anything else."

What this is not:

- Free. Symbol extraction is real CI surface; package upgrades break it; some packages just don't extract; some packages run but fail at non-obvious edges. The compiled path stays the safe default.
- A path to multi-tenancy. The session-per-server-process model would still apply. (Though smaller pods + faster startup would make per-request session pooling more attractive in slice 5.)

## What to do with this

Three options for the user to pick from:

1. **Stop here.** PoC answers the question; document it; revisit when there's a real driver. Nothing else to build right now.
2. **One more test.** Extract `cloud.google.com/go/compute/apiv1` and try a simple call. That's the missing data point (true gRPC). If it works, the case for promoting to a slice gets much stronger.
3. **Promote to a slice.** Build the alternative sandbox image, the SandboxTemplate, the runtime flag, the smoketests. Probably ~3-4 days of focused work given the symbol-extraction CI piece.

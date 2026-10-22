# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex, Cursor and the like) working in this repo. [`CONTRIBUTING.md`](./CONTRIBUTING.md) is the process, and it binds you as it binds everyone. This file is the short version, plus the repo map and the mistakes that have cost time here. [`CLAUDE.md`](./CLAUDE.md) imports this file, so Claude Code reads the same content.

## What this repo is

kode-gopher runs Go that an AI agent writes against the real Google Cloud SDKs, in a sandboxed Kubernetes pod ([agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)), and returns a structured result. It's a CLI (`kode-gopher exec`) and an MCP server (`kode-gopher serve`), run locally over stdio, or as a shared server in GKE with Google sign-in (OAuth 2.1). Pre-alpha, 0.x.

Read before changing a subsystem:
- [`docs/design.md`](./docs/design.md): architecture, the result contract, the sandbox protocol.
- [`docs/design-in-cluster.md`](./docs/design-in-cluster.md): the shared server, OAuth, custody, egress.
- [`docs/decisions.md`](./docs/decisions.md): decisions with their evidence. Don't contradict one without new evidence; add an entry when you settle something.
- [`docs/plan.md`](./docs/plan.md): the slice plan.

## Map

| Path | What |
|---|---|
| `cmd/kode-gopher` | the binary: `exec`, `serve`, `version`; flags and wiring |
| `cmd/sandbox-server` | the in-pod server that builds and runs snippets (part of the sandbox image) |
| `cmd/mcp-smoketest` | drives `kode-gopher serve` over MCP, for the smoketest scripts |
| `internal/normalize`, `internal/wrapper` | turn a snippet or files into a buildable `package main` |
| `internal/executor` | the host side of build, run and fetch |
| `internal/sandbox` | agent-sandbox claims, leases, dialing |
| `internal/mcp` | the MCP tools and their descriptions |
| `internal/creds` | where a snippet's Google credentials come from (ADC, OAuth user, service account) |
| `internal/oauth` | the OAuth 2.1 authorization server fronting Google sign-in, allow lists, custody |
| `internal/curated`, `internal/prompts` | the precompiled package set and the generated LLM prompt |
| `internal/prewarm` | a **separate Go module**: the sandbox image's prewarmed dependencies |
| `manifests/` | kustomize: `base` (kind applies it directly), overlays `gke`, `gke-server`, `gke-server-oauth`, and `components/egress-allowlist` |
| `sandbox/`, `server/` | Dockerfiles for the sandbox image and the server image |
| `scripts/` | deploy, smoketests, warm-pool refresh, sandbox image tag |
| `dev/ci/presubmits/` | every CI check, runnable locally |
| `docs/site/` | the user docs (Astro Starlight), published from `main` |

## Before you push

1. **Run `make presubmit` on its own, read the result, then commit.** Don't run it alongside `git commit` or `git push`: lint failures have been pushed that way. CI runs the same scripts.
2. **What changed decides what else to run:**

   | You changed | Also |
   |---|---|
   | an MCP tool's description or schema | `make prompts` |
   | `internal/curated` | `make prompts` |
   | a CLI flag or its help | `go test ./cmd/kode-gopher -run TestCLIReference -update` |
   | `sandbox/`, `cmd/sandbox-server`, `internal/prewarm` | `make sandbox-pin`, and commit the overlay |
   | `internal/prewarm`'s dependencies | `go mod tidy` inside `internal/prewarm` |
   | something users notice | a `CHANGELOG.md` entry under `## [Unreleased]`, and the matching page in `docs/site` |

3. **Lint has a complexity ratchet** (funlen, gocognit), pinned to the worst function. Split the function; never raise the limit.
4. **Go changes need an adversarial review** recorded under `## Adversarial review` in the PR body, or `review-gate` fails. For a bug fix, show the regression test fails on the pre-fix *behaviour*, as `CONTRIBUTING.md` describes. A test that fails to compile against old code proves nothing.
5. **Commits are signed off** (`git commit -s`), and the PR title is a Conventional Commit, because squash merge makes it the subject on `main`.
6. **No AI attribution anywhere:** no `Co-authored-by:` naming an AI tool, no "Generated with" footer, in any commit on the branch or in the PR. The `agent attribution` check scans them all.
7. **Every new source file gets the Apache 2.0 header** attributed to Google LLC; copy it from a neighbour.

Open PRs ready for review against `main`, not as drafts, unless one genuinely can't merge yet (a stacked PR). `main` is protected.

## House rules

- **Tests need no cloud.** Unit tests use fakes: `internal/oauth`'s harness has a fake Google, vault, directory and browser. Add to those rather than reaching a real service. The kind end-to-end test (`dev/ci/e2e/kind.sh`) runs real sandboxes without Google credentials.
- **Copy identifiers from tool output.** Commit hashes, run IDs, image digests, resource names: paste them, never type them from memory. Made-up ones have been used here.
- **Never print secrets.** Tokens, client secrets, the OAuth keyring, cookie values: not in logs, test output, PR text or commands you echo. kode-gopher's own logs carry fixed strings and emails, not request data: gosec flags logging request values (G706), and it's right to.
- **Leave other people's clusters and contexts alone.** Pass `KUBECONFIG=` or `CONTEXT=` to scripts; don't switch the current kube context. Don't delete or recreate workloads you didn't create.
- **The deploy script keeps the OAuth keyring** in the `kode-gopher-oauth` Secret. Replacing it invalidates every token the server has issued.
- **Don't tag or release unless asked.** The process is in [`docs/release-process.md`](./docs/release-process.md): annotated tags, and a `CHANGELOG.md` section that becomes the release notes verbatim.

## Foot-guns

- **`internal/prewarm` is its own module.** `go mod tidy` and govulncheck at the root don't cover it; the presubmits do.
- **The sandbox image tag is a hash of its sources.** Editing them without `make sandbox-pin` fails `sandbox-pin`; with it, the GKE overlay points at an image CI publishes only after merge.
- **Docs-site prose can't hard-code tool or package counts or version pins** (`dev/tools/docs-lint`). Name them, or link the generated reference page. Generated pages carry a DO-NOT-EDIT header: regenerate, don't edit.
- **Docs-site links are root-relative** (`[Deploy](/deploy/)`); the base path is added at build time.
- **OAuth flow state lives in sealed envelopes and cookies**, bound to the browser that started sign-in. A change to cookie names, lifetimes or envelope fields breaks sign-ins in progress during a deploy. Say so in the changelog.
- **The shared server's sandboxes reach only an egress allowlist** (`manifests/components/egress-allowlist`). A new Google API host a snippet needs may already be covered by `*.googleapis.com`; anything else needs `EGRESS_ALLOW`.

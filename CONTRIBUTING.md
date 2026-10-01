# Contributing to kode-gopher

kode-gopher follows the same process as [go-steer/core-agent](https://github.com/go-steer/core-agent) and [go-steer/mast](https://github.com/go-steer/mast). Design lives in [`docs/design.md`](./docs/design.md) and [`docs/design-in-cluster.md`](./docs/design-in-cluster.md), the slice plan in [`docs/plan.md`](./docs/plan.md), and decisions with their evidence in [`docs/decisions.md`](./docs/decisions.md).

## Issues

- **Bugs:** [open an issue](https://github.com/gke-demos/kode-gopher/issues/new) with your OS and Go version, the substrate (kind or GKE, and the agent-sandbox version), the snippet or MCP call, and the output.
- **Features:** check [`docs/plan.md`](./docs/plan.md) and the [open issues](https://github.com/gke-demos/kode-gopher/issues) first. File the use case before the proposed solution.

## Pull requests

### Workflow

1. Branch off `main`. Keep the diff focused; unrelated cleanup is a separate PR.
2. Run everything CI runs before pushing:
   ```bash
   make presubmit   # dev/ci/presubmits/all.sh
   ```
   CI ([`.github/workflows/ci.yml`](./.github/workflows/ci.yml)) runs the same scripts under [`dev/ci/presubmits/`](./dev/ci/presubmits), so green locally means green remotely. A new check is a script there, wired into both `all.sh` and a `ci.yml` job.
3. If the change is user-visible (a `feat:` or `fix:`, or anything else a user of the CLI, the MCP server or the manifests would notice), add an entry under `## [Unreleased]` in [`CHANGELOG.md`](./CHANGELOG.md), linking the PR or issue. Those entries become the release notes; see [`docs/release-process.md`](./docs/release-process.md).
4. Open the PR against `main` as ready for review. PRs are squash-merged, so the PR title becomes the commit subject on `main`.

What the presubmits check:

| Check | Script |
|---|---|
| build, vet, `gofmt -s` | `build.sh`, `vet.sh`, `fmt.sh` |
| unit tests with `-race -timeout 5m` | `test.sh` |
| golangci-lint v2.12.1 ([config](./dev/tools/.golangci.yml)) | `lint.sh` |
| `go mod tidy` is a no-op | `mod-tidy.sh` |
| Go pins (go.mod `toolchain`, `sandbox/Dockerfile`, workflows) agree | `go-toolchain.sh` |
| `internal/prompts/` regenerated (`make prompts`) | `prompts.sh` |
| GKE overlay pins the current sandbox image (`make sandbox-pin`) | `sandbox-pin.sh` |
| `dev/release/notes.sh` extracts release notes from `CHANGELOG.md` | `release-notes.sh` |
| no agent attribution | `agent-attribution.sh` |
| govulncheck | `vuln.sh` |

The lint config includes a complexity ratchet (funlen, gocognit) pinned to today's worst function. Lower it when you split that function; never raise it.

### Commit messages: Conventional Commits

Subject lines (and so PR titles) follow [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:`: user-visible new functionality
- `fix:`: user-visible bug fix
- `docs:`: documentation only
- `test:`: tests only
- `refactor:`: neither a feature nor a fix
- `chore:` / `build:` / `ci:`: repo plumbing

An optional scope goes in parentheses, e.g. `feat(mcp): ...` or `fix(sandbox): ...`. Keep the subject under about 70 characters. The body explains why, and what you verified (tests, kind, GKE).

### No AI-agent attribution

Commits and PRs carry no AI-agent attribution:
- no `Co-authored-by:` naming an AI tool;
- no "Generated with" footer;
- no agent as commit author;
- no trailer marking the work as agent-authored.

Author the work under your own name.

The `agent attribution` check ([`.github/workflows/agent-attribution.yml`](./.github/workflows/agent-attribution.yml)) scans every commit on your branch plus the PR title and body. It checks every commit, not just the last, because a squash merge copies each branch commit's co-authors into `main`. Many AI coding tools add a co-author trailer by default, so turn that off. A human co-author is fine.

### License headers

Every source file carries the Apache 2.0 header attributed to Google LLC; copy it from any existing file. golangci-lint's `goheader` linter enforces it on `.go` files.

### Sandbox image

The sandbox image (`sandbox/`, `cmd/sandbox-server`, `internal/prewarm`) is tagged by a hash of its sources. If you change any of them, run `make sandbox-pin` and commit the updated `manifests/overlays/gke/kustomization.yaml`. CI publishes the image from `main`.

## License

By contributing you agree that your contributions are licensed under the [Apache License 2.0](./LICENSE).

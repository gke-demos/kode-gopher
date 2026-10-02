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
4. If the PR touches Go code, run the [adversarial review](#adversarial-review) and record it in the PR body under an `## Adversarial review` heading.
5. Sign off your commits ([DCO](#developer-certificate-of-origin-dco)).
6. Open the PR against `main` as ready for review. PRs are squash-merged, so the PR title becomes the commit subject on `main`.

`main` is protected: a PR merges only once the required checks are green (`test`, `lint`, `hygiene`, `agent attribution` and `review-gate`).

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

PRs that touch Go code, the sandbox image, `manifests/base` or `testdata/` also run an end-to-end test on a kind cluster ([`.github/workflows/e2e-kind.yml`](./.github/workflows/e2e-kind.yml)). It has no Google credentials, so it covers what runs without them: `kode-gopher serve`, over stdio and over streamable HTTP, building and running snippets in real agent-sandbox sandboxes. It takes a few minutes, needs docker and kind, and isn't part of `make presubmit`. To run it locally:

```bash
dev/ci/e2e/kind.sh   # KEEP=1 keeps the cluster, BUILD=1 builds the sandbox image
```

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

### Adversarial review

Before opening a PR that touches Go code, run a skeptical review of the diff, usually with a subagent told to find what's wrong:
- correctness;
- races;
- API misuse, checked against the dependency's real source, not memory.

Fix or pin every finding, and record the outcome in the PR body under an `## Adversarial review` heading.

For a bug fix, also **verify the regression test fails on the pre-fix code.** A test that passes on the buggy code documents the fix but doesn't guard it. Don't do this by reverting files or checking out the parent commit: once the fix adds a symbol, the test no longer compiles against old sources, and a compile error says nothing about the assertion. Instead:
1. Copy the production files aside.
2. Patch the old *behavior* back into the new code, keeping every new symbol so the package still builds, and mark each site `// PREFIX BEHAVIOUR`.
3. Run the tests, and record the failures in the PR.
4. Restore the files, and check that no marker remains.

A test that passes both before and after by design (a pure function, a boundary check) is fine: say so in the PR.

The `review-gate` check ([`.github/workflows/review-gate.yml`](./.github/workflows/review-gate.yml)) fails Go-touching PRs whose body has no "Adversarial review" section; PRs without Go changes pass. For a local reminder, merge [`dev/claude/settings-review-gate.json`](./dev/claude/settings-review-gate.json) into your untracked `.claude/settings.json`: it blocks `gh pr create` without the section. The process and the check come from go-steer/core-agent.

### Developer Certificate of Origin (DCO)

All commits must be **signed off** under the [Developer Certificate of Origin](https://developercertificate.org/). The DCO is a lightweight statement that you wrote the change, or have the right to submit it under the project's Apache 2.0 license. It's a `Signed-off-by:` trailer in the commit message, not a cryptographic signature.

Sign off by passing `-s` to `git commit`:

```bash
git commit -s -m "fix(mcp): ..."
```

That appends:

```
Signed-off-by: Your Name <you@example.com>
```

The name and email must match your `git config user.name` and `user.email`. If you forget, amend with `git commit --amend -s` (one commit), or rebase with `-x 'git commit --amend -s --no-edit'` (several). This matches go-steer/core-agent and k8s-lookout. No check enforces it.

If an AI coding assistant writes commits for you, it commits under your name, so its sign-off is your statement. Let it sign off only on work you're submitting as your own.

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

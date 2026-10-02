# Release process

kode-gopher releases follow the same process as [go-steer/core-agent](https://github.com/go-steer/core-agent) and [go-steer/mast](https://github.com/go-steer/mast). Pushing a `v*` tag runs [`.github/workflows/release.yml`](../.github/workflows/release.yml), which publishes a GitHub Release with:

- `kode-gopher` binaries for linux and darwin on amd64 and arm64, built by GoReleaser ([`.goreleaser.yaml`](../.goreleaser.yaml)) with `CGO_ENABLED=0`, one `tar.gz` per platform;
- `checksums.txt`, signed with Sigstore cosign keyless (`checksums.txt.sigstore.json`, a bundle with the signature and certificate);
- notes taken from [`CHANGELOG.md`](../CHANGELOG.md).

The sandbox image is not part of this. CI publishes it from `main` under a content-derived tag, which the GKE overlay pins (see [`CONTRIBUTING.md`](../CONTRIBUTING.md#sandbox-image)).

## Versions

Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html), and tags carry a `v` prefix: `v0.1.0`.

- **Pre-1.0**, a minor bump (`v0.2.0`) may break the CLI flags, the MCP tool contract or the manifests. Patch releases (`v0.2.1`) are fixes only.
- **Release candidates** take an `-rc.N` suffix: `v0.2.0-rc.1`, `v0.2.0-rc.2`. GoReleaser marks any tag with a pre-release suffix as a GitHub Pre-release (`prerelease: auto`), so it doesn't become "Latest".

The version a binary reports comes from [`internal/version`](../internal/version/version.go). Both `kode-gopher version` and the MCP server's `Implementation.Version` read it.

- **Release builds**: GoReleaser sets `Version`, `Commit` and `Date` with `-ldflags`. The version is the tag.
- **`go install github.com/gke-demos/kode-gopher/cmd/kode-gopher@v0.1.0`**: reports the module version Go records.
- **Builds from a checkout** (`go build`): report Go's pseudo-version and the VCS revision, with `modified` for a dirty tree.

## Release notes

Release notes are the `CHANGELOG.md` section for the version, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format:

- **Every pull request** with a user-visible change adds an entry under `## [Unreleased]` (see [`CONTRIBUTING.md`](../CONTRIBUTING.md)).
- **When you cut a release**, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, and add a fresh, empty `## [Unreleased]` above it.
- **The release workflow** runs [`dev/release/notes.sh vX.Y.Z`](../dev/release/notes.sh). It prints that section, which becomes the GitHub Release body verbatim.
  - If there is no `## [X.Y.Z]` heading, it falls back to the `## [Unreleased]` section and says so at the top. A tag cut before the CHANGELOG was rolled still gets real notes.
  - If both are empty, it fails and nothing is published.

Run `dev/release/notes.sh vX.Y.Z` locally to see exactly what will be published. The `release-notes.sh` presubmit tests the script and checks that every version in `CHANGELOG.md` yields its own notes.

GoReleaser's own git-log changelog is never used. It is not turned off with `changelog: disable: true`, though: GoReleaser reads the `--release-notes` file inside its changelog pipe, so disabling the pipe would silently drop our notes. mast published two releases with empty bodies that way. Passing `--release-notes` already stops GoReleaser from generating its own changelog. After publishing, the workflow also checks that the Release body is not empty.

## Cut a release

1. **Roll the CHANGELOG.** In a PR, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` (for a release candidate, `## [X.Y.Z-rc.N] - YYYY-MM-DD`), add an empty `## [Unreleased]` above it, and tidy the entries. Check the notes:
   ```bash
   dev/release/notes.sh vX.Y.Z
   ```
   Merge the PR.
2. **Tag the merge commit on `main` and push the tag:**
   ```bash
   git fetch origin
   git tag vX.Y.Z origin/main
   git push origin vX.Y.Z
   ```
3. **Watch the `release` workflow** on the Actions tab. It runs every presubmit, extracts the notes, runs GoReleaser and checks the published body. Nothing is published if any presubmit fails.
4. **Check the Release page**:
   - the body is the CHANGELOG section;
   - there are four archives, plus `checksums.txt` and `checksums.txt.sigstore.json`;
   - "Latest" is shown only for a non-pre-release tag.

Re-running the workflow against the same tag replaces the Release's body and assets (`mode: replace`). If the notes were wrong, fix `CHANGELOG.md` on `main` and edit the Release body by hand: don't move a published tag.

## Dry run

Build and sign everything without publishing, verify the signature, and download `dist/` as a workflow artifact. Signing in the dry run catches signing breakage before a tag is pushed; it adds an entry to Sigstore's public transparency log, as any signature does.

```bash
gh workflow run release.yml -f dry_run=true
```

Run from a branch, it uses the `## [Unreleased]` notes, so `[Unreleased]` must not be empty.

Locally, with [GoReleaser](https://goreleaser.com/install/) v2:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign
./dist/kode-gopher_linux_amd64_v1/kode-gopher version
```

## Verify a release

`checksums.txt` is signed keyless by the release workflow's GitHub OIDC identity. Download the archive you need plus `checksums.txt` and `checksums.txt.sigstore.json`, then, with cosign 2.4 or later:

```bash
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/gke-demos/kode-gopher/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --check --ignore-missing checksums.txt
```

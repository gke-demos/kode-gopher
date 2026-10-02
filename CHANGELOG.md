# Changelog

All notable changes to kode-gopher are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Every pull request with a user-visible change adds an entry under `## [Unreleased]`. Each release's section becomes its GitHub Release notes verbatim; see [`docs/release-process.md`](./docs/release-process.md).

## [Unreleased]

## [0.1.1] - 2026-10-02

The first published release. It contains everything in 0.1.0 below, plus the release-signing fix.

### Fixed

- **Release signing works with cosign 3.** The `checksums.txt` signature is now a Sigstore bundle, `checksums.txt.sigstore.json`, replacing `checksums.txt.sig` and `checksums.txt.pem`. Verify it with `cosign verify-blob --bundle`; see [docs/release-process.md](./docs/release-process.md). The v0.1.0 release failed at this step before anything was published. The release workflow's dry run now signs and verifies too.

## [0.1.0] - 2026-10-02

Tagged, but there's no GitHub Release: the release workflow failed at signing before publishing anything (fixed in 0.1.1). The server image `ghcr.io/gke-demos/kode-gopher/server:0.1.0` was published from this tag.

The first release. kode-gopher runs Go that an AI agent writes against the real Google Cloud SDKs, in a sandboxed Kubernetes pod, and returns a structured result. It runs locally over stdio, or as a shared server in GKE with Google sign-in. Pre-alpha: flags, the MCP tool contract and the manifests may change in 0.x releases. Documentation: https://gke-demos.github.io/kode-gopher/. Slice details are in [`docs/plan.md`](./docs/plan.md), and the evidence behind them in [`docs/decisions.md`](./docs/decisions.md).

### Added

- **Documentation site.** https://gke-demos.github.io/kode-gopher/ covers getting started, deploying for a team, concepts and reference. The MCP tools, CLI and precompiled packages reference pages are generated from the code, with drift tests.
- **Output field descriptions.** `execute_go_code`'s output schema now describes every field, including `phase`, `tidied` and the result kinds, so clients and models know what they mean.
- **Service identity and client credentials (slice 9 step 5).** `serve --credentials=service --service-account=<gsa>` runs every snippet as a Google service account, impersonated through the IAM Credentials API. Under `--auth=oauth`, a pre-registered client with a `service_account` gets tokens through the OAuth `client_credentials` grant, for CI and automation, and its snippets run as that account. `gcp_auth_status` reports `mode=service`. The unused `creds.Workload` stub is gone.
- **In-cluster deployment (slice 9 step 4).** `manifests/overlays/gke-server` runs `kode-gopher serve --transport=http` in the cluster behind a GKE Gateway (global external Application LB with a Google-managed certificate), with its own Role and network policy; `gke-server-oauth` switches it to Google sign-in. `scripts/deploy-gke-server.sh` deploys either and `scripts/smoketest-http.sh` tests it. The server image (`server/Dockerfile`) is published to `ghcr.io/gke-demos/kode-gopher/server` for amd64 and arm64, signed with cosign. The GKE overlay now denies all in-cluster ingress to the sandbox-router.
- **Release process.** `kode-gopher version` and the MCP server's `Implementation.Version` report the build version, which GoReleaser stamps on tagged releases (linux and darwin, amd64 and arm64, with a cosign-signed `checksums.txt`). Release notes come from this file. ([#12](https://github.com/gke-demos/kode-gopher/issues/12))
- **OAuth 2.1 sign-in (slice 9 step 3).** `serve --auth=oauth` makes kode-gopher an OAuth 2.1 authorization server that fronts Google sign-in, with PKCE, resource indicators, CIMD, DCR and pre-registered clients. Access is allow-listed by Google group and domain, and snippets run as the signed-in user.
- **HTTP transport (slice 9 step 2).** `serve --transport=http` serves streamable HTTP, with one sandbox per MCP session, claim leases so a crashed server's sandboxes expire, and a per-user session cap.
- **In-cluster mode (slice 9 step 1).** `--in-cluster` dials sandboxes by their Service. `--credentials=access-token` serves a short-lived token to the run only, through a GCE metadata emulator in the sandbox server.
- **agent-sandbox v1.0 (slice 8).** v1.0.4 client, v1beta1 manifests shared by kind and GKE, claims on the `go-runtime-pool` warm pool, and the upstream Go sandbox-router. Sandboxes mount no Kubernetes service account token.
- **Fast compiled path (slice 7).** kode-gopher's own sandbox image and in-pod server, published by CI under a content-derived tag that the GKE overlay pins. `go mod tidy` runs only when the prewarmed lockfile misses an import. A GCS snippet runs in about 4.4 s end to end on GKE with gVisor.
- **Production hardening (slice 4).** The `gcp_auth_status` and `lookup_package_docs` tools, multi-file snippets, a unified credentials source, `--context` on `exec` and `serve`, sessions that recover from a dead sandbox, and an LLM prompt generated from the curated package set.
- **MCP server (slice 2).** `kode-gopher serve` exposes `execute_go_code` over stdio.
- **CLI and result contract (slices 0 and 1).** `kode-gopher exec <file.go>` normalizes a snippet or a full `package main` program, builds and runs it in an agent-sandbox pod, and returns a discriminated structured result. Verified on kind and on GKE Autopilot with gVisor.

### Security

- **Sandboxes no longer see the namespace's Services in their environment.** The sandbox template sets `enableServiceLinks: false`, so snippets no longer get `KODE_GOPHER_SERVICE_HOST`, `SANDBOX_ROUTER_SVC_SERVICE_HOST` and the like. Those addresses were already blocked by network policy. The kubelet still sets `KUBERNETES_SERVICE_HOST`.

### Fixed

- **The model is no longer told credentials arrive as a forwarded ADC file.** The `execute_go_code` description and the system prompt now say that the Google Cloud libraries find credentials on their own, and that the identity depends on the deployment. In-cluster deployments mount no credentials file. ([#28](https://github.com/gke-demos/kode-gopher/issues/28))
- **Large results no longer vanish.** A snippet's result over 16 KiB was cut by the same truncation as stdout, failed to parse, and was silently dropped, so `execute_go_code` reported `exit_code: 0` with no `result`. Results now come back whole up to 256 KiB. A larger result, or a `result.json` that isn't valid JSON, comes back as a warning that says why, and the call is flagged as an error.
- With `--auth=static`, a 401 from `/mcp` now carries `WWW-Authenticate: Bearer`, as RFC 6750 requires.

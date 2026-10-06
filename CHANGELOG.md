# Changelog

All notable changes to kode-gopher are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Every pull request with a user-visible change adds an entry under `## [Unreleased]`. Each release's section becomes its GitHub Release notes verbatim; see [`docs/release-process.md`](./docs/release-process.md).

## [Unreleased]

### Added

- **Group allow lists work on GKE.** `serve --oauth-groups-service-account` (`GROUPS_SERVICE_ACCOUNT` for the deploy script) checks `--oauth-allow-groups` as a service account that kode-gopher impersonates for that call. That service account holds the Groups Reader admin role. kode-gopher's own Workload Identity principal has no email, so it can't be assigned an admin role. ([#36](https://github.com/gke-demos/kode-gopher/issues/36))

## [0.3.1] - 2026-10-05

A security patch: the sandbox image's dependencies are updated to fix six known vulnerabilities. Redeploy to pick up the new sandbox image; the deploy script replaces the warm pool's old sandboxes itself.

### Security

- **The sandbox image's dependencies are patched and scanned.** The prewarmed modules every snippet builds against carried six known vulnerabilities (gRPC, OpenTelemetry SDK, `golang.org/x/text`). They're bumped to fixed versions: gRPC v1.83.2, OpenTelemetry v1.45.0, `x/text` v0.41.0. CI now runs govulncheck (package level, with the image's Go version) and `go mod tidy -diff` on `internal/prewarm` too. Before, it only checked the root module.

## [0.3.0] - 2026-10-03

Sandboxes on a shared server now reach only an allowlist of hosts, and deploys are more reliable. **Upgrading a shared server needs one cluster change first:** `gcloud container clusters update <cluster> --location <location> --enable-fqdn-network-policy`. On Autopilot, GKE then restarts the nodes over a few hours. Documentation: https://gke-demos.github.io/kode-gopher/.

### Security

- **Sandboxes on a shared server reach only an allowlist of hosts.** They used to reach any public IP address. The `gke-server` overlays now allow DNS, plus port 443 to `*.googleapis.com`, the Go module proxy and the cluster region's GKE control-plane endpoints, through a GKE FQDN network policy (`manifests/components/egress-allowlist`). `EGRESS_ALLOW` adds hosts. The cluster needs `--enable-fqdn-network-policy`, and `scripts/deploy-gke-server.sh` checks for it. Local use (kind, the plain `gke` overlay) is unchanged.

### Fixed

- **Secret changes now reach kode-gopher on redeploy.** It reads its Secret only at startup, and the deploy script didn't restart it when only the Secret changed. So a rotated static token, a new `CLIENTS_FILE` or a new Google client file had no effect until something else changed the pod. The deploy script now hashes the Secret into the pod template.
- **A redeploy without `CLIENTS_FILE` keeps the pre-registered clients.** It used to reset them to none, unlike the keyring and Google client file, which it kept.
- **Deploying a new sandbox image reaches new sessions straight away.** agent-sandbox doesn't replace a warm pool's unclaimed sandboxes when the template changes, so after an upgrade new sessions kept getting old-image sandboxes. `scripts/deploy-gke-server.sh` now replaces them when their image differs from the template's, and waits for one on the new image, through the new `scripts/refresh-warm-pool.sh`. The kind scripts use it too. ([#41](https://github.com/gke-demos/kode-gopher/issues/41))

## [0.2.0] - 2026-10-02

Precompiled clients for troubleshooting, and local sandboxes that no longer outlive a killed process. Documentation: https://gke-demos.github.io/kode-gopher/.

### Added

- **Cloud Logging, Monitoring and Trace clients are precompiled** in the sandbox image (`cloud.google.com/go/logging/logadmin`, `monitoring/apiv3/v2`, `trace/apiv1`), so troubleshooting snippets build in seconds without `go mod tidy`. `lookup_package_docs` now serves docs for any package in a precompiled module, including the `*pb` request types (`monitoringpb`, `tracepb`) that their calls need, and the model is told those packages are compiled too. ([#27](https://github.com/gke-demos/kode-gopher/issues/27))

### Fixed

- **A killed local kode-gopher no longer leaks its sandbox.** Sandbox claims from `serve` over stdio and from `exec` now carry the same renewed lease as the HTTP server's (`--claim-lease`, 10 minutes by default). A process that dies without a clean shutdown loses its sandbox within one lease, instead of holding it forever. `serve --persistent` and `exec --keep` stay lease-free, so their sandboxes outlive the process. Local users need permission to patch SandboxClaims. ([#34](https://github.com/gke-demos/kode-gopher/issues/34))

## [0.1.1] - 2026-10-02

The first published release of kode-gopher: an MCP server (and CLI) that runs Go an AI agent writes against the real Google Cloud SDKs in a sandboxed Kubernetes pod, locally over stdio or as a shared server in GKE with Google sign-in. Pre-alpha. It's 0.1.0, whose release failed before publishing, plus the signing fix below; the full feature list is in [the 0.1.0 notes](https://github.com/gke-demos/kode-gopher/blob/main/CHANGELOG.md#010---2026-10-02). Documentation: https://gke-demos.github.io/kode-gopher/.

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

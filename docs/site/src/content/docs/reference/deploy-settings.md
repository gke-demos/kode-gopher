---
title: Deployment settings
description: The environment variables and flags of scripts/deploy-gke-server.sh.
---

`scripts/deploy-gke-server.sh` deploys kode-gopher to a GKE cluster; see [Deploy for a team](/deploy/). Its options are environment variables. `scripts/deploy-gke-server.sh --help` prints the same list from the script itself.

## Flags

| Flag | Meaning |
|---|---|
| `--auth static` or `--auth oauth` | How clients authenticate: one shared token (the default) or [Google sign-in](/deploy/google-sign-in/). |
| `--dry-run` | Print the rendered manifests and apply nothing. |

## All modes

| Variable | Default | Meaning |
|---|---|---|
| `KUBECONFIG`, `CONTEXT` | current context | The cluster to deploy to. `CONTEXT` is passed as `--context`, so your current context is unchanged. |
| `PROJECT` | `gcloud config get-value project` | Project for the static IP and the certificate. |
| `IMAGE` | `ghcr.io/gke-demos/kode-gopher/server:main` | The server image. Pin a `main-<short-sha>` or version tag for a stable deployment. |
| `EGRESS_ALLOW` | none | Extra hosts sandboxes may reach on port 443, comma-separated: `api.example.com,*.example.org`. A `*.` pattern matches one label. Always allowed: `*.googleapis.com`, `proxy.golang.org`, `sum.golang.org` and `*.<cluster-region>.gke.goog`. |
| `KG_HOST` | `<ip-with-dashes>.sslip.io` | Public hostname. For your own domain, point a DNS A record at the IP; see [Operations](/deploy/operations/#your-own-domain). |

## Static token only

| Variable | Default | Meaning |
|---|---|---|
| `SERVICE_ACCOUNT` | none | Run every snippet as this Google service account (`--credentials=service`) instead of kode-gopher's own identity. kode-gopher needs `roles/iam.serviceAccountTokenCreator` on it. |

## Google sign-in only

| Variable | Default | Meaning |
|---|---|---|
| `GOOGLE_CLIENT_FILE` | none (required on first deploy) | The Google OAuth client JSON downloaded from the console. |
| `ALLOW_DOMAINS` | none | Comma-separated Workspace domains to admit, matched against the ID token's `hd` claim. |
| `ALLOW_GROUPS` | none | Comma-separated Google group emails to admit. Nested membership counts. |
| `GROUPS_SERVICE_ACCOUNT` | none | The service account that checks `ALLOW_GROUPS`: it holds the Groups Reader admin role, and kode-gopher impersonates it. See [Groups](/deploy/google-sign-in/#groups). |
| `CUSTODY` | `vault` | Where users' Google grants are kept: `vault` (Google's Agent Identity credential vault) or `sealed` (inside kode-gopher's own refresh tokens). |
| `VAULT_AUTH_PROVIDER` | none | With vault custody: `projects/<p>/locations/<l>/authProviders/<name>`. |
| `CLIENTS_FILE` | the existing Secret's | Pre-registered clients, a JSON array. Without it, a redeploy keeps the clients already deployed. A client with a `service_account` uses the `client_credentials` grant; see [Clients](/deploy/clients/). |
| `OPEN_REGISTRATION` | `true` | Accept clients that register themselves (DCR, Client ID Metadata Documents), not only pre-registered ones. |

At least one of `ALLOW_DOMAINS` and `ALLOW_GROUPS` must be set: an empty allow-list admits no one.

## What it creates

| Resource | Notes |
|---|---|
| Global static IP `kode-gopher-ip` | Kept across deploys. |
| Google-managed certificate `kode-gopher-cert` | For `KG_HOST`. The script stops if it exists for a different hostname. |
| Secret `kode-gopher-token` | Static token: a random token, created only if missing. |
| Secret `kode-gopher-oauth` | Google sign-in: the client JSON, the token-sealing keyring (created only if the Secret is new), and `CLIENTS_FILE`. |
| Manifests in `codemode` | From `manifests/overlays/gke-server` or `gke-server-oauth`, with the API server's address, the image and the OAuth flags filled in. |

The server's own flags are on the [CLI](/reference/cli/#kode-gopher-serve) page.

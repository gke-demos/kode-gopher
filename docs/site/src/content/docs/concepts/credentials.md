---
title: Credentials
description: Who snippets run as, and how credentials reach the sandbox in each mode.
sidebar:
  order: 2
---

Snippets call Google Cloud with ordinary Application Default Credentials (ADC). The Google client libraries find credentials on their own, so a snippet never handles a token. What differs between deployments is whose credentials those are, and how they reach the sandbox. `gcp_auth_status` always reports the answer.

## Modes

| `gcp_auth_status` mode | When | Snippets run as | How the sandbox gets credentials |
|---|---|---|---|
| `forwarded` | Local use, the default | You | Your ADC file is copied into the sandbox for each run, with `GOOGLE_APPLICATION_CREDENTIALS` pointing at it. |
| `access-token` | Shared server, static token | kode-gopher's own Workload Identity | A short-lived token, served by a metadata server emulator in the sandbox. |
| `service` | `--credentials=service`, or a client-credentials client | A Google service account | A short-lived token for that account, minted through the IAM Credentials API and served the same way. |
| `oauth` | Shared server, Google sign-in | The signed-in user | The user's own short-lived token, served the same way. |
| `none` | No credentials configured | Nobody | Nothing. Google API calls fail. |

## The metadata server emulator

On a shared server, no credential file ever enters the sandbox. The sandbox's own server answers the standard GCE metadata endpoints that the Google libraries query. For each run, kode-gopher hands it one access token, which it serves only for that run, along with the project ID. Consequences:
- A snippet can't find a refresh token or a key to copy, only an access token that expires within the hour.
- One user's token is never visible to another user's run, because each MCP session has its own sandbox.
- The real GKE metadata server is blocked from the sandbox, so snippets can't reach the node's identity either.

## Where tokens come from on a shared server

- **Static token, default:** kode-gopher mints tokens from its own ADC, which is its Workload Identity. Grant that principal what snippets need.
- **Service account:** kode-gopher calls `generateAccessToken` for the account. It needs `roles/iam.serviceAccountTokenCreator` on that account, and nothing project-wide.
- **Google sign-in:** the user's token comes from their sign-in. With vault custody, Google's Agent Identity credential vault holds the grant and kode-gopher retrieves fresh tokens from it. kode-gopher checks that every token from the vault belongs to the signed-in user and carries `cloud-platform`. With sealed custody, kode-gopher refreshes the token from the user's Google refresh token, which is sealed inside kode-gopher's own refresh token.

## Reaching GKE clusters

Sandboxes mount no Kubernetes service account token, so `rest.InClusterConfig()` fails, and cluster-internal addresses are blocked. A snippet reaches any GKE cluster, including the one it runs in, the same way it reaches any Google API:
1. Look the cluster up with `container/apiv1`.
2. Connect to its DNS endpoint (`ControlPlaneEndpointsConfig.DnsEndpointConfig.Endpoint`), which works even for private clusters.
3. Authenticate with an `oauth2.Transport` from `google.DefaultTokenSource`.

What the snippet may then do in the cluster is whatever Kubernetes RBAC grants the caller's Google identity. The repository's `testdata/list_gke_pods_snippet.go` is a worked example.

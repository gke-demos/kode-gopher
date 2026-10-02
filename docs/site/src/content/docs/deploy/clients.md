---
title: Clients
description: Connect MCP clients to a deployment with Google sign-in, pre-register clients, and give CI its own credentials.
sidebar:
  order: 3
---

## Claude Code

```bash
claude mcp add --transport http kode-gopher https://<host>/mcp
```

Then run `/mcp` and authenticate. Claude Code receives its tokens on a `localhost` callback, the standard pattern for OAuth clients that run on your machine. That means the browser must reach the machine Claude Code runs on.

### Claude Code on a remote machine

When Claude Code runs over SSH or on a Cloud Workstation, the final redirect to `http://localhost:<port>/callback` lands on your laptop instead. Pin the port, then do one of these:

```bash
claude mcp add --transport http --callback-port 33418 kode-gopher https://<host>/mcp
```

- **Forward the port** from your laptop to the remote machine: `ssh -L 33418:localhost:33418 <host>`, or on Cloud Workstations, `gcloud workstations start-tcp-tunnel ... <workstation> 33418 --local-host-port=localhost:33418`.
- **Rewrite the URL.** When the browser fails to load `http://localhost:33418/callback?code=...`, replace just `http://localhost:33418` with an address that reaches the remote machine's port 33418, such as the Cloud Workstations port proxy `https://33418-<workstation-host>`, and keep the rest of the URL.

## MCP Inspector

Connect to `https://<host>/mcp` with the Streamable HTTP transport, and use its OAuth flow.

## Pre-registered clients

By default, any MCP client can register itself, by dynamic client registration or a Client ID Metadata Document. Before Google sign-in, each user sees a kode-gopher page that names the client and asks them to confirm.

To accept only clients you list, set `OPEN_REGISTRATION=false` and pass `CLIENTS_FILE`, a JSON array:

```json
[
  {"client_id": "inspector", "client_name": "MCP Inspector", "redirect_uris": ["http://localhost:6274/oauth/callback"]}
]
```

Pre-registered clients skip the confirmation page.

## CI and automation: client credentials

A client with a `service_account` is a client-credentials client. It gets access tokens with its secret, no browser, and its snippets run as that service account:

```json
[
  {"client_id": "ci", "client_secret": "<random>", "client_name": "CI", "service_account": "runner@<PROJECT_ID>.iam.gserviceaccount.com"}
]
```

```bash
curl -u ci:<secret> -d grant_type=client_credentials https://<host>/token
```

Rules for these clients:
- **No redirect URIs:** they can't sign users in.
- **Access tokens only:** there's no refresh token, so the client asks again with its secret.
- **Scope and resource:** `scope` may only be `kode-gopher:execute`, which is also the default, and `resource`, if sent, must be the MCP endpoint.
- **Revocation:** removing the client from the file, or changing its account, revokes its tokens at the next request.
- **IAM:** kode-gopher needs `roles/iam.serviceAccountTokenCreator` on the account, as in [service-account mode](/deploy/static-token/#or-as-a-service-account).

In Go, the MCP SDK's `extauth.ClientCredentialsHandler` speaks this grant. To check it end to end, run `scripts/smoketest-http.sh --client-credentials <file>` with a file holding `{"client_id", "client_secret"}`.

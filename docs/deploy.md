# Deploying kode-gopher in a GKE cluster

This guide runs kode-gopher as a shared MCP server in a GKE cluster, behind a GKE Gateway with a Google-managed certificate. MCP clients connect over streamable HTTP at `https://<host>/mcp`. Each MCP session gets its own sandbox, in the same cluster.

To run kode-gopher on your own machine over stdio instead, see [getting-started.md](./getting-started.md). The design behind this deployment is in [design-in-cluster.md](./design-in-cluster.md).

## Choose how clients authenticate

| | Static token (`--auth=static`) | Google sign-in (`--auth=oauth`) |
|---|---|---|
| Clients send | one shared bearer token | a kode-gopher OAuth token, from a browser sign-in with Google |
| Snippets run as | kode-gopher's own Workload Identity, or a service account you name | each signed-in user, with their own Google token |
| Who can use it | anyone with the token | users admitted by Workspace domain or Google group |
| Setup | a few minutes | a Google OAuth client, and optionally the Agent Identity credential vault |
| Good for | trying it out, single-tenant tools, demos | teams |

Under Google sign-in, CI jobs and other automation can also use the OAuth `client_credentials` grant, and their snippets run as a service account.

Whichever you choose, the sandbox never receives a long-lived credential. Each run gets a short-lived access token from a metadata server emulator inside the sandbox. No ADC file and no Kubernetes service account token are mounted.

## Prerequisites

- **A GKE Autopilot cluster with the agent-sandbox addon,** and the `codemode` namespace:
  ```bash
  gcloud container clusters create-auto kode-gopher \
    --location=us-central1 --release-channel=rapid --enable-agent-sandbox
  kubectl create namespace codemode
  ```
  Autopilot includes Workload Identity, gVisor and the Gateway API.
- **This repository,** since the deploy script applies its manifests: `git clone https://github.com/gke-demos/kode-gopher`.
- **`kubectl`, `gcloud` and `python3`.** You need permission to create a global static IP address and an SSL certificate in the project, and to apply manifests in `codemode`.

kode-gopher's Kubernetes service account is `codemode/kode-gopher`. Google IAM refers to it as this Workload Identity principal, used in the grants below:

```
principal://iam.googleapis.com/projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/<PROJECT_ID>.svc.id.goog/subject/ns/codemode/sa/kode-gopher
```

Get the project number with `gcloud projects describe <PROJECT_ID> --format='value(projectNumber)'`.

## What the deploy script does

`scripts/deploy-gke-server.sh` is safe to rerun, and every option is an environment variable (`scripts/deploy-gke-server.sh --help`). It never prints secrets. It:

1. creates the global static IP `kode-gopher-ip` if it's missing. The public hostname defaults to `<ip-with-dashes>.sslip.io`, a wildcard DNS name that resolves to that IP;
2. creates the Google-managed certificate `kode-gopher-cert` for that hostname if it's missing;
3. creates the Secret the auth mode needs, keeping an existing one;
4. applies `manifests/overlays/gke-server`, or `gke-server-oauth` with `--auth=oauth`. These contain:
   - the kode-gopher Deployment (one replica), Service, Role and network policy;
   - the Gateway and HTTPRoute;
   - the sandbox template, warm pool and router from `manifests/overlays/gke`.

The certificate turns ACTIVE once the hostname resolves to the IP and the load balancer is up, usually within an hour of its creation. Until then, use `--port-forward` with the smoketest.

Pass `CONTEXT=<kubectl context>` to target a cluster other than the current context, and `--dry-run` to print the manifests without applying anything.

**The server image.** The default is `ghcr.io/gke-demos/kode-gopher/server:main`, which CI rebuilds on every merge, for amd64 and arm64, signed with cosign. For a deployment that doesn't change under you, set `IMAGE` to a fixed tag: `main-<short-sha>` now, or a version tag once releases are cut.

## Option 1: a static token

```bash
scripts/deploy-gke-server.sh
```

The token is in the Secret `kode-gopher-token`. To connect Claude Code:

```bash
TOKEN=$(kubectl -n codemode get secret kode-gopher-token -o jsonpath='{.data.token}' | base64 -d)
claude mcp add --transport http kode-gopher https://<host>/mcp --header "Authorization: Bearer $TOKEN"
```

Anyone with the token can run code as the identity below. Share it like a password.

**Who snippets run as.** By default they run as kode-gopher's own Workload Identity (`--credentials=access-token`). That principal starts with no roles, so grant it what your snippets need, for example:

```bash
gcloud projects add-iam-policy-binding <PROJECT_ID> --role=roles/viewer --member=<principal>
```

**Or as a service account.** To run every snippet as a specific Google service account, allow kode-gopher to mint tokens for that account only, then deploy with `SERVICE_ACCOUNT`:

```bash
gcloud iam service-accounts add-iam-policy-binding <sa-email> \
  --role=roles/iam.serviceAccountTokenCreator --member=<principal>
SERVICE_ACCOUNT=<sa-email> scripts/deploy-gke-server.sh
```

kode-gopher calls the IAM Credentials API to mint each token, so the API must be enabled (`gcloud services enable iamcredentials.googleapis.com`). `gcp_auth_status` then reports `mode=service` and the account's email.

## Option 2: Google sign-in

kode-gopher becomes an OAuth 2.1 authorization server for MCP clients, and fronts Google sign-in. A client that connects without a token gets pointed at kode-gopher's metadata, registers itself, and opens a browser. The user signs in with Google, and from then on the client holds kode-gopher tokens. Snippets run as the user, with a Google access token that kode-gopher keeps fresh.

### 1. Create a Google OAuth client

In the Cloud console, under **Google Auth Platform**:

1. **Branding / Audience:** use an **Internal** consent screen in your Workspace organization, so the `cloud-platform` scope needs no app verification.
2. **Clients:** create a **Web application** client. Mark it as used by an AI-powered agent if the console asks. Add these **Authorized redirect URIs**:
   - `https://<host>/callback`
   - with vault custody (below), also the auth provider's callback: `https://agentidentitycredentials.googleapis.com/v1/projects/<PROJECT_ID>/locations/<LOCATION>/authProviders/<NAME>/oauthcallback`
3. Download the client's JSON. That file is `GOOGLE_CLIENT_FILE`.

To know `<host>` before the first deploy, create the IP yourself. The script reuses it:

```bash
gcloud compute addresses create kode-gopher-ip --global
gcloud compute addresses describe kode-gopher-ip --global --format='value(address)'   # 203.0.113.7 -> 203-0-113-7.sslip.io
```

If your organization restricts access to Google Cloud scopes, sign-in fails with "Access blocked: your institution's admin needs to review" until a Workspace admin marks the client as Trusted.

### 2. Choose where users' Google grants are kept

- **Vault (default, recommended).** Google's Agent Identity credential vault holds each user's grant. Google runs its own consent for `cloud-platform`, and kode-gopher's tokens carry no Google secret. Revocation and audit happen on Google's side.
- **Sealed (`CUSTODY=sealed`).** kode-gopher keeps each user's Google refresh token, encrypted inside the kode-gopher refresh token it issues. There's no extra setup, which makes it good for a first deploy.

To set up the vault:

```bash
gcloud services enable agentidentity.googleapis.com

gcloud agent-identity auth-providers create kode-gopher \
  --location=<LOCATION> \
  --three-legged-oauth-client-id=<client-id> \
  --three-legged-oauth-client-secret=<client-secret> \
  --three-legged-oauth-authorization-url='https://accounts.google.com/o/oauth2/v2/auth?access_type=offline&prompt=consent' \
  --three-legged-oauth-token-url=https://oauth2.googleapis.com/token \
  --three-legged-oauth-default-continue-uri=https://<host>/consent/continue \
  --three-legged-oauth-enable-pkce \
  --allowed-scopes=openid,https://www.googleapis.com/auth/userinfo.email,https://www.googleapis.com/auth/cloud-platform \
  --workload-ids=<principal>

gcloud agent-identity auth-providers add-iam-policy-binding kode-gopher \
  --location=<LOCATION> --role=roles/agentidentity.user --member=<principal>
```

Points to get right:
- **Authorization URL:** `access_type=offline` is what lets the vault renew tokens without asking the user again.
- **The principal appears twice,** in `--workload-ids` and in the IAM binding.
- **Propagation:** the IAM grant can take a few minutes to take effect.

Deploy with `VAULT_AUTH_PROVIDER=projects/<PROJECT_ID>/locations/<LOCATION>/authProviders/kode-gopher`.

### 3. Decide who's admitted

Set `ALLOW_DOMAINS` (Workspace domains, matched against the ID token's `hd` claim), `ALLOW_GROUPS` (Google group emails, nested membership counts), or both. An empty allow-list admits no one. Membership is checked at sign-in and at every token refresh, so removing someone takes effect within 15 minutes.

The group check calls Cloud Identity as kode-gopher's own identity, which needs the Groups Reader admin role in your Workspace. That role is assigned by email, and kode-gopher's Workload Identity principal has none. Granting it this way is untested (see [design-in-cluster.md](./design-in-cluster.md#to-validate-before-or-while-building)). Domains work with no extra setup.

### 4. Pre-registered clients (optional)

By default any MCP client can register itself (DCR, or a Client ID Metadata Document). Before Google sign-in, each user sees a kode-gopher page naming the client and asking them to confirm. Set `OPEN_REGISTRATION=false` to accept only clients listed in `CLIENTS_FILE`, a JSON array:

```json
[
  {"client_id": "inspector", "client_name": "MCP Inspector", "redirect_uris": ["http://localhost:6274/oauth/callback"]},
  {"client_id": "ci", "client_secret": "<random>", "client_name": "CI", "service_account": "runner@<PROJECT_ID>.iam.gserviceaccount.com"}
]
```

A client with a `service_account` is a **client-credentials** client, for CI and automation. It needs a secret and no `redirect_uris`, and it gets access tokens with no browser:

```bash
curl -u ci:<secret> -d grant_type=client_credentials https://<host>/token
```

Its snippets run as that service account, so kode-gopher needs `roles/iam.serviceAccountTokenCreator` on it, as in Option 1. go-sdk's `extauth.ClientCredentialsHandler` speaks this grant. Removing the client from the file, or changing its account, revokes its tokens at the next request.

### 5. Deploy

```bash
GOOGLE_CLIENT_FILE=./client_secret_....json \
ALLOW_DOMAINS=example.com \
VAULT_AUTH_PROVIDER=projects/<PROJECT_ID>/locations/<LOCATION>/authProviders/kode-gopher \
scripts/deploy-gke-server.sh --auth=oauth
```

The script stores the client JSON, a new token-sealing keyring and `CLIENTS_FILE` in the Secret `kode-gopher-oauth`. On later deploys `GOOGLE_CLIENT_FILE` is optional. The keyring is kept, because replacing it would invalidate every token kode-gopher has issued.

### 6. Connect

**Claude Code:**

```bash
claude mcp add --transport http kode-gopher https://<host>/mcp
```

Then run `/mcp` in Claude Code and authenticate. Your browser goes through kode-gopher's confirmation page and Google sign-in, and on first use the vault's consent screen. Claude Code then receives its tokens on a `localhost` callback, so the browser must run on the same machine as Claude Code.

If Claude Code runs on a remote machine (SSH, Cloud Workstations), there are two ways to get the callback through:
- add `--callback-port <port>` to pin the port, and forward that port from your local machine (`ssh -L`, or `gcloud workstations start-tcp-tunnel`);
- when the browser fails to load `http://localhost:<port>/callback?...`, rewrite just the host part of that URL so it reaches the remote machine.

**MCP Inspector:** connect to `https://<host>/mcp` with the Streamable HTTP transport, and use its OAuth flow.

Check with `gcp_auth_status`. It should report `mode=oauth` and your email.

## Your own domain

Set `KG_HOST=kode-gopher.example.com` and create a DNS A record pointing it at the `kode-gopher-ip` address. Do this before the first deploy, because the certificate is issued for whatever hostname the script sees the first time. To change the hostname later, delete `kode-gopher-cert`, then redeploy. Under Google sign-in, the issuer URL changes with the hostname, so update the OAuth client's redirect URI as well.

## Test a deployment

`scripts/smoketest-http.sh` reads the auth mode from the Deployment and checks everything that doesn't need a browser:
- **Both modes:** 401s without a valid token.
- **Static:** a full MCP session, in which a snippet gets its token from the metadata emulator with no ADC file in the sandbox.
- **Google sign-in:** discovery metadata, registration, and every refusal path of `/authorize` and `/token`.
- **With `--client-credentials <file>`** (`{"client_id", "client_secret"}`): a full session as that client's service account.

```bash
scripts/smoketest-http.sh                    # through the load balancer
scripts/smoketest-http.sh --port-forward     # before the certificate is ACTIVE
```

## Operating it

- **Upgrade:** rerun the deploy script, with a new `IMAGE` if you pin one. Secrets, the IP and the certificate are kept. The sandbox image is pinned in `manifests/overlays/gke`, so pulling a newer checkout updates it.
- **Sessions:** one MCP session holds one sandbox. Idle sessions end after 15 minutes, and each user may hold 2 sandboxes at once. Sandbox claims carry a 10-minute lease that kode-gopher renews, so if the server dies, its sandboxes are cleaned up.
- **One replica.** Sessions and the token replay cache live in memory. A restart ends open MCP sessions (clients reconnect) and forgets the replay cache.
- **Rotate the static token:** `kubectl -n codemode delete secret kode-gopher-token`, then redeploy. The script creates a new one.
- **Logs:** `kubectl -n codemode logs deployment/kode-gopher`. They contain no tokens.
- **Network:** kode-gopher's egress is limited to DNS, the sandboxes, the API server, the GKE metadata server and public HTTPS. Its ingress is limited to Google's load balancer ranges. The sandbox router accepts no in-cluster traffic at all.

## Remove it

```bash
kubectl -n codemode delete gateway kode-gopher   # first, so the load balancer releases the IP and certificate
kubectl delete -k manifests/overlays/gke-server
kubectl -n codemode delete secret kode-gopher-token kode-gopher-oauth --ignore-not-found
gcloud compute addresses delete kode-gopher-ip --global
gcloud compute ssl-certificates delete kode-gopher-cert --global
```

Also remove the IAM grants you made to kode-gopher's principal. Those grants belong to the project's Workload Identity pool, not the cluster, so a future `codemode/kode-gopher` service account in any cluster of the project would inherit them. If you're finished with them, also remove the OAuth client's redirect URI and the vault auth provider.

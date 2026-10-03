# In-cluster kode-gopher: design

Status: built (slice 9, steps 1-5), 2026-10-02. This doc answers the open questions in `docs/plan.md > Slice 5` for an in-cluster deployment, and records how each step was built and checked. To deploy kode-gopher, see the [docs site](https://gke-demos.github.io/kode-gopher/deploy/).

## Goal

A user adds one URL to their MCP client, signs in with Google, and `execute_go_code` runs as them. They need no kubeconfig, no Kubernetes RBAC, and no local kode-gopher binary.

The local stdio mode (`kode-gopher serve` with forwarded ADC) stays for development and personal use.

## Shape

```
MCP client (Claude Code, Desktop, ...)
   │  HTTPS, Streamable HTTP, Bearer <kode-gopher token>
   ▼
GKE Gateway (global external Application LB, managed cert)
   ▼
kode-gopher Deployment (namespace codemode)
   ├─ /mcp                     MCP endpoint (go-sdk StreamableHTTPHandler + RequireBearerToken)
   ├─ /.well-known/...         protected-resource + authorization-server metadata
   ├─ /authorize /token /register /callback    minimal OAuth AS fronting Google sign-in
   │
   │  Kubernetes API (KSA kode-gopher: create/get/delete/patch SandboxClaims)
   │  HTTP to <sandbox>.codemode.svc:8888 (ConnectivityInClusterService, no router)
   ▼
sandbox pods (unchanged: gVisor, C3, no KSA token, egress to public IPs only)
```

The router and port-forward drop out of this path. The router stays in the base only for local stdio use.

## Decisions

### 1. Transport: Streamable HTTP, stateful sessions

`kode-gopher serve --transport=http --addr=:8080` serves go-sdk's `StreamableHTTPHandler`. It's stateful: the `Mcp-Session-Id` identifies the session, which owns one sandbox. `SessionTimeout` closes idle sessions.

Stateful sessions live in one replica's memory, so v1 runs **1 replica**. Scaling out needs session affinity on `Mcp-Session-Id` at the Gateway, or a session store. That's deferred until one replica is shown to be the limit. At ~2.5 s of sandbox time per call, the kode-gopher process itself is mostly idle.

When the replica restarts, clients get 404 for their session. Per the MCP spec, they then re-initialize, and the next call claims a new sandbox.

### 2. Session topology: one sandbox per MCP session

This keeps today's stdio semantics: files persist between calls, and so does the `$GOCACHE` of the session's own builds. The session is bound to the authenticated user. A session presented with another user's token is rejected.

Per-request claims would add 1-2 s of claim time to every call for no isolation benefit. A session is already one user.

**Leaks.** HTTP gives no reliable disconnect. The SDK's `SessionTimeout` (`--session-timeout`, default 15 min) ends a session that sees no requests, and ending a session (timeout or client `DELETE`) closes its sandbox. That covers a live server. For a crashed one, each claim gets `spec.lifecycle.shutdownTime = now + lease` (`--claim-lease`, default 10 min) and `shutdownPolicy: Delete`. While the session is open, kode-gopher renews the lease every third of its length. If kode-gopher dies, renewals stop and the controller reaps the sandbox within the lease. The v1.0.4 client doesn't expose lifecycle, so kode-gopher patches the claim right after `Open`, and fails the open if that first patch fails. Local stdio `serve` and `exec` use the same lease since #34; only `--persistent` and `exec --keep` stay lease-free.

**Quota.** There's a per-user cap on open sandboxes (`--max-sandboxes-per-user`, default 2), so one user can't drain the warm pool. It counts sessions that hold a sandbox. A session only takes a slot on its first sandbox-using tool call, so a session that never runs code doesn't count.

### 3. Auth: kode-gopher is its own OAuth authorization server, fronting Google

The MCP authorization spec makes the server an OAuth 2.1 resource server. The client discovers an authorization server via Protected Resource Metadata (RFC 9728) and registers with it dynamically (RFC 7591) or with a Client ID Metadata Document. Google's OAuth server supports neither DCR nor CIMD, so the client can't talk to Google directly.

The spec also forbids token passthrough: the token the client presents to kode-gopher must not be forwarded upstream.

So kode-gopher implements the spec's third-party-authorization pattern:
1. **Registration.** The MCP client identifies itself to kode-gopher by any of the registration methods below, then starts `/authorize`.
2. **Google sign-in.** kode-gopher redirects to Google sign-in as a confidential OAuth client it owns. The scopes are `openid email https://www.googleapis.com/auth/cloud-platform`, with `access_type=offline`.
3. **Callback.** On `/callback`, kode-gopher receives Google's tokens, checks the user against an allow-list (a Google group or domain), and completes the MCP client's authorization code flow with **its own** tokens.
4. **Tool calls.** kode-gopher unseals the caller's access token, which carries a Google access token for the user, and hands that token to the session's sandbox for the run (section 4).

go-sdk gives us the resource-server side (`auth.RequireBearerToken`, `auth.ProtectedResourceMetadataHandler`) and the metadata types (`oauthex`). We write the authorization-server endpoints: registration, authorize, callback and token, with PKCE. That's the bulk of the new code.

#### Standard methods supported

We target the MCP authorization spec 2025-11-25 and the go-sdk client in v1.6.0 (`auth.AuthorizationCodeHandler`, `auth/extauth`), so stock clients work without configuration.

**Discovery.**
- **Protected Resource Metadata (RFC 9728)** at `/.well-known/oauth-protected-resource`. It's also referenced from the 401 response's `WWW-Authenticate: Bearer resource_metadata=...`.
- **Authorization-server metadata** at both `/.well-known/oauth-authorization-server` (RFC 8414) and `/.well-known/openid-configuration`. The spec has clients try both.

**Authorization code grant, OAuth 2.1 rules:**
- PKCE is required, and only `S256`. The `plain` method, and requests without PKCE, are rejected.
- Redirect URIs must match exactly. Loopback redirects (`http://127.0.0.1:<any port>`, `http://localhost`) are allowed per RFC 8252 for native clients.
- There's no implicit grant and no password grant.
- Refresh tokens rotate for public clients: each refresh returns a new one, and the old one is invalid.

**Resource indicators (RFC 8707).** Clients send `resource=https://<host>/mcp`. Tokens carry that audience, and the verifier rejects any other. This is the spec's defence against token reuse across servers.

**Scopes and step-up.**
- One scope, `kode-gopher:execute`, is required for `execute_go_code`.
- Missing scope gets `403` with `WWW-Authenticate: Bearer error="insufficient_scope", scope=...`, which triggers the client's step-up flow.
- Room is left for finer scopes later, such as read-only tools.

**Client registration, all three spec mechanisms:**

| Mechanism | How we support it |
|---|---|
| **Client ID Metadata Documents** (spec-preferred) | `client_id` is an HTTPS URL. We fetch the document (size cap, timeout, public IPs only, cached per HTTP caching headers) and check `redirect_uris` and the client name shown at consent. |
| **Dynamic Client Registration (RFC 7591)** | Open registration for public clients at `/register`. Stateless: the issued `client_id` is a sealed envelope of the registered metadata, so there's nothing to store and any replica can check it. |
| **Pre-registered clients** | Static client IDs (and secrets for confidential clients) from config, for operators who want a closed set. |

An operator can turn off open registration (CIMD and DCR) and keep only pre-registered clients.

**Extensions.**

| Extension | Who uses it | What snippets run as |
|---|---|---|
| **Client credentials** (RFC 6749 §4.4; go-sdk `extauth.ClientCredentialsHandler`) | CI and automation, with no user. Pre-registered confidential clients only. | The service account configured for that client (service-identity mode). There's no user to act as. |
| **Enterprise-managed authorization** (SEP-990: ID-JAG via token exchange, RFC 8693, then the JWT-bearer grant, RFC 7523; go-sdk `extauth`) | Organizations whose IdP (Okta, Entra, and so on) issues identity assertions for kode-gopher. We trust configured issuers. | Later. The assertion identifies the user but brings no Google grant. Options: service identity, or Workforce Identity Federation (exchange the IdP token at Google STS for a federated token for that user). |

**What "acting as the user" requires.** Only the authorization-code flow through Google sign-in gives us a Google refresh token for the user. The other methods authenticate the caller, but snippets then run as a configured service account, unless workforce federation is added later. `gcp_auth_status` reports which case applies (`mode=oauth` or `mode=service`).

**Token storage: stateless.** Both of kode-gopher's tokens are AEAD-sealed envelopes. The client holds them but can't read them. The key comes from Secret Manager and is rotated by key ID, so there's no database and any replica can verify.

| kode-gopher token | Contents | Lifetime |
|---|---|---|
| refresh token | `{sub, email, client_id, google_refresh_token}` | until Google revokes the grant; rotated on every use |
| access token | `{sub, email, client_id, aud, scope, google_access_token, exp}` | `min(15 min, Google token's remaining life)` |

Google is called only at `/token`, for the code exchange or a refresh:
- kode-gopher uses the Google refresh token to get a fresh Google access token;
- it seals that token into a new kode-gopher access token.

A tool call makes no Google call and holds no state. It unseals the Google access token from the request's bearer token. The Google refresh token exists only inside the sealed refresh token held by the MCP client. It's never in memory during a tool call and never reaches a sandbox.

Revocation: revoking the Google grant (or removing the user from the allow-list, checked at each refresh) kills the next refresh. Access tokens die within 15 min, but a Google access token already handed to a sandbox stays valid for up to its 1 h life. Google doesn't let us shorten it, which is one reason section 4 drops it when each run ends.

#### Alternative custody: the Agent Identity credential vault

Google's Agent Identity auth manager (`agentidentity.googleapis.com`; [3-legged OAuth](https://docs.cloud.google.com/iam/docs/auth-with-3lo-v2)) can hold users' Google grants instead of our sealed refresh token. Google runs its own consent leg, with PKCE and `state`, against our OAuth client, and keeps the tokens in a Google-managed vault keyed by a user ID we supply. kode-gopher calls:
- `authProviders/{p}/credentials:retrieve` with `{userId, scopes, continueUri}`. It returns either `success {token, expireTime, scopes}` or `uriConsentRequired {authorizationUri, consentNonce}`. `forceRefreshToken` forces a new token.
- `credentials:finalize` with `{userId, consentNonce, userIdValidationState}` after the browser returns to our `continueUri`.

How it fits:
- **Unchanged:** the MCP-facing authorization server (PKCE, resource indicators, registration). The vault has no MCP-client side.
- **Still ours:** user sign-in. The vault trusts the user ID we assert, so we sign the user in first (Google, `openid email` only) and use their stable `sub` as `userId`.
- **Consent:** it's chained into `/authorize`. After sign-in, `retrieve`. On `uriConsentRequired`, redirect the browser to `authorizationUri`. Google calls back to the vault, then to our `continueUri`. We `finalize` and then finish the MCP client's authorization code flow.
- **Tokens:** kode-gopher's refresh token then carries no Google secret, just `{sub, email, client_id}`. The Google access token sealed into kode-gopher's access token comes from `retrieve` at `/token` time.
- **Gains:** no Google refresh token passes through our tokens; Google-side audit and revocation; the same mechanism reaches non-Google providers (GitHub, Jira) later.

**Spike, 2026-09-30, phase A (GKE access): passed.**
- The auth provider `kg-spike-google` in `us-central1` has a placeholder OAuth client.
- It accepts a GKE Workload Identity principal (`principal://iam.googleapis.com/projects/<n>/locations/global/workloadIdentityPools/<project>.svc.id.goog/subject/ns/codemode/sa/<ksa>`) both in `--workload-ids` and in an IAM binding for `roles/agentidentity.user`. That role is GA and grants only `agentidentity.authProviders.retrieveCredentials`.
- From a pod, `retrieve` returned `uriConsentRequired`, after about 4 minutes of IAM propagation.
- The consent URL carries `code_challenge_method=S256` and `state`, and redirects to `.../authProviders/kg-spike-google/oauthcallback`.
- Project owner does **not** include `retrieveCredentials`, so the principal needs the role explicitly.

**Spike, 2026-09-30, phase B (real consent): passed.** It used a Web OAuth client marked "used by an AI-powered agent". The harness ran in the spike pod as the Workload Identity, and the continue URI was reached through a browser.
- **The flow works end to end.** `retrieve` returns `uriConsentRequired`, then Google consent, then the vault's `oauthcallback`, then our continue URI. That URI receives `user_id_validation_state`, `auth_provider_name`, `connector_name` and `uuid`. Then `finalize` (200, empty body), then `retrieve` returns `success` with a 1 h token issued to our client (`azp`).
- **Finalize caller:** the workload, with only `roles/agentidentity.user`. No extra role is needed.
- **The user ID isn't checked.** The vault accepted an arbitrary `userId` and stored the grant of whichever account consented. The API has only `retrieve` and `finalize`, so there's no way to list or delete a stored credential. kode-gopher must:
  - request `openid email` alongside `cloud-platform`;
  - after `finalize`, check the token's tokeninfo `sub` against the signed-in user;
  - refuse on a mismatch.
  The `userId` is always our own verified `sub`, never user input.
- **Offline access is configurable.** By default the consent URL has no `access_type=offline`. Query parameters on the provider's `authorizationUrl` are preserved, so `...?access_type=offline&prompt=consent` adds them.
- **Renewal works, given the offline grant and exact scopes.** After the 1 h token expired, plain `retrieve` silently returned a fresh token for the same `sub`, and `retrieve` with `forceRefreshToken` minted another. No consent was needed. Two traps:
  - **Scopes must match the stored grant exactly.** The vault stores Google's canonical names (`email` becomes `https://www.googleapis.com/auth/userinfo.email`). A request naming `email` doesn't match, and every call falls through to `uriConsentRequired`. kode-gopher requests `openid`, `https://www.googleapis.com/auth/userinfo.email` and `cloud-platform`, spelled that way, always.
  - **`expireTime` is unreliable.** One renewed token reported an `expireTime` 4 h out, while tokeninfo gave it 1 h. Treat tokens as good for under an hour, and retry with `forceRefreshToken` on a 401.

  `forceRefreshToken` with a still-valid token returns `uriConsentRequired`, as the docs imply (set it only for an expired or invalid token). Without the offline grant, the grant can't renew.
- **Org admin trust.** In a Workspace org that restricts Google Cloud scopes, consent fails with "Access blocked: your institution's admin needs to review" until an admin marks the client Trusted. This applies to kode-gopher's own client under either custody option.

Decision: phase B passed, so the vault is the default custody backend and the sealed envelope is the fallback for deployments without it. It's an interface in `internal/oauth`, either way.

**Google OAuth client.** It's an "Internal" consent screen in the deploying organization, so `cloud-platform` needs no app verification. Mark it "used by an AI-powered agent". In orgs that restrict Google Cloud scopes, a Workspace admin must mark it Trusted (see the phase B results). This is the "own a client" cost that `docs/design.md > Why not 3LO` avoided for desktop use; in-cluster there's no way around it.

**Deployment config.** Nothing about the Google side is hard-coded. Each deployment sets:
- its project;
- the OAuth client (ID, plus the secret in Secret Manager);
- the issuer URL;
- the credential custody backend (with the auth provider name, if it's the vault);
- the allow-list.

The reference deployment uses `gke-demos-345619` in the `gke.ninja` org.

**Allow-list.** Users are admitted by Google group, by domain, or both. Membership in any listed group or domain admits them, and an empty list admits no one. Group support is required.
- **Domains:** the verified ID token's `hd` claim, never the email suffix.
- **Groups:** kode-gopher asks Cloud Identity with its own Workload Identity. It calls `groups.memberships.checkTransitiveMembership`, so nested groups count. The kode-gopher service account needs read access to group membership, which a Workspace admin grants with the Groups Reader admin role. Users don't consent to any groups scope.
- **When it's checked:** at the callback and at every refresh, with the result cached for 5 minutes. Removing someone from a group takes effect at their next refresh, so within the 15-minute access-token lifetime.

#### Step 3 as built

`internal/oauth`, enabled with `kode-gopher serve --transport=http --in-cluster --auth=oauth`. It differs from the design above, or settles details it left open, as follows.

**Flags.** Nothing about the Google side is built in:

| Flag | Meaning |
|---|---|
| `--oauth-issuer` | Public base URL. The MCP endpoint, and every token's audience, is `<issuer>/mcp`. Must be https (http only on loopback, for tests). |
| `--oauth-google-client-file` | kode-gopher's Google OAuth client, as the JSON the console downloads. Its redirect URIs must include `<issuer>/callback`, and under vault custody the auth provider's `oauthcallback` URL (the vault then returns the browser to `<issuer>/consent/continue`). |
| `--oauth-keyring-file` | Sealing keys, one `<id> <base64 32 bytes>` per line, sealing key first. Keep older keys listed until what they sealed has expired (30 days for refresh tokens). |
| `--oauth-custody` | `vault` (default) with `--oauth-vault-auth-provider=projects/<p>/locations/<l>/authProviders/<name>`, or `sealed`. |
| `--oauth-allow-domains`, `--oauth-allow-groups` | Comma-separated. At least one entry. |
| `--oauth-clients-file` | Pre-registered clients: a JSON array of `{client_id, client_secret?, client_name, redirect_uris}`. |
| `--oauth-open-registration` | Accept DCR and CIMD clients. Default true. |
| `--project`, `--quota-project` | Reported to snippets as `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_QUOTA_PROJECT`. |

The vault and Cloud Identity calls use kode-gopher's own ADC (its Workload Identity).

**Consent page.** kode-gopher acts as a proxy for Google, so the spec requires user consent per dynamically registered client: without it, a malicious DCR or CIMD client could ride a user's existing Google session. So DCR and CIMD clients get a kode-gopher page naming the client (and the CIMD URL) and its redirect host, with Continue and Cancel, before Google sign-in. Pre-registered clients skip it.

**Browser binding.** `/authorize` sets a random `kg_auth` cookie (HttpOnly, SameSite=Lax, Secure on https). The consent form and Google `state` carry its hash, and the callback refuses a different browser. Vault consent carries the pending sign-in in a sealed `kg_consent` cookie.

**Sign-in scopes by custody.**
- Sealed: `openid`, `userinfo.email` and `cloud-platform`, offline. The callback refuses a grant missing `cloud-platform` or the refresh token.
- Vault: only `openid` and `userinfo.email`, online. The vault's own consent asks for `cloud-platform`.

**ID token.** It comes straight from Google's token endpoint over TLS, so per OIDC Core §3.1.3.7 the signature isn't checked. The issuer, audience, expiry, `sub` and `email_verified` are.

**Vault grants are checked on every retrieve.** The tokeninfo `sub` must equal the signed-in user's, and the token must carry `cloud-platform`. A wrong-account consent stays in the vault under the user's ID, because there's no delete API, so the user can't sign in until it's revoked at https://myaccount.google.com/permissions. The vault's `expireTime` is ignored in favour of tokeninfo's, and a token with under 5 minutes left is force-refreshed once.

**Tokens.**

| kode-gopher token | Contents | Lifetime |
|---|---|---|
| authorization code | the request, user, Google access token (and refresh token, sealed custody) | 1 min, single use |
| access token | `{sub, email, client_id, aud, scope, google_access_token, google_token_exp}` | `min(15 min, Google token's life - 1 min)` |
| refresh token | `{user, client_id, aud, scope, google_refresh_token (sealed custody only), jti}` | 30 days, renewed by each use; rotated for every client |

Refresh doesn't take a `resource`; it keeps the original audience. Each envelope's purpose is in its AEAD associated data, so one kind never opens as another.

**Replay cache.** Spent codes and rotated refresh tokens are remembered in memory, until they'd have expired anyway. v1 runs one replica, and a restart forgets: a refresh token already rotated before the restart could be used once more. A shared cache comes with multiple replicas.

**Scope.** `kode-gopher:execute` is always granted; unknown requested scopes are dropped. A valid token without it gets `403` with `Bearer error="insufficient_scope"`. go-sdk's own 403 lacks the error code, so kode-gopher answers that case itself.

**Allow-list failures.** If the group check itself fails (Cloud Identity down or denied), the callback returns `server_error` and a refresh returns HTTP 500. Neither is cached, and neither revokes anything.

**Why not IAP.** IAP authenticates the user but gives the backend an identity assertion, not a Google access token for the user. So snippets couldn't act as the user. MCP clients also don't do IAP's browser flow. IAP would fit only the service-identity mode below.

### 4. Credentials in the sandbox: access token only, via a local metadata emulator

Today the sandbox gets the user's ADC file, which includes the refresh token. In-cluster, the sandbox must never see a refresh token.

The token travels **with the run request and lives only in sandbox-server's memory** for that one command. It's never written to the sandbox filesystem.

1. **Deliver.** The run step's `/execute` request carries an optional `credentials` field: `{access_token, expiry, email, project, quota_project}`.
   - sandbox-server is ours, so this is a backwards-compatible extension. Requests without the field behave as today, so stdio and the router path are unaffected.
   - The agent-sandbox client's `Run` can't carry extra fields: it takes only a command and timeouts. So in-cluster, kode-gopher sends the run step itself: a plain HTTP POST to the sandbox's `Status.ServiceFQDN:8888`. The client still owns claim, open and close; uploads can keep using the client's `Write`.
   - Retries are skipped deliberately. A run isn't idempotent, so it shouldn't be retried like the client's other calls.
2. **Serve.** Each such command gets its own **GCE metadata emulator** on its own 127.0.0.1 port. Concurrent commands never share one. It answers with the token:
   - `computeMetadata/v1/instance/service-accounts/{default,<email>}/token`;
   - `.../email`, `.../scopes`, `default/?recursive=true`;
   - `project/project-id`, `universe/universe-domain`.

   It requires `Metadata-Flavor: Google`, like the real server, and doesn't serve ID tokens (`/identity`).

   The command runs with `GCE_METADATA_HOST` and `GCE_METADATA_IP` set to that port. It also gets `GOOGLE_CLOUD_PROJECT` (and `GOOGLE_CLOUD_QUOTA_PROJECT`) from the field.
3. **Forget.** When the command exits, or its timeout fires, sandbox-server drops the token and closes the emulator's port.
   - Nothing depends on kode-gopher cleaning up: a kode-gopher crash mid-run can't leave a token behind.
   - Between calls, an idle session's sandbox holds no credential.
   - A session runs one call at a time, so there's never more than one token in flight per sandbox.
   - At session end, the claim and sandbox are deleted; pool sandboxes are never reused.

Rejected alternatives:
- **A token file uploaded with the code:** it lands on disk, and removal depends on a follow-up delete that a crash can skip.
- **The token in the command string** (`VAR=... ./bin`): it shows up in process arguments and in any command logging along the path.

**What the snippet can see.** A snippet can read the token, from the emulator or from memory. That's by design: it runs as the user, so it holds exactly what any Google client library would hold. What it never sees is the refresh token, so a leaked token dies within the hour and can't be renewed. That's the difference from stdio's forwarded ADC, which puts a refresh token in the sandbox.

Go's ADC (`google.FindDefaultCredentials`) falls through to the metadata server when `GOOGLE_APPLICATION_CREDENTIALS` is unset, and `metadata.OnGCE()` trusts `GCE_METADATA_HOST`. So snippets and client libraries work unchanged, and the prompt doesn't change.

Why an emulator rather than the real metadata server:
- the network policy blocks 169.254.169.254;
- Workload Identity would give the pod's identity, not the user's.

This also fixes `docs/design.md > workload`, which predates slice 8's policy.

**Service-identity mode** (step 5): a deployment can run every snippet as one GSA. kode-gopher uses its own Workload Identity to mint the token and feeds the same emulator. It suits demos and single-tenant tools. The only code difference is where the token comes from.

### 5. Network policy

| Policy | Ingress | Egress |
|---|---|---|
| sandbox (template, existing) | from `app=sandbox-router` **and** `app=kode-gopher`, :8888 | DNS; on the in-cluster server, :443 to an FQDN allowlist (`manifests/components/egress-allowlist`), else public IPs |
| kode-gopher (new) | from the Gateway's proxy ranges and GFE health checks, :8080 | DNS, API server, sandbox pods :8888, Google APIs |
| router (new) | none except port-forward (loopback) | sandboxes :8888, DNS |

The router's deny-all ingress policy (the earlier proposal) lands here too. The router stays only for local stdio.

### 6. RBAC and identity

- **KSA `kode-gopher`:**
  - a Role in `codemode` for `sandboxclaims` (create, get, list, watch, patch, delete) and `sandboxes` (get, list, watch);
  - no port-forward;
  - no cluster-wide grants.
- **Workload Identity for kode-gopher itself:**
  - `secretmanager.versions.access` on the OAuth client secret and the token-sealing key;
  - nothing else in user mode;
  - in service-identity mode, and for each client-credentials client's service account, `roles/iam.serviceAccountTokenCreator` on that service account only.
- **Users** need no Kubernetes RBAC at all.

### 7. What doesn't change

- Sandbox template, warm pool, image, and C3 ComputeClass.
- `internal/executor`, `internal/normalize`, and the tool surface (`execute_go_code`, `gcp_auth_status`).
- `gcp_auth_status` reports `mode=oauth, identity=<user email>`.
- Local stdio mode, forwarded ADC, and the router path.

### Step 4 as built

`scripts/deploy-gke-server.sh [--auth=static|oauth]` deploys `manifests/overlays/gke-server` (or `gke-server-oauth`, on top of it). That's everything in `manifests/overlays/gke` plus:

| File | What |
|---|---|
| `rbac.yaml` | KSA `kode-gopher`; Role and RoleBinding for claims and sandboxes in `codemode` |
| `deployment.yaml` | Service and Deployment: one replica, `Recreate`, image `ghcr.io/gke-demos/kode-gopher/server`, `--auth=static` with the token from Secret `kode-gopher-token` |
| `gateway.yaml` | Gateway (`gke-l7-global-external-managed`, HTTPS only, static IP `kode-gopher-ip`, pre-shared cert `kode-gopher-cert`), HTTPRoute, HealthCheckPolicy (`/healthz`), GCPBackendPolicy |
| `networkpolicy.yaml` | kode-gopher's ingress and egress |

`gke-server-oauth` swaps the token for the Secret `kode-gopher-oauth` (`google-client.json`, `keyring`, `clients.json`). The script appends the per-deployment flags: issuer, allow-list, custody and the vault auth provider.

What the script does that the overlay can't:
- It creates the static IP and the Google-managed certificate if they're missing. The hostname defaults to `<ip with dashes>.sslip.io`, so a test deployment needs no DNS.
- It fills in the API server's address (from the `kubernetes` EndpointSlice) in the egress policy.
- It creates the Secrets: a random static token, or for OAuth, a new keyring only on first deploy, since replacing it would invalidate every token.
- It renders the overlay through a temporary kustomization, which patches the image and the flags.

Decisions made while building:
- **Kubernetes Secrets, not Secret Manager**, for v1. Secret Manager needs the CSI add-on and IAM on each secret. Section 6's Workload Identity grant becomes unnecessary until then.
- **One replica.** Sessions and the OAuth replay cache are in memory. `Recreate` avoids two pods splitting them during a rollout.
- **Backend timeout of 900 s.** The load balancer's 30 s default would cut off longer `execute_go_code` calls and the standalone SSE stream.
- **Selectors use `component: server`** as well as `app: kode-gopher`. `app: kode-gopher` is what the sandbox policy admits, so other kode-gopher pods (test pods, `exec` jobs) carry it too and mustn't become Service endpoints.
- **kode-gopher's egress:**
  - DNS;
  - sandboxes on :8888;
  - the API server on :443;
  - the GKE metadata server (169.254.169.254:80 and 169.254.169.252:988, for Workload Identity);
  - public IPs on :443.

  Its ingress is Google's front-end ranges (130.211.0.0/22, 35.191.0.0/16) on :8080. Those cover both the proxies and the health checks.
- **Router lock-down** (`manifests/overlays/gke/router-networkpolicy.yaml`): no ingress at all. Egress is DNS and sandboxes on :8888. `kubectl port-forward` and kubelet probes aren't subject to policy, so local stdio is unaffected.

Checked on `kg-sandbox` on 2026-10-01:
- `scripts/smoketest-http.sh` with static auth, both through the load balancer (`https://<ip>.sslip.io`, once the certificate turned ACTIVE, within an hour of its creation) and with `--port-forward`:
  - 401 without a token or with a wrong one;
  - initialize, tools/list, and `gcp_auth_status` reports `mode=access-token`, `credential_type=metadata`;
  - a snippet got a valid token from the metadata emulator, with no ADC file or `GOOGLE_APPLICATION_CREDENTIALS` in the sandbox (built in 0.7 s);
  - two sessions, and `DELETE` ended both.
- From a pod in another namespace, the router, the kode-gopher Service and a sandbox pod all timed out, while a public URL answered.
- `kode-gopher exec` over stdio (port-forward to the locked-down router) still listed the project's buckets.

Real Google sign-in, checked on `kg-sandbox` on 2026-10-02, with `--auth=oauth`, vault custody (`kg-spike-google`) and `--oauth-allow-domains`:
- **Setup.** The Google OAuth client lists `<issuer>/callback` as a redirect URI. The auth provider lists kode-gopher's KSA principal in its workload IDs and grants it `roles/agentidentity.user`. Groups Reader isn't needed with a domain allow-list.
- **Claude Code** (`claude mcp add --transport http --callback-port <port> kode-gopher <issuer>/mcp`, then `/mcp`) went through the whole flow:
  - it identified itself with a Client ID Metadata Document (`client_id=https://claude.ai/oauth/claude-code-client-metadata`), so kode-gopher fetched claude.ai's metadata document (CIMD), with no DCR;
  - kode-gopher's consent page, Google sign-in, the vault's own consent, `/consent/continue`, then the code exchange.

  Claude Code ran on a Cloud Workstation, so its `localhost` callback was reached by rewriting the final redirect's host to the workstation's port proxy.
- **Results.**
  - `gcp_auth_status` reported `mode=oauth, credential_type=authorized_user` and the signed-in user's email.
  - An `execute_go_code` snippet the model wrote listed the project's GKE clusters as that user (built in 1.3 s, no tidy).
- `scripts/smoketest-http.sh --client-credentials` passed against the same deployment.

More checks on the same day, after redeploying release v0.1.1 (pinned by digest):
- **Token refresh.** Claude Code made a call after 3 hours idle without signing in again: the refresh grant got a fresh Google token from the vault.
- **Allow-list refusal.** `garisingh@google.com`, an account in another Workspace domain, got past Google's sign-in (the Internal client admitted it) and was refused at kode-gopher's callback: `access_denied: garisingh@google.com isn't allowed to use this server`. No code was issued and the vault was never asked.
- **Signing in again.** A second sign-in as the same user skipped the vault's consent, because the grant was already stored. One earlier attempt ended on kode-gopher's "Sign-in expired" page: each `/authorize` sets a new browser cookie, so a sign-in link opened twice can only finish in its newest tab.
- **No credentials left in the sandbox.** During a client-credentials run, the sandbox's environment, every process's environ and cmdline, and 5,354 files held no copy of the run's token, no other access token, and no refresh token. After the run, the emulator's port refused connections.
- **Emulator coverage.** Storage, BigQuery, Compute (REST), GKE and Secret Manager (gRPC) all got a token from the emulator; the identity has no roles, so each got a permission-denied from the API, proving the credentials path. `instance/zone`, `instance/id` and identity tokens aren't served; `?scopes=` is accepted and ignored.
- **Graceful restart.** A rollout released an open session's claim at once (no wait for the lease). The old session ID got 404 from the new pod, and a new session worked on another sandbox. `smoketest-http.sh --client-credentials` passed on v0.1.1.
- **Found and fixed or filed:** results over 16 KiB vanished (#26); sandboxes saw the namespace's Service links (#29); stdio and `exec` claims had no lease, so a killed local process leaked its sandbox (#34, fixed).

Still to run: a second user, MCP Inspector, and a hard pod kill (#35). The group allow list needs a design change first (#36).

### Step 5 as built

Two ways to run snippets as a Google service account rather than a user. Both mint the account's token through the IAM Credentials API (`generateAccessToken`, cloud-platform scope, 1 h) as kode-gopher's own Workload Identity, and feed it to the sandbox's metadata emulator like any other run token. `creds.Impersonator` caches one token per account and mints again 5 min before expiry. The KSA needs `roles/iam.serviceAccountTokenCreator` on each account, and nothing project-wide.

**Service-identity mode:** `serve --credentials=service --service-account=<gsa>` (`SERVICE_ACCOUNT=<gsa>` for `scripts/deploy-gke-server.sh`). Every snippet runs as that account. It's meant for `--auth=static`; under `--auth=oauth` each request already carries its own credentials. `gcp_auth_status` reports `mode=service, credential_type=service_account, email=<gsa>`, with the project taken from the account's email unless `GOOGLE_CLOUD_PROJECT` is set. This replaces the `creds.Workload` stub, which predated the in-cluster design. Without `--service-account`, `--credentials=access-token` still runs snippets as kode-gopher's own Workload Identity.

**Client-credentials grant** (RFC 6749 §4.4), for CI and automation:
- A pre-registered client with a `service_account` in `--oauth-clients-file`:
  ```json
  [{"client_id": "ci", "client_secret": "<random>", "client_name": "CI", "service_account": "runner@<project>.iam.gserviceaccount.com"}]
  ```
  It must have a secret and no `redirect_uris`, so it can't sign users in. User clients can't use the grant: `unauthorized_client`.
- `POST /token` with `grant_type=client_credentials` (secret by Basic or form) returns an access token only. There's no refresh token (§4.4.3); the client asks again.
  - `scope` may only be `kode-gopher:execute` (the default), or `invalid_scope`.
  - `resource`, if sent, must be the MCP endpoint, or `invalid_target`.
  - If minting fails, `server_error`, with the IAM error logged.
- The sealed access token has the same shape as a user's, with the service account's Google token inside, `mode=service`, and subject `client:<client_id>`. MCP sessions and the per-user sandbox cap bind to that subject.
- At each request the verifier also checks that the client still exists with the same service account. Removing it, or changing its account, revokes its tokens at once.
- Authorization-server metadata adds `client_credentials` to `grant_types_supported` when such a client is configured.
- The allow-list doesn't apply: an operator who pre-registers the client has admitted it.
- go-sdk's `extauth.ClientCredentialsHandler` connects unchanged (`internal/oauth/client_credentials_test.go`).

**Checks on `kg-sandbox`** (2026-10-01), with the step 5 image:
- `--credentials=access-token`: `scripts/smoketest-http.sh` passes as before.
- `--credentials=service` naming a service account that doesn't exist: `gcp_auth_status` reports `mode=service` and the account. `execute_go_code` returns IAM's error ("Gaia id not found"), which shows that kode-gopher reached the IAM Credentials API as its Workload Identity through the network policy.
- With a test service account `kg-runner` (no roles of its own; the KSA has `roles/iam.serviceAccountTokenCreator` on it):
  - `SERVICE_ACCOUNT=kg-runner@… scripts/deploy-gke-server.sh` (static auth): `scripts/smoketest-http.sh` passes, and the snippet's token belongs to kg-runner.
  - `--auth=oauth` (sealed custody, a pre-registered client bound to kg-runner): `scripts/smoketest-http.sh --client-credentials` passes. A wrong secret gets `invalid_client`, the token comes with no refresh token, and the snippet runs as kg-runner.

## Code changes

| Area | Change |
|---|---|
| `internal/sandbox` | `Options.Connectivity`: in-cluster service when running in a pod; claim lifecycle patch and lease renewal; direct `/execute` with credentials for the run step |
| `internal/mcp` | HTTP transport; per-session sandbox map keyed by `Mcp-Session-Id` and bound to user; per-user session cap |
| `internal/oauth` (new) | Authorization server: metadata, register, authorize, callback, token; sealed tokens; custody interface; allow-list (Cloud Identity group check, `hd` domain check) |
| `internal/creds` | `OAuthUser` source (the request's Google access token, unsealed from its kode-gopher access token); `Service` source and `Impersonator` for service-identity mode and client credentials |
| `cmd/sandbox-server` | Optional `credentials` field on `/execute`, held in memory for that command; metadata emulator on 127.0.0.1 |
| `manifests/overlays/gke-server` (new) | Deployment, Service, Gateway + HTTPRoute + cert, KSA + Role, network policies, Secret Manager refs |
| `scripts/smoketest-http.sh` (new) | End-to-end over HTTP with a pre-minted test token; negative auth cases |

## Build order

1. **Sandbox-server metadata emulator + in-cluster connectivity.** Test with a kode-gopher pod using forwarded ADC converted to an access token. That proves the data path without OAuth.
2. **HTTP transport with sessions, claim leases, and quota.** Behind a static bearer token for testing.
3. **OAuth authorization server with Google, plus the allow-list (groups and domains).**
   - Authorization code with PKCE, resource indicators, and all three registration mechanisms.
   - Test with Claude Code and MCP Inspector.
   - Credential custody is behind an interface: the Agent Identity vault by default (it passed the spike), sealed envelope as the fallback.
4. **Manifests: Gateway, cert, policies. Smoketest on GKE.**
5. **Service-identity mode and the client-credentials extension.** This is a separate step: a different trust model (no user), its own config, and nothing in steps 1–4 depends on it.

Enterprise-managed authorization is a later step.

Each step ships separately. Step 2 alone is a usable internal deployment.

## Pass criteria

- Claude Code, given only the URL, completes the sign-in and runs `list_buckets_snippet.go`. The output matches `gcloud` for that user, and a second user sees their own buckets.
- No refresh token ever reaches a sandbox, and the access token never touches its filesystem. Check by inspecting the filesystem and env during a run. After the run, the emulator's port is closed.
- An unauthenticated request gets 401 with a `WWW-Authenticate` resource-metadata pointer. A user outside the allow-list is refused at the callback. A user admitted only through a nested group gets in. Removing them from the group refuses their next refresh.
- Negative auth cases are rejected, each with the correct OAuth error:
  - no PKCE, or `plain` PKCE;
  - a mismatched redirect URI;
  - a token for another `resource`;
  - a reused refresh token.
- A go-sdk client signs in by each registration mechanism in turn: CIMD, DCR and pre-registered.
- Killing the kode-gopher pod mid-session: the client re-initializes, and orphaned claims are gone within the lease.
- From a pod in another namespace, the router and the sandboxes are unreachable.

## To validate before or while building

- **Client interop.** Test the sign-in against Claude Code, Claude Desktop, MCP Inspector and a go-sdk client. Clients differ in which registration method they try first and in how they send `resource`. Claude Code: passed 2026-10-02, by CIMD, sending `resource`. The go-sdk client is covered in tests (`TestE2EClientCredentials` and the OAuth e2e tests). MCP Inspector and Claude Desktop: #35.
- **CIMD fetch safety.** Fetching client-supplied URLs from the kode-gopher pod needs an SSRF guard (public IPs only, no redirects to private ranges). This is on top of its egress policy.
- **Scope policy.** Whether an org policy restricts `cloud-platform` on Internal OAuth clients. Confirmed for `gke.ninja`: the client had to be marked Trusted in the Admin console.
- **Group check permissions.** That the Groups Reader admin role on the kode-gopher service account is enough for `checkTransitiveMembership`. Also whether it covers groups with external members. Blocked: kode-gopher calls as a Workload Identity principal, which has no email to assign an admin role to. See #36 for the options.
- **Health checks.** GKE Gateway health-check source ranges for the kode-gopher ingress policy.
- **Library coverage.** Metadata emulator coverage for the client libraries in `curated.Packages`. Some ask for `?scopes=`, some for `/identity` tokens; we'll decide what to support.
- **Streaming progress.** `docs/plan.md > Slice 5 > Streaming` stays out of scope here. Streamable HTTP makes it possible later.

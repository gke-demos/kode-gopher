---
title: Google sign-in
description: Deploy kode-gopher with OAuth 2.1 sign-in through Google, so every user's snippets run as that user.
sidebar:
  order: 2
---

With `--auth=oauth`, kode-gopher becomes an OAuth 2.1 authorization server for MCP clients, and signs users in with Google:
1. A client that connects without a token is pointed at kode-gopher's metadata. It registers itself and opens a browser.
2. The user signs in with Google.
3. From then on, the client holds kode-gopher tokens.

Snippets run as the user, with a Google access token that kode-gopher keeps fresh.

## 1. Create a Google OAuth client

kode-gopher's host name goes into the client's redirect URIs. To know it before the first deploy, create the IP yourself; the script reuses it:

```bash
gcloud compute addresses create kode-gopher-ip --global
gcloud compute addresses describe kode-gopher-ip --global --format='value(address)'
# 203.0.113.7 means the host is 203-0-113-7.sslip.io
```

Then, in the Cloud console under **Google Auth Platform**:

1. **Audience:** use an **Internal** consent screen in your Workspace organization. That way the `cloud-platform` scope needs no app verification.
2. **Clients:** create a **Web application** client. If the console asks, mark it as used by an AI-powered agent. Add these **Authorized redirect URIs**:
   - `https://<host>/callback`
   - with vault custody (next step), also the auth provider's callback:
     `https://agentidentitycredentials.googleapis.com/v1/projects/<PROJECT_ID>/locations/<LOCATION>/authProviders/<NAME>/oauthcallback`
3. Download the client's JSON file.

:::note
If your organization restricts access to Google Cloud scopes, sign-in fails with "Access blocked: your institution's admin needs to review" until a Workspace admin marks the client as Trusted.
:::

## 2. Choose where users' Google grants are kept

- **Vault (default, recommended).** Google's Agent Identity credential vault holds each user's grant. Google runs its own consent for `cloud-platform`, and kode-gopher's tokens carry no Google secret. Revocation and audit happen on Google's side.
- **Sealed (`CUSTODY=sealed`).** kode-gopher keeps each user's Google refresh token, encrypted inside the kode-gopher refresh token it issues. There's nothing else to set up, which makes it a good choice for a first deploy.

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

What to get right:
- **`access_type=offline`** in the authorization URL lets the vault renew tokens without asking the user again.
- **[kode-gopher's principal](/deploy/#kode-gophers-identity) goes in twice:** in `--workload-ids` and in the IAM binding.
- **The IAM grant** can take a few minutes to take effect.

## 3. Decide who's admitted

- **`ALLOW_DOMAINS`:** Workspace domains, matched against the ID token's `hd` claim, not the email address.
- **`ALLOW_GROUPS`:** Google group emails. Nested membership counts. Needs the setup below.

Set either or both. An empty allow-list admits no one. Membership is checked at sign-in and at every token refresh, and a decision is reused for 5 minutes, so removing someone takes effect within about 20 minutes.

### Groups

kode-gopher checks group membership with the Cloud Identity Groups API. The calling identity needs the **Groups Reader** admin role in each Workspace whose groups it checks. Admin roles are assigned by email, and kode-gopher's Workload Identity principal has none, so it checks groups as a dedicated service account that it impersonates for that call only:

```bash
gcloud services enable cloudidentity.googleapis.com
gcloud iam service-accounts create kode-gopher-groups
gcloud iam service-accounts add-iam-policy-binding kode-gopher-groups@<PROJECT_ID>.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountTokenCreator --member=<principal>
```

Then, in each Workspace's Admin console, as a super admin: **Account > Admin roles**, point to **Groups Reader**, then **Assign admin > Assign service accounts**, enter the service account's email, **Add**, **Assign role**. Don't limit the assignment to security groups unless all your groups are security groups. The account needs no project roles, and kode-gopher mints its token with the read-only groups scope only.

Deploy with `ALLOW_GROUPS=<group emails>` and `GROUPS_SERVICE_ACCOUNT=kode-gopher-groups@<PROJECT_ID>.iam.gserviceaccount.com`. At startup kode-gopher mints one token for the account and logs a warning if it can't, for example because the token-creator grant is missing or still propagating.

**Workspace editions.** Cloud Identity's one-call transitive membership check is only available on Google Workspace Enterprise and Cloud Identity Premium. On other editions (Business Starter, Standard and Plus) it answers 403, and kode-gopher walks the group and its nested groups with the membership list instead, which every edition has:
- It checks each group's direct members before its nested groups, and a match anywhere admits.
- Users and service accounts match by email. Devices, shared drives and organization-wide entries ("everyone in the organization") aren't expanded.
- If a nested group can't be read (no Groups Reader in its Workspace, deleted, or from an external identity source), or nesting goes past 10 levels or 100 groups, members found elsewhere still get in. Only accounts not found anywhere get `server_error` instead of a refusal, since they might be in the group that couldn't be read.
- Membership lists are cached for 5 minutes, so one walk serves every user checked against the same groups.

:::note
Groups in another Workspace than the user's, such as a group in a second domain you administer, should work if the service account holds Groups Reader in the group's Workspace and that group allows external members. This hasn't been verified yet.
:::

## 4. Deploy

```bash
GOOGLE_CLIENT_FILE=./client_secret_....json \
ALLOW_DOMAINS=example.com \
VAULT_AUTH_PROVIDER=projects/<PROJECT_ID>/locations/<LOCATION>/authProviders/kode-gopher \
scripts/deploy-gke-server.sh --auth=oauth
```

The script stores the client file, a new token-sealing keyring, and any `CLIENTS_FILE` in the Secret `kode-gopher-oauth`. On later deploys `GOOGLE_CLIENT_FILE` is optional. The keyring is always kept, because replacing it would invalidate every token kode-gopher has issued.

By default any MCP client can register itself. To limit which clients can connect, or to add CI clients, see [Clients](/deploy/clients/).

## 5. Sign in

```bash
claude mcp add --transport http kode-gopher https://<host>/mcp
```

Run `/mcp` in Claude Code and authenticate. The browser goes through three screens:
1. kode-gopher's confirmation page, naming the client;
2. Google sign-in;
3. on first use, the vault's own consent screen.

Then ask the model to call `gcp_auth_status`. It should report `mode=oauth` and your email. [Clients](/deploy/clients/) covers remote machines and other MCP clients.

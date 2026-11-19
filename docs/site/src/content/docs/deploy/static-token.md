---
title: Static token
description: Deploy kode-gopher behind one shared bearer token.
sidebar:
  order: 1
---

The quickest deployment: one bearer token that every client sends.

```bash
scripts/deploy-gke-server.sh
```

The script generates the token and stores it in the Kubernetes Secret `kode-gopher-token` by default (or in Google Cloud Secret Manager when `SECRET_STORE=secret-manager` is set, granting `roles/secretmanager.secretAccessor` on that secret to kode-gopher's Workload Identity principal). To connect Claude Code:

```bash
TOKEN=$(kubectl -n codemode get secret kode-gopher-token -o jsonpath='{.data.token}' | base64 -d)
# Or when deployed with SECRET_STORE=secret-manager:
# TOKEN=$(gcloud secrets versions access latest --secret=kode-gopher-token --project=<PROJECT_ID>)
claude mcp add --transport http kode-gopher https://<host>/mcp --header "Authorization: Bearer $TOKEN"
```

:::caution
Anyone with the token can run code as the identity below. Share it like a password. To rotate it under `SECRET_STORE=k8s`, delete the Kubernetes Secret and rerun the deploy script; under `SECRET_STORE=secret-manager`, add a new secret version (`gcloud secrets versions add kode-gopher-token --data-file=...`) and rerun the deploy script to pin and load the new version.
:::

## Who snippets run as

By default, snippets run as kode-gopher's own [Workload Identity principal](/deploy/#kode-gophers-identity). That principal starts with no roles, so grant it what your snippets need, for example read-only access to the project:

```bash
gcloud projects add-iam-policy-binding <PROJECT_ID> --role=roles/viewer --member=<principal>
```

## Or as a service account

To run every snippet as a particular Google service account, allow kode-gopher to mint tokens for that account only. Then deploy with `SERVICE_ACCOUNT`:

```bash
gcloud services enable iamcredentials.googleapis.com
gcloud iam service-accounts add-iam-policy-binding <sa-email> \
  --role=roles/iam.serviceAccountTokenCreator --member=<principal>
SERVICE_ACCOUNT=<sa-email> scripts/deploy-gke-server.sh
```

kode-gopher calls the IAM Credentials API for each token, and caches it until 5 minutes before it expires. `gcp_auth_status` reports `mode=service` and the account's email.

## Check it

```bash
scripts/smoketest-http.sh                  # through the load balancer
scripts/smoketest-http.sh --port-forward   # before the certificate is ACTIVE
```

The smoketest checks that a request without the token, or with a wrong one, is refused. It then runs a full MCP session, in which a snippet gets its token from the metadata emulator with no ADC file in the sandbox. See [Operations](/deploy/operations/) for more.

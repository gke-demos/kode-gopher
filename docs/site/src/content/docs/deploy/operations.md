---
title: Operations
description: Your own domain, testing, upgrades, limits, and removal.
sidebar:
  order: 4
---

## Your own domain

Set `KG_HOST=kode-gopher.example.com`, and create a DNS A record pointing it at the `kode-gopher-ip` address.

Do this before the first deploy, because the certificate is issued for whatever hostname the script sees first. To change the hostname later, delete `kode-gopher-cert`, then redeploy. Under Google sign-in the issuer URL changes with the hostname, so update the OAuth client's redirect URI as well.

## Test a deployment

`scripts/smoketest-http.sh` reads the auth mode from the Deployment, and checks everything that doesn't need a browser:
- **Both modes:** a 401 without a valid token.
- **Static token:** a full MCP session, in which a snippet gets its token from the metadata emulator with no ADC file in the sandbox.
- **Google sign-in:** discovery metadata, client registration, and every refusal path of `/authorize` and `/token`.
- **With `--client-credentials <file>`:** a full session as that client's service account.

Use `--port-forward` before the certificate is ACTIVE.

## Upgrade

Rerun the deploy script, with a new `IMAGE` if you pin one. It keeps the Secrets, the IP and the certificate. The sandbox image is pinned in `manifests/overlays/gke`, so pulling a newer checkout updates it too.

## Limits and behavior

- **Sessions:** one MCP session holds one sandbox. Idle sessions end after 15 minutes, and each user may hold 2 sandboxes at once.
- **Crash cleanup:** sandbox claims carry a 10-minute lease that kode-gopher renews, so if the server dies, its sandboxes are cleaned up.
- **One replica:** sessions and the token replay cache live in memory. A restart ends open MCP sessions, which clients reopen, and forgets the replay cache.
- **Tokens:**
  - kode-gopher access tokens last up to 15 minutes;
  - refresh tokens last 30 days, renewed by each use;
  - authorization codes last 1 minute.
- **Network:**
  - kode-gopher's egress is limited to DNS, the sandboxes, the API server, the GKE metadata server and public HTTPS;
  - its ingress is limited to Google's load balancer ranges;
  - the sandbox router accepts no in-cluster traffic at all.

## Logs

```bash
kubectl -n codemode logs deployment/kode-gopher
```

kode-gopher never logs tokens or secrets.

## Remove it

```bash
kubectl -n codemode delete gateway kode-gopher   # first, so the load balancer releases the IP and certificate
kubectl delete -k manifests/overlays/gke-server
kubectl -n codemode delete secret kode-gopher-token kode-gopher-oauth --ignore-not-found
gcloud compute addresses delete kode-gopher-ip --global
gcloud compute ssl-certificates delete kode-gopher-cert --global
```

Also remove the IAM grants you made to kode-gopher's principal. Those grants belong to the project's Workload Identity pool, not the cluster: a future `codemode/kode-gopher` service account in any cluster of the project would inherit them. If you're done with them, also remove the OAuth client's redirect URI and the vault auth provider.

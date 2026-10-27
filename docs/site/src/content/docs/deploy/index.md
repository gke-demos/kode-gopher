---
title: Deploy for a team
description: Run kode-gopher as a shared MCP server in a GKE cluster, behind HTTPS.
sidebar:
  label: Overview
  order: 0
---

This section runs kode-gopher as a shared MCP server in a GKE cluster. It sits behind a GKE Gateway with a Google-managed certificate. MCP clients connect over streamable HTTP at `https://<host>/mcp`, and each MCP session gets its own sandbox in the same cluster.

## Choose how clients authenticate

| | Static token | Google sign-in |
|---|---|---|
| Clients send | one shared bearer token | a kode-gopher OAuth token, from signing in with Google in a browser |
| Snippets run as | kode-gopher's own identity, or a service account you name | each signed-in user, with their own Google token |
| Who can use it | anyone with the token | users admitted by Workspace domain or Google group |
| Setup | a few minutes | a Google OAuth client, and optionally Google's credential vault |
| Good for | trying it out, single-tenant tools, demos | teams |

Under Google sign-in, CI jobs and other automation can also get tokens with the OAuth `client_credentials` grant, and their snippets run as a service account.

Either way, the sandbox never holds a long-lived credential. Each run gets a short-lived access token from a metadata server emulator inside the sandbox. No ADC file and no Kubernetes service account token are mounted.

## Prerequisites

- **A GKE Autopilot cluster with the agent-sandbox addon and FQDN network policy,** and the `codemode` namespace. To create a new cluster:
  ```bash
  gcloud container clusters create-auto kode-gopher \
    --location=us-central1 --release-channel=rapid --enable-agent-sandbox
  gcloud container clusters update kode-gopher \
    --location=us-central1 --enable-fqdn-network-policy
  kubectl create namespace codemode
  ```
  Or to enable both features on an existing Autopilot cluster (kode-gopher lives in `codemode` and can share the cluster with other workloads):
  ```bash
  gcloud container clusters update <CLUSTER> \
    --location=us-central1 --enable-agent-sandbox
  gcloud container clusters update <CLUSTER> \
    --location=us-central1 --enable-fqdn-network-policy
  kubectl create namespace codemode
  ```
  Autopilot includes Workload Identity, gVisor and the Gateway API. FQDN network policy lets sandboxes reach only an allowlist of hostnames (see [Sandbox isolation](/concepts/isolation/)). Autopilot can only turn it on after creation, and GKE then restarts the nodes, which can take a few hours. The deploy script won't run without it.
- **A clone of the repository** (`git clone https://github.com/gke-demos/kode-gopher`). The deploy script applies its manifests.
- **`kubectl`, `gcloud` and `python3`,** and permission to create a global static IP address and an SSL certificate in the project, and to apply manifests in `codemode`.

### kode-gopher's identity

kode-gopher runs as the Kubernetes service account `codemode/kode-gopher`. Google IAM refers to it by this Workload Identity principal, which the grants in the following pages use:

```
principal://iam.googleapis.com/projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/<PROJECT_ID>.svc.id.goog/subject/ns/codemode/sa/kode-gopher
```

Get the project number with `gcloud projects describe <PROJECT_ID> --format='value(projectNumber)'`.

### Accessing multiple Google Cloud projects

A deployment is not limited to the project its cluster runs in. The access token handed to each snippet carries the `cloud-platform` scope, so a snippet can call APIs in **any** Google Cloud project that the signed-in user (or configured service account) has IAM permissions on. `PROJECT` (defaulting to `gcloud config get-value project`) and `QUOTA_PROJECT` only set the default `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_QUOTA_PROJECT` reported inside the sandbox when a snippet doesn't pass a project ID explicitly.

## The deploy script

`scripts/deploy-gke-server.sh` is safe to rerun. Every option is an environment variable; see [Deployment settings](/reference/deploy-settings/). It never prints secrets. Each run:

1. creates the global static IP `ADDRESS_NAME` (default `kode-gopher-ip`) if it's missing. The public hostname is `KG_HOST` if set, or `<ip-with-dashes>.sslip.io`, a wildcard DNS name that resolves to that IP;
2. creates the Google-managed certificate `CERT_NAME` (default `kode-gopher-cert`) for that hostname if it's missing;
3. creates the Secret the auth mode needs, keeping an existing one;
4. applies the manifests:
   - the kode-gopher Deployment (one replica), Service, Role and network policy;
   - the Gateway and HTTPRoute;
   - the sandbox template, warm pool and router.

The certificate turns ACTIVE once the hostname resolves to the IP and the load balancer is up, usually within 15 to 60 minutes of being created. If you run more than one kode-gopher deployment in the same GCP project, set distinct `ADDRESS_NAME` and `CERT_NAME` values for each so they don't share the same IP or certificate.

Use `CONTEXT=<kubectl context>` to target a cluster other than the current context, and `--dry-run` to print the manifests without applying anything.

**The server image.** By default the script deploys `ghcr.io/gke-demos/kode-gopher/server:main`. CI rebuilds it on every merge, for amd64 and arm64, and signs it with cosign. For a production deployment that doesn't change under you, pin `IMAGE` to a release tag (`ghcr.io/gke-demos/kode-gopher/server:<version>`) or a commit tag (`main-<short-sha>`).

## Next

- [Static token](/deploy/static-token/): the quickest deployment.
- [Google sign-in](/deploy/google-sign-in/): every user runs as themselves.

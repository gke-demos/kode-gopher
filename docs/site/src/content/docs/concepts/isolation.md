---
title: Sandbox isolation
description: What contains the code a model writes.
sidebar:
  order: 3
---

kode-gopher doesn't review the code a model writes. That's the point: the model writes arbitrary programs. Containment is the security model.

| Layer | What it does |
|---|---|
| Runtime | On GKE, sandboxes run under gVisor, which intercepts system calls in user space, with a locked-down security context: non-root, no privilege escalation, all capabilities dropped. kind has no gVisor, so use it for development only. |
| One sandbox per session | Each MCP session gets its own pod from the warm pool. Sessions never share a pod, so one user's files, caches and tokens are never visible to another. |
| No Kubernetes credentials | Sandboxes mount no service account token. A snippet can't act on the cluster it runs in, except through Google APIs as its own Google identity. Service links are off too, so the namespace's Services don't appear in the environment; only the kubelet's `KUBERNETES_SERVICE_HOST` remains. |
| Network | The sandbox's network policy allows DNS and public IP addresses only. Private ranges, the cluster's API server and the GKE metadata server are blocked. Inbound, only the router and kode-gopher may reach the sandbox's server port. |
| Credentials | Locally, your ADC. On a shared server, one short-lived access token per run, from the [metadata emulator](/concepts/credentials/#the-metadata-server-emulator), and no long-lived secret. |
| Filesystem | Scratch space in the pod only. `/app` is wiped before every call, and nothing persists once the session's sandbox is released. |

**What isn't contained:**
- **The identity's own permissions.** A snippet can do anything its identity is allowed to do in Google Cloud. Give service accounts and Workload Identity only the roles snippets need, and remember that locally, snippets run as you.
- **Data sent to public endpoints.** Egress is filtered by IP address, not by host name, so a snippet could send data to any public address.

The shared server adds its own controls in front:
- authentication, by static token or Google sign-in with an allow-list;
- a cap of 2 sandboxes per user;
- network policies that limit what can reach kode-gopher and the router.

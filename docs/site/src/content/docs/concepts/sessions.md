---
title: Sessions and sandboxes
description: How MCP sessions map to sandbox pods, and what happens when things go wrong.
sidebar:
  order: 4
---

## One sandbox per MCP session

- **Claim:** the first tool call in an MCP session claims a pod from the warm pool (`go-runtime-pool`). That takes a moment if a sandbox is ready, and longer if the pool is cold.
- **Reuse:** every later call in the session reuses the pod.
  - Before each `execute_go_code` call, `/app` is wiped. The Go module and build caches survive, so packages a session has already compiled stay fast.
  - `lookup_package_docs` runs in the same pod, without wiping anything.
- **One call at a time:** calls within a session run in order. Separate sessions run in parallel, each in its own pod.
- **Recovery:** if the pod dies mid-call, for example on eviction, kode-gopher claims a new one and retries the call once.

## Local (stdio)

One `kode-gopher serve` process is one MCP session. The sandbox is released when the client disconnects. If the process is killed instead (a crashed client, a closed laptop), its claim's lease runs out and the sandbox goes away within `--claim-lease` (10 minutes). `kode-gopher exec` works the same way.

The lease is renewed only while the process runs. A laptop asleep for longer than the lease loses its sandbox too, and the next call starts in a new one, without the old `/app` and caches. The expiry is computed from the local clock, so a clock running far behind the cluster's shortens it.

Two flags change this for development:
- `--claim` reattaches to an existing claim. Without `--persistent`, the reattached claim takes the lease and is deleted on exit, like a new one. If that sandbox dies mid-session, kode-gopher tries the claim again, then carries on in a new sandbox.
- `--persistent` leaves the sandbox running on exit, so it can be reattached later. A persistent sandbox has no lease: kode-gopher clears any expiry the claim had, so it stays until you delete it. `exec --keep` is the same.

## Shared server (HTTP)

Each `Mcp-Session-Id` is a session with its own sandbox.

| Setting | Default | Meaning |
|---|---|---|
| `--session-timeout` | 15 min | Sessions idle this long end, and their sandboxes are released. |
| `--max-sandboxes-per-user` | 2 | Open sandboxes allowed per user. A tool call that would open another one fails with "too many open sandboxes for this user" until one of the user's sessions ends. |
| `--claim-lease` | 10 min | Each claim carries a lease that kode-gopher renews while the session lives. If the server crashes, its claims expire and the sandboxes go away. Local sessions have the same lease. |

A client can end its session explicitly with HTTP `DELETE`, which releases the sandbox at once. kode-gopher runs as a single replica, and sessions live in its memory, so a restart ends open sessions. Clients start new ones.

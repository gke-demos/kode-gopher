---
title: How it works
description: What happens between the model writing a Go snippet and the result coming back.
---

kode-gopher is an [MCP](https://modelcontextprotocol.io) server. An MCP client, such as Claude Code, connects to it and offers its tools to the model. When the model calls `execute_go_code`, the snippet goes through four steps.

```
MCP client ──► kode-gopher serve ──► sandbox pod (agent-sandbox, POST /run-go)
                1. normalize          2. reset /app (keeping /app/persist) + go build
                                      3. run, with short-lived credentials
                4. result ◄────────── result.json, stdout, stderr (+ attachments)
```

## 1. Normalize

The model can send a **snippet** (any package name except `main`, declaring `func run(ctx context.Context) (any, error)`) or a **full program** (`package main` with `func main()`). For a snippet, kode-gopher adds a generated `main` that:
- calls `run`;
- JSON-encodes whatever it returns;
- catches errors and panics.

The model can also send several files, including helper packages.

## 2. Build

The sandbox is a pod from a warm pool, already running with the Go toolchain. In a single `POST /run-go` request, the sandbox server wipes `/app` (preserving `/app/persist`), writes the files atomically, bootstraps `go.mod` from the image's lockfile, and runs `go build` before any runtime credentials or environment variables are exposed.

The image has the [precompiled packages](/reference/packages/) in its build cache, so a snippet that sticks to them compiles in about two seconds on GKE. Any other pure-Go import works too, at the cost of a `go mod tidy` and a slower first build.

If the build fails, the result says `phase: build` and carries the compiler's errors. The model knows to fix its code, not to retry.

## 3. Run

The binary runs in the same pod immediately after a clean build. How it gets Google credentials depends on how kode-gopher is set up:
- **On your machine,** your own Application Default Credentials are copied in for the run (or minted as a short-lived token with `--credentials=access-token` or `--credentials=service`).
- **On a shared server,** a metadata server emulator inside the sandbox hands the program a short-lived access token for the duration of the run only. That's the signed-in user's token, a service account's, or kode-gopher's own, optionally restricted to read-only OAuth scopes with `--read-only` or per-call `read_only: true`.

Either way, the Google Cloud libraries find the credentials on their own, and the snippet just calls `storage.NewClient(ctx)`. See [Credentials](/concepts/credentials/).

## 4. Return a result

The program's return value, or its error or panic, comes back in the same response as a structured result, alongside stdout, stderr, the exit code, timings, and any `image/png`, `image/jpeg`, or `image/webp` files written under `/app/.kode-gopher/attachments/` (returned as MCP `ImageContent` blocks):

```json
{
  "phase": "run",
  "mode": "wrapped",
  "exit_code": 0,
  "build_ms": 1325,
  "duration_ms": 1770,
  "tidied": false,
  "result": { "kind": "ok", "value": { "clusters": [ ... ] } }
}
```

See [Snippets and results](/concepts/snippets-and-results/) for every field and result kind.

## Sessions

Each MCP session holds one sandbox for its whole life:
- The first call claims a pod from the warm pool.
- Later calls reuse it. Each call starts with a clean `/app` except `/app/persist`, which keeps session scratch files across calls alongside the module and build caches, so repeated imports and multi-turn investigations stay fast.
- Calls in a session run one at a time.
- If the pod dies mid-call, kode-gopher claims a new one and retries once.

The sandbox is released when the session ends. See [Sessions and sandboxes](/concepts/sessions/).

## Two ways to run kode-gopher

| | On your machine | Shared, in a GKE cluster |
|---|---|---|
| Transport | stdio: your MCP client starts `kode-gopher serve` | streamable HTTP at `https://<host>/mcp` |
| Sandboxes | in a kind or GKE cluster your kubectl reaches | in the same cluster as kode-gopher |
| Snippets run as | you | each signed-in Google user, a service account, or kode-gopher's own identity |
| Guide | [Getting started](/getting-started/) | [Deploy for a team](/deploy/) |

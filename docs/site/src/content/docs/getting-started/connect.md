---
title: Connect your MCP client
description: Point Claude Code, Claude Desktop or another MCP client at kode-gopher over stdio.
sidebar:
  order: 3
---

## Claude Code

```bash
claude mcp add kode-gopher -- kode-gopher serve --context=<context> --namespace=<namespace>
```

## Claude Desktop and other clients

Most clients take a JSON config like this (`claude_desktop_config.json` for Claude Desktop):

```json
{
  "mcpServers": {
    "kode-gopher": {
      "command": "kode-gopher",
      "args": ["serve", "--context=<context>", "--namespace=<namespace>"]
    }
  }
}
```

Settings:
- **Command:** use the full path, typically `~/go/bin/kode-gopher`, if your client doesn't inherit your `PATH`.
- **`--context`:** leave it out to use your current kubectl context.
- **`--namespace`:** `default` on kind, `codemode` with the GKE overlay.

## Try it

The server offers three [tools](/reference/tools/): `execute_go_code`, `gcp_auth_status` and `lookup_package_docs`. Their descriptions tell the model which packages are precompiled and how to write a snippet, so you can just ask:

> Use execute_go_code to list the GKE clusters in my project.

Ask it to call `gcp_auth_status` to see who snippets run as.

## Credentials

In this mode kode-gopher copies your ADC file (`~/.config/gcloud/application_default_credentials.json`) and `GOOGLE_CLOUD_PROJECT` into the sandbox for each run. **Snippets can do anything your account can do.** Treat the model's code as running as you, because it is.

With no ADC file, snippets run without credentials and `gcp_auth_status` reports `mode=none`. More in [Credentials](/concepts/credentials/).

## Troubleshooting

- **Stuck at "opening sandbox".** The warm pool has no ready sandbox. Check `kubectl -n <namespace> get sandboxes,pods`. On GKE, a fresh pool waits for nodes to scale up and the image to pull, which takes a few minutes. On kind, check that the image was loaded.
- **`ErrImagePull` on GKE right after updating your checkout.** The overlay pins a sandbox image tag that CI publishes when a change merges. For a few minutes after a merge that changed the image, the tag may not exist yet.
- **Slow builds with `tidied: true`.** The snippet imports a package outside the [precompiled set](/reference/packages/), so the build ran `go mod tidy` first. It works, but more slowly. Later calls in the same session are faster.
- **Permission errors from Google APIs.** These come from your own account. Compare `gcp_auth_status` with `gcloud auth list`, and check `GOOGLE_CLOUD_PROJECT`.

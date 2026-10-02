---
title: Getting started
description: Run kode-gopher on your own machine as a local MCP server.
sidebar:
  label: Overview
  order: 0
---

This section runs kode-gopher on your machine. Your MCP client (Claude Code, Claude Desktop, Gemini CLI) starts `kode-gopher serve` as a subprocess over stdio. Each snippet the model writes is built and run in a sandbox pod on a Kubernetes cluster that your kubectl can reach. Snippets use your own Google credentials.

To run kode-gopher as a shared server for a team instead, see [Deploy for a team](/deploy/).

## What you need

- **Go** 1.26 or later, to install the CLI. Go downloads the exact toolchain version it needs.
- **A Kubernetes cluster with [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox):** a local kind cluster (Docker required), or GKE Autopilot with the agent-sandbox addon. [Set up a sandbox cluster](/getting-started/sandbox-cluster/) covers both.
- **`kubectl`**, with a context for that cluster and permission to create SandboxClaims and port-forward in its namespace.
- **`gcloud`**, signed in to Application Default Credentials (ADC):
  ```bash
  gcloud auth application-default login
  export GOOGLE_CLOUD_PROJECT=<your-project>
  ```

## Steps

1. [Install the CLI](/getting-started/install/).
2. [Set up a sandbox cluster](/getting-started/sandbox-cluster/), and check it from the command line.
3. [Connect your MCP client](/getting-started/connect/), and ask the model to use Google Cloud.

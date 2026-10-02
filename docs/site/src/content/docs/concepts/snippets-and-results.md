---
title: Snippets and results
description: The two shapes of code execute_go_code accepts, and how outcomes come back.
sidebar:
  order: 1
---

## Snippets

A snippet is a file in any package except `main` that declares exactly this function:

```go
func run(ctx context.Context) (any, error)
```

kode-gopher adds the `main` function. It calls `run`, encodes the returned value as JSON, and turns a returned error or a panic into a structured result. The snippet never writes `main`, and doesn't print its answer: it returns it.

```go
package snippet

import (
	"context"
	"os"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
)

func run(ctx context.Context) (any, error) {
	c, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	resp, err := c.ListClusters(ctx, &containerpb.ListClustersRequest{
		Parent: "projects/" + os.Getenv("GOOGLE_CLOUD_PROJECT") + "/locations/-",
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, cl := range resp.Clusters {
		names = append(names, cl.Name)
	}
	return names, nil
}
```

## Full programs

A file in `package main` with its own `func main()` runs as is. The program owns stdout and stderr. For a structured result, it writes the same JSON shape the wrapper writes to `/app/.kode-gopher/result.json`:

```json
{"kind": "ok", "value": ...}
```

## Several files

The `files` argument takes a map from path to source, instead of `code`:
- Files at the root form the entry package. For a snippet, exactly one of them declares `run`.
- Files in subdirectories are helper packages. The module is named `kode_gopher_user`, so `helper/format.go` is imported as `kode_gopher_user/helper`.
- A `go.mod` in the map replaces the image's lockfile. The snippet then chooses its own versions, and pays for any recompilation.

## Imports

Packages in the [precompiled set](/reference/packages/) build in seconds. Any other pure-Go package works too: the build runs `go mod tidy` first, and the result reports `tidied: true`.

`extra_imports` adds blank imports through a generated file. It's rarely needed, as a hint for `go mod tidy`.

## Results

Every call returns:

| Field | Meaning |
|---|---|
| `phase` | `build` if compilation failed, with the compiler's errors in `stderr`. `run` if it compiled and ran. |
| `mode` | `wrapped` for a snippet, `verbatim` for a full program. |
| `exit_code` | The compiler's exit code (build) or the program's (run). |
| `stdout`, `stderr` | Output of the phase that ran last. Snippets can log to stdout freely, since the answer travels separately. |
| `build_ms`, `duration_ms` | Build time, and build plus run time. |
| `tidied` | Whether the build needed `go mod tidy`. |
| `result` | The structured result, below. |

The `result.kind` is one of:

| Kind | When | Other fields |
|---|---|---|
| `ok` | `run` returned a value | `value`: the value, as JSON |
| `error` | `run` returned an error | `message` |
| `panic` | `run` panicked | `message`, `stack` |
| `marshal_error` | the value couldn't be encoded as JSON (a channel or a func, for example) | `message`, `type` |

The MCP response is flagged as an error (`isError`) when the build failed, the program exited non-zero, or the result kind isn't `ok`. The model can tell what to fix from `phase` and `kind`: compile errors mean changing the code, and `error` results usually mean a permission or API problem.

The full schema is on the [MCP tools](/reference/tools/) page.

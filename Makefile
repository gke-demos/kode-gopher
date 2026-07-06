# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

.PHONY: prompts prompts-check test build

# prompts regenerates internal/prompts/{system.md,description.go} from
# internal/curated.Packages. Run after touching the curated set so the
# LLM-facing prompt and the execute_go_code tool description stay in
# sync with what the sandbox actually caches.
prompts:
	go generate ./internal/curated

# prompts-check fails if the committed generated files are out of sync
# with the current curated set. Suitable for CI.
prompts-check: prompts
	@if ! git diff --exit-code internal/prompts/; then \
		echo ""; \
		echo "internal/prompts/ is out of date. Run 'make prompts' and commit the result."; \
		exit 1; \
	fi

# test runs the full unit-test suite.
test:
	go test ./...

# build compiles all binaries (kode-gopher, mcp-smoketest).
build:
	go build ./...

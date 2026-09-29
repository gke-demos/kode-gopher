# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

.PHONY: prompts prompts-check test build sandbox-tag sandbox-pin sandbox-pin-check

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

# The sandbox image is published by CI under a tag derived from its inputs
# (sandbox/, internal/prewarm/). The GKE overlay pins that tag.
SANDBOX_REPO := ghcr.io/gke-demos/kode-gopher/sandbox
GKE_OVERLAY := manifests/overlays/gke/kustomization.yaml

# sandbox-tag prints the tag the current sources build to.
sandbox-tag:
	@scripts/sandbox-image-tag.sh

# sandbox-pin points the GKE overlay at the current tag. Run after
# touching sandbox/ or internal/prewarm/, and commit the result.
sandbox-pin:
	sed -i 's|$(SANDBOX_REPO):[A-Za-z0-9._-]*|$(SANDBOX_REPO):'"$$(scripts/sandbox-image-tag.sh)"'|' $(GKE_OVERLAY)

# sandbox-pin-check fails if the GKE overlay doesn't pin the current tag.
# Suitable for CI.
sandbox-pin-check:
	@tag=$$(scripts/sandbox-image-tag.sh); \
	if ! grep -q "$(SANDBOX_REPO):$$tag\b" $(GKE_OVERLAY); then \
		echo "$(GKE_OVERLAY) doesn't pin $(SANDBOX_REPO):$$tag."; \
		echo "Run 'make sandbox-pin' and commit the result."; \
		exit 1; \
	fi

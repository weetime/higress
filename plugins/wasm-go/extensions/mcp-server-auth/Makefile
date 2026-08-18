# MCP Server Auth Plugin Makefile
# Wraps the shared plugins/wasm-go Makefile with this plugin's registry path.

PLUGIN_NAME := mcp-server-auth
# Absolute dir of this Makefile, so VERSION resolves under `make -C` / `make -f`.
PLUGIN_DIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
# All rise plugins are published under the camp/higress/ prefix —
# camp/higress/mcp-server-auth. The deployed WasmPlugin CR and helm-camp
# plugins.properties both reference that exact path, so do NOT "normalise"
# the higress/ segment away.
REGISTRY ?= quanzhenglong.com/camp/higress
# Single source of truth: the VERSION file (bare, no leading v). Keep it in sync
# with higress.io/wasm-plugin-version in wasmplugin.yaml.
VERSION ?= v$(shell cat $(PLUGIN_DIR)/VERSION)
IMG ?= ${REGISTRY}/${PLUGIN_NAME}:${VERSION}

# Parent directory containing the main Makefile
PARENT_DIR := ../..

# The shared Dockerfile builds with this image; go.mod needs >= go1.24.4, and the
# default builder tag in that Dockerfile is go1.24.0 -> pin it here.
BUILDER ?= higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/wasm-go-builder:go1.24.4-oras1.0.0

.PHONY: help
help:
	@echo "MCP Server Auth Plugin Build Targets:"
	@echo "  make build        - Build the plugin wasm file into ./plugin.wasm"
	@echo "  make build-image  - Build the plugin OCI image"
	@echo "  make build-push   - Build and push the plugin OCI image"
	@echo "  make local-build  - Build the plugin locally without docker"
	@echo "  make test         - Run plugin tests"
	@echo ""
	@echo "Configuration:"
	@echo "  PLUGIN_NAME = ${PLUGIN_NAME}"
	@echo "  IMG         = ${IMG}"
	@echo ""
	@echo "Examples:"
	@echo "  make build-push VERSION=v0.0.12"

.PHONY: build
build:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) BUILDER=$(BUILDER) make build

.PHONY: build-image
build-image:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) BUILDER=$(BUILDER) make build-image

.PHONY: build-push
build-push:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) BUILDER=$(BUILDER) make build-push
	@echo ""
	@echo "✅ Successfully built and pushed: $(IMG)"

.PHONY: local-build
local-build:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) make local-build

.PHONY: test
test:
	go test -v ./...

.PHONY: clean
clean:
	rm -f plugin.wasm main.wasm coverage.out coverage.html
	@echo "✅ Cleaned build artifacts"

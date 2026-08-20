# Ext Header Enrich Plugin Makefile
# This Makefile simplifies building and pushing the ext-header-enrich plugin

# Plugin configuration
PLUGIN_NAME := ext-header-enrich
# Absolute dir of this Makefile, so VERSION resolves under `make -C` / `make -f`.
PLUGIN_DIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
REGISTRY ?= quanzhenglong.com/camp/higress
# Single source of truth: the VERSION file (bare, no leading v). Keep it in sync
# with higress.io/wasm-plugin-version in wasmplugin.yaml.
VERSION ?= v$(shell cat $(PLUGIN_DIR)/VERSION)
IMG ?= ${REGISTRY}/${PLUGIN_NAME}:${VERSION}

# Parent directory containing the main Makefile
PARENT_DIR := ../..

# The shared Dockerfile defaults to a go1.24.0 builder; go.mod needs >= go1.24.4,
# so pin it here.
BUILDER ?= higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/wasm-go-builder:go1.24.4-oras1.0.0
# Target platforms for the multi-arch image. The wasm payload is arch-neutral,
# these only fill in the OCI index so any node arch can pull it.
PLATFORMS ?= linux/amd64,linux/arm64
GOPROXY := $(shell go env GOPROXY)

# Default target
.PHONY: help
help:
	@echo "Ext Header Enrich Plugin Build Targets:"
	@echo "  make build        - Build the plugin wasm file"
	@echo "  make build-image  - Build the plugin docker image"
	@echo "  make build-push   - Build and push the plugin docker image"
	@echo "  make build-multiarch      - Build a multi-arch image into ./ext-header-enrich-oci.tar (no push)"
	@echo "  make build-push-multiarch - Build and push the multi-arch image ($(PLATFORMS))"
	@echo "  make local-build  - Build the plugin locally without docker"
	@echo "  make test         - Run plugin tests"
	@echo ""
	@echo "Configuration:"
	@echo "  PLUGIN_NAME = ${PLUGIN_NAME}"
	@echo "  IMG         = ${IMG}"
	@echo "  VERSION     = ${VERSION}"
	@echo ""
	@echo "Examples:"
	@echo "  make build-push"
	@echo "  make build-push VERSION=latest"
	@echo "  make build-push IMG=myregistry.com/ext-header-enrich:latest"

# Build the plugin wasm file
.PHONY: build
build:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) BUILDER=$(BUILDER) make build

# Build the plugin docker image
.PHONY: build-image
build-image:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) BUILDER=$(BUILDER) make build-image

# Build and push the plugin docker image
.PHONY: build-push
build-push:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) BUILDER=$(BUILDER) make build-push
	@echo ""
	@echo "✅ Successfully built and pushed: $(IMG)"

# Build the multi-arch image locally (OCI tarball, no push).
# A multi-platform result cannot be loaded into the local docker image store,
# so it goes to a tarball you can inspect before pushing.
.PHONY: build-multiarch
build-multiarch:
	cd $(PARENT_DIR) && docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg PLUGIN_NAME=$(PLUGIN_NAME) \
		--build-arg BUILDER=$(BUILDER) \
		--build-arg GOPROXY=$(GOPROXY) \
		-f Dockerfile.multiarch \
		-t $(IMG) \
		--provenance=false \
		--sbom=false \
		--output type=oci,dest=$(PLUGIN_DIR)/$(PLUGIN_NAME)-oci.tar \
		.
	@echo ""
	@echo "✅ OCI tarball: $(PLUGIN_DIR)/$(PLUGIN_NAME)-oci.tar"

# Build and push the multi-arch image (single OCI index covering $(PLATFORMS))
.PHONY: build-push-multiarch
build-push-multiarch:
	cd $(PARENT_DIR) && docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg PLUGIN_NAME=$(PLUGIN_NAME) \
		--build-arg BUILDER=$(BUILDER) \
		--build-arg GOPROXY=$(GOPROXY) \
		-f Dockerfile.multiarch \
		-t $(IMG) \
		--provenance=false \
		--sbom=false \
		--push \
		.
	@echo ""
	@echo "✅ Successfully built and pushed multi-arch: $(IMG)"

# Build the plugin locally without docker
.PHONY: local-build
local-build:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) make local-build

# Run tests
.PHONY: test
test:
	go test -v ./...

# Run tests with coverage
.PHONY: test-coverage
test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Clean build artifacts
.PHONY: clean
clean:
	rm -f plugin.wasm main.wasm $(PLUGIN_NAME)-oci.tar
	rm -f coverage.out coverage.html
	@echo "✅ Cleaned build artifacts"


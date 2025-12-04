# Model Auth Plugin Makefile
# This Makefile simplifies building and pushing the model-auth plugin

# Plugin configuration
PLUGIN_NAME := model-auth
REGISTRY ?= quanzhenglong.com/camp/
VERSION ?= latest
IMG ?= ${REGISTRY}${PLUGIN_NAME}:${VERSION}

# Parent directory containing the main Makefile
PARENT_DIR := ../..

# Default target
.PHONY: help
help:
	@echo "Model Auth Plugin Build Targets:"
	@echo "  make build        - Build the plugin wasm file"
	@echo "  make build-image  - Build the plugin docker image"
	@echo "  make build-push   - Build and push the plugin docker image"
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
	@echo "  make build-push IMG=myregistry.com/model-auth:latest"

# Build the plugin wasm file
.PHONY: build
build:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) make build

# Build the plugin docker image
.PHONY: build-image
build-image:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) make build-image

# Build and push the plugin docker image
.PHONY: build-push
build-push:
	cd $(PARENT_DIR) && PLUGIN_NAME=$(PLUGIN_NAME) IMG=$(IMG) make build-push
	@echo ""
	@echo "✅ Successfully built and pushed: $(IMG)"

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
	rm -f plugin.wasm main.wasm
	rm -f coverage.out coverage.html
	@echo "✅ Cleaned build artifacts"

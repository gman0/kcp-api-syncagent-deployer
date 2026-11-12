# Image / chart coordinates.
REGISTRY        ?= localhost:5001
IMAGE_NAME      ?= api-syncagent-deployer
IMAGE_TAG       ?= latest
IMAGE           ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

CHART_DIR       ?= charts/api-syncagent-deployer
CHART_OUT       ?= bin/charts
CHART_REPO      ?= oci://$(REGISTRY)/charts

BIN_DIR         ?= bin
BINARY          ?= $(BIN_DIR)/deployer

# Prefer podman, fall back to docker.
CONTAINER_TOOL  ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)
# Insecure push to local registry (http) — adjust for real registries.
PUSH_FLAGS      ?= $(if $(findstring podman,$(CONTAINER_TOOL)),--tls-verify=false,)

GO              ?= go

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  %-20s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the deployer binary into bin/.
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd

.PHONY: image
image: check-container-tool ## Build the container image (podman or docker).
	$(CONTAINER_TOOL) build -t $(IMAGE) -f Dockerfile .

.PHONY: image-push
image-push: check-container-tool ## Push the container image to $(REGISTRY).
	$(CONTAINER_TOOL) push $(PUSH_FLAGS) $(IMAGE)

.PHONY: chart-package
chart-package: ## Package the Helm chart into bin/charts/.
	@mkdir -p $(CHART_OUT)
	helm package $(CHART_DIR) --destination $(CHART_OUT)

.PHONY: chart-push
chart-push: chart-package ## Push the packaged chart to $(CHART_REPO).
	@pkg=$$(ls -1t $(CHART_OUT)/*.tgz | head -n1); \
	echo "Pushing $$pkg to $(CHART_REPO)"; \
	helm push "$$pkg" $(CHART_REPO) --plain-http

.PHONY: clean
clean: ## Remove build artefacts.
	rm -rf $(BIN_DIR)

.PHONY: check-container-tool
check-container-tool:
	@if [ -z "$(CONTAINER_TOOL)" ]; then \
		echo "Neither podman nor docker found in PATH" >&2; exit 1; \
	fi
	@echo "Using container tool: $(CONTAINER_TOOL)"

.PHONY: fullbuild
fullbuild: image image-push chart-package chart-push

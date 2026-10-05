SHELL := /bin/bash

BINARY      := docker-hoster-injector
PKG         := github.com/mint/docker-hoster-injector
CMD         := ./cmd/$(BINARY)
BIN_DIR     := bin
DIST_DIR    := dist
IMAGE       ?= docker-hoster-injector
TAG         ?= dev
GO          ?= go
GOFLAGS     ?=
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     ?= -s -w -X main.version=$(VERSION)
BUILDFLAGS  := -trimpath -ldflags '$(LDFLAGS)'

# Colours are disabled when not attached to a terminal.
ifneq ($(shell test -t 1 && echo tty),tty)
  BOLD := ; SGR0 := ; GRN := ; YLW := ; RED := ; DIM := ; RST :=
else
  BOLD := $(shell tput bold); SGR0 := $(shell tput sgr0)
  GRN := $(shell tput setaf 2); YLW := $(shell tput setaf 3)
  RED := $(shell tput setaf 1); DIM := $(shell tput dim); RST := $(shell tput sgr0)
endif

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "$(BOLD)docker-hoster-injector$(SGR0)"
	@echo
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  $(BOLD)%-18s$(SGR0) %s\n", $$1, $$2}'
	@echo

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

.PHONY: build
build: ## Compile the binary into ./bin
	@echo "$(DIM)build$(RST) $(BINARY)"
	@$(GO) build $(GOFLAGS) $(BUILDFLAGS) -o $(BIN_DIR)/$(BINARY) $(CMD)
	@echo "$(GRN)OK$(RST) $(BIN_DIR)/$(BINARY)"

.PHONY: run
run: build ## Run the binary locally
	@$(BIN_DIR)/$(BINARY)

.PHONY: fmt
fmt: ## Format the source tree
	@$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	@$(GO) vet ./...

.PHONY: lint
lint: ## Run staticcheck when available, always run vet
	@$(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "$(YLW)WARN$(RST) staticcheck not installed, skipping"; \
	fi

.PHONY: test
test: ## Run unit tests
	@$(GO) test $(GOFLAGS) ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector
	@$(GO) test $(GOFLAGS) -race -count=1 ./...

.PHONY: cover
cover: ## Run unit tests and report coverage
	@$(GO) test $(GOFLAGS) -covermode=atomic -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -n 1

.PHONY: test-integration
test-integration: build ## Run the acceptance tests against the local Docker daemon
	@echo "$(DIM)integration$(RST) requires a Docker daemon and will provision containers"
	@$(GO) test $(GOFLAGS) -tags=integration -count=1 -timeout=20m -v ./test/integration/...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	@$(GO) mod tidy

.PHONY: image
image: ## Build the container image
	@docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .
	@echo "$(GRN)OK$(RST) $(IMAGE):$(TAG)"

.PHONY: clean
clean: ## Remove build artifacts
	@rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out
	@echo "$(GRN)OK$(RST) clean"

.PHONY: ci
ci: lint test-race ## What CI runs on the fast lane

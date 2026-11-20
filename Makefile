MODULE  := github.com/cardeahq/cardea-gateway
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X $(MODULE)/internal/cli.version=$(VERSION) \
           -X $(MODULE)/internal/cli.commit=$(COMMIT) \
           -X $(MODULE)/internal/cli.date=$(DATE)

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the gateway binary into bin/gateway
	go build -ldflags "$(LDFLAGS)" -o bin/gateway ./cmd/gateway

.PHONY: run
run: ## Run the gateway locally (go run ... serve)
	go run ./cmd/gateway serve

# Docker: the image holds only the binary. The repo is mounted at /app so
# ./cardea-gateway.yaml and its relative configs/ paths resolve as with
# `make run`; provider API keys are read from .env when present.
IMAGE ?= cardea-gateway:dev
PORT  ?= 8080

.PHONY: docker-build
docker-build: ## Build the gateway Docker image (IMAGE=cardea-gateway:dev)
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE) .

.PHONY: up
up: docker-build ## Build the image and run the gateway in Docker (PORT=8080)
	docker run --rm -it \
		-p $(PORT):8080 \
		-v "$(CURDIR)":/app \
		$(if $(wildcard .env),--env-file .env) \
		-e ANTHROPIC_API_KEY -e MISTRALAI_API_KEY \
		$(IMAGE)

.PHONY: test
test: ## Run unit tests
	go test ./...

# The policy and gateway packages run evaluations in goroutines and swap
# policies at runtime. The policy package takes about two minutes under
# -race, so this is not part of `make check`.
.PHONY: test-race
test-race: ## Run policy and gateway tests with the race detector (slow)
	go test -race ./internal/policy/... ./internal/gateway/...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

# golangci-lint must load packages with the Go version it was built with;
# pin lint to go.mod's version, the one CI uses, whatever the local default.
GO_MOD_VERSION := $(shell go list -m -f '{{.GoVersion}}')

.PHONY: lint
lint: ## Run golangci-lint
	GOTOOLCHAIN=go$(GO_MOD_VERSION) golangci-lint run ./...

.PHONY: fmt
fmt: ## Format Go sources in place
	gofmt -l -w .

.PHONY: fmt-check
fmt-check: ## Fail if any Go source needs gofmt
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: check
check: build vet fmt-check lint test ## Build, vet, fmt-check, lint and test (what CI runs)

.PHONY: vulncheck
vulncheck: ## Scan dependencies with govulncheck
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin

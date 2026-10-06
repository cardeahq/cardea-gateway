MODULE  := github.com/agentkeeper/agentkeeper
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X $(MODULE)/internal/cli.version=$(VERSION) \
           -X $(MODULE)/internal/cli.commit=$(COMMIT) \
           -X $(MODULE)/internal/cli.date=$(DATE)

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o bin/agentkeeper ./cmd/agentkeeper

.PHONY: run
run:
	go run ./cmd/agentkeeper

.PHONY: test
test:
	go test ./...

# The policy and gateway packages run evaluations in goroutines and swap
# policies at runtime. The policy package takes about two minutes under
# -race, so this is not part of `make check`.
.PHONY: test-race
test-race:
	go test -race ./internal/policy/... ./internal/gateway/...

.PHONY: vet
vet:
	go vet ./...

# golangci-lint must load packages with the Go version it was built with;
# pin lint to go.mod's version, the one CI uses, whatever the local default.
GO_MOD_VERSION := $(shell go list -m -f '{{.GoVersion}}')

.PHONY: lint
lint:
	GOTOOLCHAIN=go$(GO_MOD_VERSION) golangci-lint run ./...

.PHONY: fmt
fmt:
	gofmt -l -w .

.PHONY: fmt-check
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: check
check: build vet fmt-check lint test

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin

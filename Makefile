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

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: fmt
fmt:
	gofmt -l -w .

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin

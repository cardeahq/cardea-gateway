# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

agentkeeper is an LLM gateway written in Go. It sits between applications and
LLM providers to add security, privacy, observability, and easy provider
switching. Behavior is driven by user-configurable rules (e.g. circuit
breakers, token compaction) — the rule engine is not yet implemented.

Module: `github.com/agentkeeper/agentkeeper`. Requires Go 1.26+.

## Commands

```sh
make build   # build bin/agentkeeper, with version info injected via -ldflags
make run     # go run ./cmd/agentkeeper
make test    # go test ./...
make lint    # golangci-lint run ./...
make fmt     # gofmt -l -w .
make tidy    # go mod tidy
```

Run a single test package/test: `go test ./internal/config/... -run TestName -v`.

Run the gateway directly: `go run ./cmd/agentkeeper serve [--config path.yaml]`.

## Architecture

- `specs/` — functional and technical specifications. Not code; the
  source of intent for behavior, including features not implemented yet.
  Flat layout, `NNNN-<feature>-{functional,technical}.md`, where `NNNN`
  numbers the feature rather than the document — both halves of a feature
  share it (see the `new-func-spec` and `new-tech-spec` skills).
- `cmd/agentkeeper/main.go` — entrypoint; delegates immediately to `internal/cli`.
- `internal/cli` — Cobra command tree (`root.go`, `serve.go`, `version.go`).
  Persistent `--config` flag lives on the root command; each subcommand's
  `RunE` calls `loadConfig()` and constructs its own logger/dependencies
  rather than relying on shared global state.
- `internal/config` — the `Config` struct is the single source of truth for
  all configuration. `Load()` layers Viper sources in increasing priority:
  built-in defaults → YAML file (`--config`, or `./agentkeeper.yaml` /
  `$HOME/.config/agentkeeper` / `/etc/agentkeeper` if unset) → `AGENTKEEPER_`-
  prefixed env vars (dots become underscores, e.g. `AGENTKEEPER_SERVER_LISTEN_ADDR`).
  New config fields must be added to the `Config` struct tree, given a
  default in `defaults()`/`Load()`, and documented in
  `configs/agentkeeper.example.yaml`.
- `internal/logging` — builds the zap logger from `config.LoggingConfig`
  (`level`: debug/info/warn/error; `format`: console/json). This is the only
  place zap config translation happens.
- `internal/gateway` — the gateway server. Routes: `/healthz`, and `/`
  (everything else) forwarded as-is to the single configured provider's
  `Handler()`. There is no provider selector in the URL or request — the
  caller cannot choose or see which upstream is in use; agentkeeper
  decides that from config alone (see `internal/provider`). Request/
  response interception and rule evaluation will be added here. `Run(ctx)`
  blocks and shuts down gracefully on context cancellation (wired to
  SIGINT/SIGTERM in `internal/cli/serve.go` via `signal.NotifyContext`).
- `internal/provider` — the `Provider` interface (`Name() string`,
  `Handler() http.Handler`). `NewHTTPProxy` covers any upstream reachable
  by forwarding the request as-is; providers needing custom auth
  injection or request rewriting implement `Provider` directly (see the
  `add-provider` skill). Exactly one `Provider` is active per gateway
  instance, built from `config.Config.Provider` — Anthropic is the only
  one implemented so far. Future provider *switching* (as opposed to
  fixed per-deployment config) is expected to be rule-driven, not
  caller-driven.

Data flow for `agentkeeper serve`: `cli.serve` loads `config.Config` →
builds a `zap.Logger` from it → builds the configured `provider.Provider`
→ constructs a `gateway.Gateway` from all three → `Run`s it until
interrupted.

## Conventions

- Business logic lives under `internal/`; `cmd/agentkeeper` stays a thin
  entrypoint.
- Write the simplest implementation that solves the task: no speculative
  abstractions, no configurability beyond what's actually needed, no
  interfaces with a single implementation "for later." Prefer the boring,
  direct solution over a clever or general one.
- Every package has a one-line package-level doc comment stating its
  responsibility. Beyond that, default to no comments. Only add one when
  the code alone would surprise a reader — a non-obvious invariant, a
  behavioral contract not visible in the signature, a workaround, or valid
  values not encoded in the type (e.g. a string field that's really an
  enum). Never add a comment that just restates what the code already
  says (`// NewFoo builds a Foo` above `func NewFoo() Foo`).
- Commands build their own dependencies (logger, gateway) inside `RunE`
  rather than reaching for package-level singletons.
- Before implementing a feature, check `specs/` for a spec covering it and
  follow it. If the implementation ends up diverging from the spec, update
  the spec in the same change rather than leaving it stale.

## How to use AgentKeeper with OpenCode

1. Run agentkeeper with a Claude API key

```sh   
export ANTHROPIC_API_KEY=sk-ant-...   # in agentkeeper's environment only
agentkeeper serve --config ./agentkeeper.yaml
```

Listens on port 8080 by default.
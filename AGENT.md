# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Cardea Gateway is an LLM gateway written in Go. It sits between applications and
LLM providers to add security, privacy, observability, and easy provider
switching. Behavior is driven by user-configurable rules (e.g. circuit
breakers, token compaction) — the rule engine is not yet implemented.

Module: `github.com/cardeahq/cardea-gateway`. Requires Go 1.26+.

## Commands

```sh
make build   # build bin/gateway, with version info injected via -ldflags
make run     # go run ./cmd/gateway
make test    # go test ./...
make test-race # go test -race for internal/policy and internal/gateway (slow, ~2 min)
make lint    # golangci-lint run ./...
make fmt     # gofmt -l -w .
make tidy    # go mod tidy
```

Run a single test package/test: `go test ./internal/config/... -run TestName -v`.

Run the gateway directly: `go run ./cmd/gateway serve [--config path.yaml]`.

## Architecture

- `specs/` — functional and technical specifications. Not code; the
  source of intent for behavior, including features not implemented yet.
  Flat layout, `NNNN-<feature>-{functional,technical}.md`, where `NNNN`
  numbers the feature rather than the document — both halves of a feature
  share it (see the `new-func-spec` and `new-tech-spec` skills).
- `cmd/gateway/main.go` — entrypoint; delegates immediately to `internal/cli`.
- `internal/cli` — Cobra command tree (`root.go`, `serve.go`, `version.go`).
  Persistent `--config` flag lives on the root command; each subcommand's
  `RunE` calls `loadConfig()` and constructs its own logger/dependencies
  rather than relying on shared global state.
- `internal/config` — the `Config` struct is the single source of truth for
  all configuration. `Load()` layers Viper sources in increasing priority:
  built-in defaults → YAML file (`--config`, or `./cardea-gateway.yaml` /
  `$HOME/.config/cardea-gateway` / `/etc/cardea-gateway` if unset) → `CARDEA_GATEWAY_`-
  prefixed env vars (dots become underscores, e.g. `CARDEA_GATEWAY_SERVER_LISTEN_ADDR`).
  New config fields must be added to the `Config` struct tree, given a
  default in `defaults()`/`Load()`, and documented in
  `configs/cardea-gateway.example.yaml`.
- `internal/logging` — builds the zap logger from `config.LoggingConfig`
  (`level`: debug/info/warn/error; `format`: console/json). This is the only
  place zap config translation happens.
- `internal/gateway` — the gateway server. Routes: `/healthz`, and `/`
  (everything else) forwarded as-is to the single configured provider's
  `Handler()`. There is no provider selector in the URL or request — the
  caller cannot choose or see which upstream is in use; the Gateway
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

- `internal/pipeline` — runs each proxied call through the fixed stage order
  (identification, tracing, policies); each stage is enabled by
  `pipeline.stages.<name>`. `internal/policy` owns embedded OPA, the Rego
  policy and data files and the input document; `internal/gateway/policies.go`
  adapts it to a stage. `SIGHUP` (or `policies.watch`) reloads the policy.

- `schemas/` — JSON Schemas of the trace record (`trace-record.v1`) and of
  one line of a Cardea batch (`cardea-intake-line.v1`). Hand-written, the
  public contract with the platform and operators' tooling: when a record
  field changes in `internal/trace/record.go`, change the schema in the
  same commit (`internal/trace/schema_test.go` fails the build if they
  drift).

- `internal/trace` — builds the call-trace records (specs/0003) and hands
  each one to the trace sink. `LogSink` appends JSON Lines to stdout or a
  file; `CardeaSink` (specs/0005) queues records and a background
  goroutine delivers them in gzip NDJSON batches to the Cardea platform
  over HTTPS, numbering them per instance so loss shows as a gap;
  `MultiSink` writes to both when `tracing.sink.type` is `logging+cardea`.
  `Write` never touches the network. **A sink never refuses a call:** a
  sink error is logged at error level and the record is lost (specs/0005
  amends 0003); only a failure of the tracing stage itself, such as an
  unreadable `tool_result`, still denies a request. The Cardea sink needs
  `tracing.sink.cardea.endpoint` and `api_key_env` (the name of the env
  var holding the API key, never the key itself), neither with a default;
  there is no tenant in the Gateway, the platform derives it from the key.

Data flow for `gateway serve`: `cli.serve` loads `config.Config` →
builds a `zap.Logger` from it → builds the configured `provider.Provider`
→ constructs a `gateway.Gateway` from all three → `Run`s it until
interrupted.

## Conventions

- Business logic lives under `internal/`; `cmd/gateway` stays a thin
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

## Spec-driven workflow

Agents and skills live in `.claude/`:

- Skills: `new-func-spec`, `new-tech-spec`, `spec-review`, `code-review`,
  `security-review`, `simplify`.
- Agents, one human gate between each: `spec-test-writer` (tests only),
  `spec-implementer` (never edits tests), `quality-gate` (PASS or FAIL).

Write the functional spec, then the technical spec, run `spec-review` on the
pair, then the three agents in order. `make check` must pass at each stage.

## GitHub-issue-triggered pipeline

The same workflow, started from a GitHub issue instead of a human writing the
specs, and run unattended between its two human gates. A scheduled Claude
Code session (`gh-issue-loop` skill, run via a Claude Code cron job — see
below) polls `cardeahq/cardea-gateway` issues and drives it.

```
issue, labeled spec-request
        │  issue-spec-writer agent: worktree + branch off main,
        │  drafts functional + technical spec from the issue, opens
        │  a draft PR ("Closes #N")
        ▼
issue labeled spec-ready  ──── HUMAN GATE 1: review the spec diff in
        │                      the draft PR, then label the issue
        │                      spec-approved (or leave PR review
        │                      comments and re-trigger spec-request)
        ▼
issue labeled spec-approved
        │  issue-pipeline agent: spec-test-writer → spec-implementer
        │  → quality-gate, back to back, no pause between them
        ▼
quality-gate PASS → PR marked "ready for review", issue labeled in-review
quality-gate FAIL → PR comment with the report, issue labeled blocked
        ▼
issue labeled in-review ──── HUMAN GATE 2: ordinary GitHub PR review
                              and merge. Nothing pipeline-specific here.
```

Labels, all on the **issue** (the PR itself only ever carries GitHub's
native review state):

| Label | Means | Who sets it |
| --- | --- | --- |
| `spec-request` | Please draft specs for this | human |
| `spec-in-progress` | Stage 0 running | agent |
| `needs-input` | Stage 0 stopped on a question only a human can answer — see the issue comment | agent |
| `spec-ready` | Draft spec PR posted, awaiting spec approval | agent |
| `spec-approved` | Specs approved, go implement | human |
| `pipeline-in-progress` | Stage 2 running | agent |
| `in-review` | quality-gate passed, PR is ready for normal review | agent |
| `blocked` | quality-gate failed, or stage 0/2 needs help | agent |

Worktrees live at `../cardea-gateway-worktrees/issue-<N>-<slug>`, one per
issue, branch `issue-<N>-<slug>`, both derived from the issue number and
title so re-entry after `needs-input` or `blocked` reuses the same one
instead of creating a duplicate.

Unlike the interactive agents above, `issue-spec-writer` and `issue-pipeline`
never call `AskUserQuestion` — there's no one watching. `issue-spec-writer`
treats the issue body and comments as the interview; if something only a
human can decide is missing, it posts the question as an issue comment and
labels the issue `needs-input` instead of guessing. `issue-pipeline` chains
`spec-test-writer`, `spec-implementer`, and `quality-gate` without the pause
each normally expects between stages — the two human gates for this whole
pipeline are spec approval and the final PR review, not one per agent.

**Running it:** manually, ask Claude to use the `gh-issue-loop` skill.
Scheduled, register it as a Claude Code cron job (`CronCreate`) that invokes
that skill every tick; a 15–30 minute interval is reasonable since this is
polling, not latency-sensitive. Every `gh` call needs a valid token — if your
shell exports a stale `GITHUB_TOKEN`, it shadows `gh`'s own keyring login and
every call fails; the agents and skill here run `GITHUB_TOKEN= gh ...` to
sidestep that, but the cron job's own environment should not have one set
either.

## Vulnerability loop

A second, simpler loop off the same `gh-issue-loop` entry point: fixing a
`govulncheck` finding is a mechanical version bump, not a design decision,
so there's no spec stage and only one human gate — the PR review.

```
CI: govulncheck fails on a push to main
        │  files or updates one open issue, labeled vulnerability,
        │  with the full govulncheck output
        ▼
issue labeled vulnerability
        │  vuln-fixer agent: worktree + branch off main, re-runs
        │  govulncheck -json to map each reachable finding to a
        │  module bump or a Go toolchain bump, applies them,
        │  verifies with make check + a clean govulncheck re-run
        ▼
all findings cleared → PR opened directly, issue labeled in-review
some findings remain, or make check breaks → PR comment or issue
  comment with what's still open and why, issue labeled blocked
        ▼
issue labeled in-review ──── HUMAN GATE: ordinary GitHub PR review
                              and merge.
```

The `govulncheck` CI job (`.github/workflows/ci.yml`) has `issues: write`
scoped to that job only, and on failure (push to `main` only, never on a PR)
dedupes against any existing open `vulnerability` issue rather than filing a
new one each run. `vuln-fixer` reuses the `vulnerability` /
`vuln-in-progress` labels for its own claim, and the shared `in-review` /
`blocked` labels from the feature pipeline above for its outcome.

If a finding has no fix published upstream yet, `vuln-fixer` still opens a
PR for whatever it could fix and leaves the issue `blocked` rather than
retrying — a human re-adds `vulnerability` once a fix ships.

## How to use the Gateway with OpenCode

1. Run the Gateway with a Claude API key

```sh   
export ANTHROPIC_API_KEY=sk-ant-...   # in the Gateway's environment only
gateway serve --config ./cardea-gateway.yaml
```

Listens on port 8080 by default.
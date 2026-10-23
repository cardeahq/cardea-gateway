# Cardea Gateway

Cardea Gateway is an LLM gateway written in Go. It sits between your
application and an LLM provider as a drop-in HTTP proxy, adding security,
privacy, and observability without any change to the client:

- **PII filtering** — redacts email addresses, SSNs, and API keys from
  requests before they reach the provider.
- **Policy rules** — an [OPA](https://www.openpolicyagent.org/)/Rego policy
  decides whether each call is allowed.
- **Call tracing** — structured, prompt-free records of every request and
  response (tokens, timings, tool calls), to a log file or the Cardea
  platform.
- **Agent & caller identification** — recognizes known agent types (Claude
  Code, opencode, ...) and gates access with a shared-secret API key.
- **Provider abstraction** — callers never see or choose the upstream
  provider; it's fixed in config, so switching providers needs no client
  changes.

Anthropic is the only upstream provider implemented today; see
[`internal/provider`](internal/provider).

## Requirements

- Go 1.26+

## Quick start

```sh
make build        # produces bin/gateway
export ANTHROPIC_API_KEY=sk-ant-...
./bin/gateway serve --config configs/cardea-gateway.example.yaml
```

The gateway listens on `:8080` and forwards everything except `/healthz` to
the configured provider.

## Configuration

Configuration loads, in increasing priority, from built-in defaults, a YAML
file, and `CARDEA_GATEWAY_`-prefixed environment variables (dots become
underscores, e.g. `CARDEA_GATEWAY_SERVER_LISTEN_ADDR`). See
[`configs/cardea-gateway.example.yaml`](configs/cardea-gateway.example.yaml)
for every key, including the pipeline stages above
(`authorization`/`identification`/`tracing`/`policies`/`filtering`), each
independently toggleable. Point at a specific file with `--config`.

## Development

```sh
make test   # run tests
make lint   # golangci-lint
make fmt    # gofmt the tree
make check  # test + lint + fmt check — run before opening a PR
```

## Project layout

- `cmd/gateway` — thin entrypoint; all logic lives under `internal/`.
- `internal/cli` — command tree (`serve`, `version`).
- `internal/config` — config loading (see above).
- `internal/gateway` — the HTTP server and request pipeline.
- `internal/pipeline` — the fixed per-call stage order: authorization,
  identification, tracing, policies, filtering.
- `internal/policy` — embedded OPA, evaluates the Rego policy.
- `internal/provider` — the `Provider` interface and the Anthropic
  implementation.
- `internal/trace` — builds and ships call-trace records.
- `schemas/` — JSON Schemas for what the gateway emits (see below).
- `specs/` — functional and technical specs; the source of intent for
  behavior, including features not implemented yet.

See [AGENT.md](AGENT.md) for a deeper tour of the architecture and the
project's conventions.

## Contributing

Before writing code, check [`specs/`](specs/) for a spec covering the
behavior you're about to touch, and follow it. Each feature has a
functional spec (what it does, and for whom) and a technical spec (how it's
built), sharing a number and slug:
`NNNN-<feature>-{functional,technical}.md`. If your change ends up
diverging from its spec, update the spec in the same PR rather than
leaving it stale.

Otherwise, normal GitHub flow: fork or branch, make `make check` pass, open
a PR. See [AGENT.md](AGENT.md) for the full conventions used in this
codebase.

## JSON Schemas

[`schemas/`](schemas/) is the machine-readable contract of what the Gateway
emits (JSON Schema draft 2020-12):

- **[`trace-record.v1.schema.json`](schemas/trace-record.v1.schema.json)** —
  one call-trace record (a `request` or a `response`), as written by the
  log sink (JSON Lines) or sent by the Cardea sink.
- **[`cardea-intake-line.v1.schema.json`](schemas/cardea-intake-line.v1.schema.json)** —
  one line of the gzip NDJSON batch the Cardea sink POSTs to the platform:
  `{"seq": N, "record": <trace record>}`, numbered per instance so gaps and
  duplicates are detectable.

Notes for consumers:

- Every field is always present; one that doesn't apply is `null`, an
  empty list is `[]`.
- Within v1, fields and values may be added — ignore unknown properties
  and treat unknown enum-like values as opaque.
- Prompts, tool arguments, and tool results are never recorded: only
  sizes, argument names, and keyed fingerprints (`fp_…`).
- Validate with any 2020-12 validator, e.g.
  `check-jsonschema --schemafile schemas/cardea-intake-line.v1.schema.json line.json`.

## Usage examples

### opencode

[opencode](https://opencode.ai) talks to Anthropic like any other client —
point its provider `baseURL` at the Gateway instead:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": {
      "options": { "baseURL": "http://localhost:8080", "apiKey": "not-needed" }
    }
  }
}
```

The Gateway strips whatever credential the caller sends and injects the
real `ANTHROPIC_API_KEY` from its own environment, so opencode never needs
a valid Anthropic key of its own.

### Claude Code

Claude Code reads `ANTHROPIC_BASE_URL` at startup and sends every request
there instead of `api.anthropic.com`:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=not-needed
claude
```

(Or set both in the `env` block of `~/.claude/settings.json` to persist
across terminals.) Confirm it's routing through the Gateway with `/status`
— the `Anthropic base URL` line should read `http://localhost:8080`.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

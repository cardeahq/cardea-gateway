# Cardea Gateway

Cardea Gateway is an LLM gateway that shields and contains AI usage with
security, privacy, observability, and easy LLM provider switching. It sits
between applications and LLM providers, enforcing user-defined rules —
custom circuit breakers, token compaction, and more.

## Requirements

- Go 1.26+

## Build

```sh
make build
```

This produces `bin/gateway`.

## Run

```sh
make run
# or
go run ./cmd/gateway serve
```

## Configuration

Configuration is loaded (in increasing priority) from built-in defaults, a
YAML config file, and `CARDEA_GATEWAY_`-prefixed environment variables. See
[`configs/cardea-gateway.example.yaml`](configs/cardea-gateway.example.yaml) for
all available keys. Point at a specific file with `--config`:

```sh
gateway serve --config ./cardea-gateway.yaml
```

## Development

```sh
make test   # run tests
make lint   # run golangci-lint
make fmt    # gofmt the tree
```

## JSON Schemas

[`schemas/`](schemas/) holds the machine-readable contract of what the Gateway
emits (JSON Schema draft 2020-12). Two files:

- **[`trace-record.v1.schema.json`](schemas/trace-record.v1.schema.json)**: one
  call-trace record, as written by the log sink (JSON Lines) or sent by the
  Cardea sink. A call produces a `request` record (agent to LLM: model,
  stream flag, outcome, stage decisions, tool results received) and a
  `response` record (LLM to agent: timings, outcome, tokens, stop reason,
  tool calls requested). Both share `call_id`, `agent_id` and `session`.
- **[`cardea-intake-line.v1.schema.json`](schemas/cardea-intake-line.v1.schema.json)**:
  one line of the batch the Cardea sink POSTs to the platform: a gzip-compressed
  NDJSON body where each line is `{"seq": N, "record": <trace record>}`. `seq`
  numbers records per Gateway instance from 1, so a gap means lost records and
  a repeat means a duplicate. The instance identity travels in the
  `X-Cardea-Instance` header and the tenant is derived from the API key, so
  neither appears in the line.

### For users and integrators

- Every field is always present; one that does not apply is `null`, an empty
  list is `[]`.
- Within v1, fields and values may be added. Ignore unknown properties and
  treat unknown values (e.g. a new `finding.kind`) as opaque.
- Prompts, tool arguments and tool results are never recorded: only sizes,
  argument names and keyed fingerprints (`fp_…`).
- Validate a record or line with any 2020-12 validator, for example
  `check-jsonschema --schemafile schemas/cardea-intake-line.v1.schema.json line.json`.
  The intake schema references the trace schema by relative `$ref`, so keep
  the two files side by side.

### For developers

- The schemas are hand-written, not generated. When a field changes in
  `internal/trace/record.go`, update the schema in the same commit.
- Additive changes stay in v1. A breaking change (removing or retyping a
  field) needs a new `v2` file and a new `schema` constant; keep v1 until
  consumers have moved.
- Value sets are documented in `description`/`examples`, not enumerated with
  `enum`, so adding a value is not a breaking change.

## How features get built

Features here start as a written spec, not as code. [`specs/`](specs/) is
the source of intent — including for behavior that isn't implemented yet.
Before writing code for a feature, check whether a spec already covers it
and follow it.

Each feature has up to two specs, named
`NNNN-<feature>-{functional,technical}.md`. Both halves share the number
and the slug — the number identifies the feature, not the document:

| | Functional | Technical |
| --- | --- | --- |
| Answers | *what* it does, and for whom | *how* it is built |
| Written for | operators and client developers | whoever implements it |
| Contains | behavior rules, operator controls, Given/When/Then acceptance criteria | packages, Go signatures, config fields, failure modes |

The functional spec comes first; the technical spec designs against it.
Start from [`specs/TEMPLATE-functional.md`](specs/TEMPLATE-functional.md)
or [`specs/TEMPLATE-technical.md`](specs/TEMPLATE-technical.md), or use the
`new-func-spec` / `new-tech-spec` Claude Code skills to draft one.

Once both halves exist, the `spec-review` skill cross-checks them: does
building what the technical spec describes produce the behavior the
functional spec promises? Run it before the agents below. They treat the
specs as the contract, so a disagreement that survives this point becomes
a wrong test that the implementer is forbidden to correct.

### Building from a spec

Implementation runs as three Claude Code agents (defined in
`.claude/agents/`), in order, **with a human review between each**. No
agent starts the next one — you do, after reading its report.

**1. Tests.** Point `spec-test-writer` at the technical spec — it finds
the functional half itself, since they share a number and slug:

> Use the spec-test-writer agent to write the tests for
> specs/0001-staging-pipeline-technical.md

It writes tests and *no implementation*, so the tree does not compile when
it finishes. That is the expected end state: every build error should be
`undefined: <thing the spec says to build>`, and the agent reports that
list as the implementer's to-do.

Review its report before moving on — especially the ambiguities it says it
had to interpret. Those interpretations become requirements the next agent
is forbidden to correct, so this is the cheapest place to catch a
misunderstanding.

**Then commit the tests.** This isn't bookkeeping: stage 3 verifies that
the implementation didn't weaken the approved tests by diffing `*_test.go`
against `HEAD`. If the tests and the implementation land in one
uncommitted blob, that check can't tell them apart.

**2. Implementation.** Then:

> Use the spec-implementer agent to implement
> specs/0001-staging-pipeline-technical.md

Its one hard rule: it may not modify, skip, or weaken any test file. Where
the tests and the spec disagree, the tests win and it reports the
divergence. If it believes a test is wrong, it stops and asks rather than
working around it — which sends the work back to stage 1.

**3. Quality gate.** Finally:

> Use the quality-gate agent to verify
> specs/0001-staging-pipeline-technical.md

It runs unattended, changes nothing, and returns exactly `VERDICT: PASS` or
`VERDICT: FAIL` — never a qualified pass. Alongside the mechanical checks
(`build`, `vet`, `test`, `lint`, `gofmt`) it asks the question the other
two can't: *would this test survive a no-op implementation?* A green suite
of hollow tests is the failure this chain exists to catch.

On FAIL, route by the finding — hollow or missing tests go back to stage 1,
implementation defects to stage 2 — then re-run the gate.

### Why the gates are human

The agents are deliberately narrow: the test writer can't implement, the
implementer can't touch tests, the gate can't fix anything. That only works
if a person checks the handoff between them. The two decisions worth your
attention are whether the tests really pin down the spec's behavior (after
stage 1), and whether the code does what the spec says rather than merely
what makes the tests green (after stage 2).

One standing rule: if an implementation ends up diverging from its spec,
update the spec in the same change rather than leaving it stale.

## Usecases

### Use the Gateway with opencode

[opencode](https://opencode.ai) talks to Anthropic like any other client, so
pointing it at the Gateway instead is just a matter of redirecting its
Anthropic provider's `baseURL`.

1. Run the Gateway with a real Anthropic API key in its own environment —
   this key never needs to be shared with opencode or any other caller:

   ```sh
   export ANTHROPIC_API_KEY=sk-ant-...   # in the Gateway's environment only
   gateway serve --config ./cardea-gateway.yaml
   ```

   Listens on `:8080` by default.

2. Point opencode's Anthropic provider at the Gateway. Add this to your
   project's `opencode.json` (or the global `~/.config/opencode/opencode.json`):

   ```json
   {
     "$schema": "https://opencode.ai/config.json",
     "provider": {
       "anthropic": {
         "options": {
           "baseURL": "http://localhost:8080",
           "apiKey": "not-needed"
         }
       }
     }
   }
   ```

   The `apiKey` value is a placeholder — the Gateway strips whatever
   credential the caller sends and injects the real `ANTHROPIC_API_KEY`
   from its own environment (see `internal/provider/anthropic.go`), so
   opencode never needs a valid Anthropic key of its own.

3. Use opencode normally. Every request it sends now flows through
   the Gateway — check the gateway's logs for the `token usage` lines to
   see sent/received token counts per request.

### Use the Gateway with Claude Code

Claude Code reads `ANTHROPIC_BASE_URL` at startup and sends every request
there instead of `api.anthropic.com` — this is the same mechanism it uses
for any organization-run LLM gateway.

1. Run the Gateway with a real Anthropic API key in its own environment —
   this key never needs to be shared with Claude Code or any other caller:

   ```sh
   export ANTHROPIC_API_KEY=sk-ant-...   # in the Gateway's environment only
   gateway serve --config ./cardea-gateway.yaml
   ```

   Listens on `:8080` by default.

2. Point Claude Code at the Gateway by setting `ANTHROPIC_BASE_URL` and a
   credential variable in the shell you launch it from:

   ```sh
   export ANTHROPIC_BASE_URL=http://localhost:8080
   export ANTHROPIC_AUTH_TOKEN=not-needed
   claude
   ```

   `ANTHROPIC_AUTH_TOKEN` is a placeholder — the Gateway strips whatever
   credential the caller sends and injects the real `ANTHROPIC_API_KEY`
   from its own environment (see `internal/provider/anthropic.go`), so
   Claude Code never needs a valid Anthropic key of its own.

   To make this persist across terminals instead of exporting it each
   time, put both variables in the `env` block of `~/.claude/settings.json`
   (not a project's `.claude/settings.json`, which gets committed):

   ```json
   {
     "env": {
       "ANTHROPIC_BASE_URL": "http://localhost:8080",
       "ANTHROPIC_AUTH_TOKEN": "not-needed"
     }
   }
   ```

3. Confirm it's routing through the Gateway: send Claude Code a message,
   then run `/status` — the `Anthropic base URL` line should show
   `http://localhost:8080`. Check the Gateway's logs for the `token usage`
   lines to see sent/received token counts per request.

## License

Apache License 2.0 — see [LICENSE](LICENSE).


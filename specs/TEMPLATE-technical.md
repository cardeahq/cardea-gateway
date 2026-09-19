# NNNN — <Feature name> (technical)

**Date:** YYYY-MM-DD
**Functional spec:** specs/NNNN-<feature>-functional.md — or "none"

<!--
Copy this file to specs/NNNN-<feature>-technical.md and fill it in.
Write for whoever implements it: enough detail that the implementation is
transcription, not invention. Link the functional spec instead of
restating its behavior. Delete a section that genuinely doesn't apply
(a feature with no config surface needs no Configuration section).
Delete these comments.
-->

## Context

<The problem and why agentkeeper needs to solve it. Link the functional
spec rather than repeating it.>

## Goals

- <What this design has to achieve.>

## Non-goals

- <What this design deliberately does not handle.>

## Design

<The shape of the solution in prose, plus a request-flow sketch where it
helps. Say where it hooks into the existing flow:

  cli.serve → config.Config → zap.Logger → provider.Provider → gateway.Gateway

Does it inspect requests, responses, both, or neither?>

## Packages and types

### `internal/<package>` — new | modified

<What is added or changed. Real signatures for everything exported:>

```go
type Foo struct { ... }

func NewFoo(cfg config.FooConfig, log *zap.Logger) *Foo
```

<Name the files that are new vs. modified.>

## Configuration

```yaml
<section>:
  <field>: <default>
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `<section>.<field>` | <type> | `<default>` | `AGENTKEEPER_<SECTION>_<FIELD>` |

<Each field needs a default in both `defaults()` and `Load()`, and an
entry in configs/agentkeeper.example.yaml.>

## Failure modes

<What happens on error, timeout, and misconfiguration. State fail-open vs.
fail-closed explicitly and justify it — it must match the functional
spec's failure behavior.>

## Testing

<Which packages get tests, what the table cases cover, and any
httptest-based upstream fake needed. This is the floor for the test
author, not the ceiling.>

## Alternatives considered

- **<Alternative>** — rejected because <reason>.

<A spec with no rejected alternatives is usually under-thought.>

## Open questions

- <Question — who or what decides it.>

<Delete this section when empty rather than writing "none".>

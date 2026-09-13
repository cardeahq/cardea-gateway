---
name: new-tech-spec
description: This skill should be used when the user asks to "write a technical spec", "bootstrap a spec", "draft a design doc", "spec out this feature", or otherwise wants a new architecture/design document created under specs/ for an agentkeeper feature.
---

# Bootstrap a technical specification

A technical spec describes *how* a feature will be built — architecture,
types, config surface, request flow, trade-offs. It is not a functional
spec (which describes *what* the feature does for the operator) and not a
task list. Write it before the code, not after.

## File location and naming

Specs live flat in `specs/` with a shared, zero-padded numeric sequence
across both kinds:

```
specs/0001-circuit-breaker-functional.md
specs/0002-circuit-breaker-technical.md
specs/0003-token-compaction-technical.md
```

Pick the next number by listing what's there — never reuse or renumber an
existing one, even if its spec was abandoned:

```sh
ls specs/ 2>/dev/null | sort | tail -5
```

Name the file `NNNN-<kebab-case-feature>-technical.md`. If a functional
spec for the same feature already exists, reuse its exact feature slug so
the pair sorts together by name.

## Interview first, then write

Do not start drafting until the design questions are answered. Read the
code that the feature touches so the questions are specific to this
codebase, then ask the user — batched via `AskUserQuestion`, not one at a
time — about anything the code can't answer:

- **Scope** — one sentence on what the feature does, and the explicit
  non-goals. What is deliberately *not* in this version?
- **Functional spec** — does one exist in `specs/`? If not, is the
  behavior settled enough to design against, or should that come first?
- **Placement in the request path** — where in the `serve` data flow does
  it sit (`cli.serve` → `config.Config` → `zap.Logger` →
  `provider.Provider` → `gateway.Gateway`)? Does it inspect requests,
  responses, both, or neither?
- **Config surface** — what new `config.Config` fields does it need, and
  what are the defaults for an operator who never sets them?
- **Failure behavior** — when the feature itself errors, does the request
  fail closed (reject) or fail open (forward unmodified)? This is a
  security decision; make the user state it rather than assuming.
- **Observability** — what should be logged or counted, and at what
  level? Confirm nothing logged can contain prompt content, API keys, or
  `Authorization` headers.
- **Alternatives** — which designs were considered and rejected, and why.
  A spec with no rejected alternatives is usually under-thought; if the
  user has none, propose two and ask them to rule one out.

Only ask what actually changes the design. Don't interview about things
already fixed by CLAUDE.md conventions (no speculative abstractions,
business logic under `internal/`, dependencies built in `RunE`).

## Structure of the document

```md
# NNNN — <Feature name> (technical)

**Status:** Draft | Accepted | Implemented | Superseded by NNNN
**Date:** YYYY-MM-DD
**Functional spec:** specs/NNNN-<feature>-functional.md (or "none")

## Context
The problem, and why agentkeeper needs to solve it. Link the functional
spec rather than restating its behavior.

## Goals / Non-goals
Bulleted. Non-goals are the more useful half — they are what stops scope
creep during implementation.

## Design
The shape of the solution in prose plus, where it helps, a request-flow
sketch. State where it hooks into the existing data flow.

## Packages and types
Per package touched: what is added or changed. Give real Go signatures
for new exported types/functions — enough that the implementation is
transcription, not invention. Say which files are new vs. modified.

## Configuration
New `config.Config` fields with YAML shape, defaults, and the matching
`AGENTKEEPER_`-prefixed env var names. Note that each field needs a
default in both `defaults()` and `Load()`, plus an entry in
`configs/agentkeeper.example.yaml`.

## Failure modes
What happens on error, timeout, and misconfiguration. State fail-open vs.
fail-closed explicitly and justify it.

## Testing
What proves this works: which packages get tests, what the table cases
cover, and any `httptest`-based upstream fake needed.

## Alternatives considered
Each with a one-line reason it was rejected.

## Open questions
Anything unresolved, with who/what decides it. Delete the section when
empty rather than leaving it with "none".
```

Drop a section that genuinely doesn't apply (a feature with no config
surface needs no Configuration section) — but don't pad one to fill it.

## Before calling it done

- [ ] Filename number is unused and the feature slug matches any existing
      functional spec for the same feature.
- [ ] Status and date filled in; Status is `Draft` unless the user says
      otherwise.
- [ ] Every new exported type/function appears with a real signature.
- [ ] Failure behavior is stated explicitly, not implied.
- [ ] Config fields list defaults and env var names.
- [ ] No prompt content, keys, or headers in anything the spec says to log.
- [ ] Tell the user the path, and that per CLAUDE.md the spec must be
      updated in the same change if the implementation diverges from it.

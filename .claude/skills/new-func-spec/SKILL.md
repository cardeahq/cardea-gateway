---
name: new-func-spec
description: This skill should be used when the user asks to "write a functional spec", "bootstrap a functional spec", "describe the behavior of this feature", "write the requirements", or otherwise wants a new behavior/requirements document created under specs/ for a Gateway feature.
---

# Bootstrap a functional specification

A functional spec describes *what* a feature does and for whom — the
observable behavior, the operator's knobs, the acceptance criteria. It is
the companion to the technical spec (*how* it is built, see the
`new-tech-spec` skill) and it comes first: the technical spec designs
against it.

Two audiences read it, and neither wants Go:

- the **operator** who deploys the Gateway and configures it;
- the **application developer** whose client talks to the gateway and who
  sees only requests, responses, status codes, and latency.

So: no Go types, no package names, no function signatures, no file
layout. If a sentence can only be understood by someone who has read
`internal/`, it belongs in the technical spec instead.

## File location and naming

Specs live flat in `specs/`, zero-padded, one number per **feature** —
both halves of a feature share it:

```
specs/0001-circuit-breaker-functional.md
specs/0001-circuit-breaker-technical.md
specs/0002-token-compaction-functional.md
```

Name the file `NNNN-<kebab-case-feature>-functional.md`. Which `NNNN`
depends on whether the feature already has a technical spec:

- **It does** (the pair was written out of order) — reuse that spec's
  number *and* slug exactly, changing only the suffix, and add the
  back-link to it.
- **It doesn't** — take the next unused number. Never reuse or renumber
  one, even if its spec was abandoned:

  ```sh
  ls specs/ 2>/dev/null | sort | tail -5
  ```

## Interview first, then write

Do not start drafting until the behavior is settled. Skim the existing
behavior the feature changes — the routes in `internal/gateway`, the
config surface in `configs/cardea-gateway.example.yaml` — so the questions
are specific to the Gateway, then ask the user, batched via
`AskUserQuestion` rather than one at a time:

- **Problem and who has it** — what goes wrong today, for the operator or
  for the calling application. One or two sentences, concrete.
- **Observable behavior** — what changes from the *caller's* side: status
  codes, response bodies, headers, latency, or nothing at all? "The caller
  cannot tell" is a valid and important answer.
- **Default posture** — with an operator who configures nothing, is the
  feature on or off, and why is that the safe default?
- **Operator controls** — what can be tuned, described in plain words
  (thresholds, limits, allow/deny lists). Not YAML keys — the technical
  spec names those.
- **Behavior on failure** — when the feature itself cannot do its job, is
  the request rejected or forwarded unchanged? Make the user state it;
  it's a security and availability decision, not an implementation
  detail.
- **What the operator sees** — what shows up in logs or metrics when the
  feature acts, and how they'd tell a blocked request from an upstream
  error. Confirm that nothing surfaced contains prompt content, API keys,
  or `Authorization` headers.
- **Non-goals** — what is deliberately not in this version.
- **Acceptance criteria** — how we will know it works. If the user has
  none, propose three and get them confirmed.

Two constraints hold across the Gateway features; don't re-litigate them
in the interview, but do check the proposed behavior against them and
flag a conflict:

- The caller cannot choose or see the upstream provider — the Gateway
  decides from config alone. A feature must not add a caller-facing
  provider selector.
- Behavior is meant to be rule-driven for the operator, not
  caller-driven.

## Structure of the document

Start from `specs/TEMPLATE-functional.md` — read it and follow its
sections and header block. Fill every placeholder, strip its guidance
comments, and drop a section that genuinely doesn't apply rather than
padding one to fill it.

Two sections carry most of the weight and are the ones to slow down on:

- **Behavior** — numbered rules, each one something an implementer
  cannot read two ways. Cover the edge of every threshold, not just the
  normal path.
- **Acceptance criteria** — these become the test suite, so each must be
  checkable on its own. "Rate limiting works" is not a criterion;
  "**Given** a limit of 10 req/min, **when** the 11th request arrives
  within the window, **then** the caller receives 429 and the request is
  not forwarded" is.

## Before calling it done

- [ ] Filename number is unused and the feature slug matches any existing
      technical spec for the same feature.
- [ ] Date filled in.
- [ ] No Go types, package names, or file paths from `internal/`.
- [ ] Every acceptance criterion is Given/When/Then and independently
      testable.
- [ ] Default posture is stated for an operator who configures nothing.
- [ ] Failure behavior says reject or forward, explicitly.
- [ ] Nothing the spec says to log or return can carry prompt content,
      API keys, or `Authorization` headers.
- [ ] Tell the user the path, and offer the `new-tech-spec` skill as the
      next step. Per CLAUDE.md, if the implementation later diverges from
      the spec, the spec is updated in the same change.

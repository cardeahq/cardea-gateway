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

Specs live flat in `specs/`, zero-padded, one number per **feature** —
both halves of a feature share it:

```
specs/0001-circuit-breaker-functional.md
specs/0001-circuit-breaker-technical.md
specs/0002-token-compaction-technical.md
```

Name the file `NNNN-<kebab-case-feature>-technical.md`. Which `NNNN`
depends on whether the feature already has a functional spec:

- **It does** — the usual case, since the functional spec comes first.
  Reuse its number *and* slug exactly, changing only the suffix.
- **It doesn't** — take the next unused number. Never reuse or renumber
  one, even if its spec was abandoned:

  ```sh
  ls specs/ 2>/dev/null | sort | tail -5
  ```

## Interview first, then write

Do not start drafting until the design questions are answered. Read the
code that the feature touches so the questions are specific to this
codebase, then ask the user — batched via `AskUserQuestion`, not one at a
time — about anything the code can't answer:

- **Scope** — one sentence on what the feature does, and the explicit
  non-goals. What is deliberately *not* in this version?
- **Functional spec** — does one exist in `specs/`? If not, is the
  behavior settled enough to design against, or should that come first
  via the `new-func-spec` skill?
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

Start from `specs/TEMPLATE-technical.md` — read it and follow its
sections and header block. Fill every placeholder, strip its guidance
comments, and drop a section that genuinely doesn't apply (a feature with
no config surface needs no Configuration section) rather than padding one
to fill it.

Two sections are what make the spec worth having:

- **Packages and types** — every new exported type and function with a
  real Go signature, and which files are new vs. modified. The bar is
  that implementing it is transcription, not invention.
- **Alternatives considered** — each with the one-line reason it lost.
  If the section is empty the design probably hasn't been stress-tested.

## Before calling it done

- [ ] Filename number is unused and the feature slug matches any existing
      functional spec for the same feature.
- [ ] Date filled in.
- [ ] Every new exported type/function appears with a real signature.
- [ ] Failure behavior is stated explicitly, not implied.
- [ ] Config fields list defaults and env var names.
- [ ] No prompt content, keys, or headers in anything the spec says to log.
- [ ] Tell the user the path, and that per CLAUDE.md the spec must be
      updated in the same change if the implementation diverges from it.

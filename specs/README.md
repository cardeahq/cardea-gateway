# specs/

Specifications for agentkeeper. These are the source of intent for the
gateway's behavior — including features that aren't implemented yet. They
are not code and not a task list; when the implementation diverges from a
spec, the spec gets updated in the same change rather than left stale.

## Layout

Flat directory, one zero-padded number per **feature**:

```
specs/0001-circuit-breaker-functional.md
specs/0001-circuit-breaker-technical.md
specs/0002-token-compaction-functional.md
```

`NNNN-<kebab-case-feature>-{functional,technical}.md`. The number
identifies the feature, not the document: both halves of a feature share
it, along with the slug.

A **new feature** takes the next free number — never one that has been
used before, even by an abandoned spec. Find it with:

```sh
ls specs/ | sort | tail -5
```

Writing the **second half of an existing feature** is the opposite move:
reuse that feature's number and slug exactly, changing only the
`-functional` / `-technical` suffix. A feature may have only one half —
a technical spec with no functional counterpart is fine, and its number
stays its own.

Every spec starts with a `**Date:**` line and a link to its counterpart.

## The two kinds

| | Functional | Technical |
| --- | --- | --- |
| Answers | *what* the feature does, and for whom | *how* it is built |
| Audience | the operator deploying agentkeeper; the developer whose client calls it | whoever implements it |
| Contains | scenarios, behavior rules, operator controls in plain words, Given/When/Then acceptance criteria | design, packages and types with real Go signatures, `config.Config` fields, failure modes, alternatives considered |
| Never contains | Go types, package names, `internal/` paths | a restatement of the functional spec's behavior — link it instead |

The functional spec comes first; the technical spec designs against it.

Two invariants hold across features and shouldn't be re-opened in a spec:
the caller cannot choose or see the upstream provider (agentkeeper decides
from config alone), and behavior is rule-driven for the operator, not
caller-driven.

## Writing one

By hand, copy the matching template and fill it in:

```sh
cp specs/TEMPLATE-functional.md specs/0004-rate-limiting-functional.md
```

`TEMPLATE-functional.md` and `TEMPLATE-technical.md` are the canonical
structure. Each section carries a short `<placeholder>` saying what goes
in it; replace them, strip the `<!-- -->` guidance comments, and delete
any section that genuinely doesn't apply. The templates are excluded from
the numeric sequence, so they never collide with a real spec.

Or let a skill do it — both interview you first and then write to the
template:

- `new-func-spec` — "write a functional spec", "write the requirements"
- `new-tech-spec` — "write a technical spec", "spec out this feature"
- `spec-review` — "check the technical spec against the functional spec"

Each ends with a checklist covering the things that are easy to leave out:
default posture with zero config, explicit fail-open vs. fail-closed, and
the privacy boundary (nothing logged or returned may carry prompt content,
API keys, or `Authorization` headers).

## From spec to implementation

When both halves exist, run `spec-review` before anything else — it
cross-checks the technical design against the behavior it is supposed to
implement, which is far cheaper to fix here than after the tests encode
it.

Once a spec is agreed, three subagents in `.claude/agents/` take it to
done, with a human gate between each stage:

1. **`spec-test-writer`** reads both specs and writes the tests — no
   implementation. The tests it produces are the contract.
   *You review and approve them.*
2. **`spec-implementer`** writes the code that makes those tests pass. It
   is forbidden from modifying any test file.
   *You review and approve the implementation.*
3. **`quality-gate`** independently verifies tests and code against the
   spec and returns a single PASS or FAIL. It changes nothing; a PASS means
   the feature is implemented.

Invoke each with the spec path, e.g. *"write the tests for
specs/0001-circuit-breaker-technical.md"*.

Specs are worth reviewing on their own, before any of that starts — a spec
PR is cheaper to argue with than an implementation PR.

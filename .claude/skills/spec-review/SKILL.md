---
name: spec-review
description: This skill should be used when the user asks to "check the technical spec against the functional spec", "do these specs agree", "review the spec pair", "spec review", or wants a design in specs/ verified against the behavior it is supposed to implement — before any tests or code are written.
model: sonnet
---

# Review a technical spec against its functional spec

The functional spec says _what_ a feature does; the technical spec says
_how_. This check answers one question: **would building exactly what the
technical spec describes produce exactly the behavior the functional spec
promises?**

Run it before `spec-test-writer`. Everything downstream treats the specs
as the contract — the test writer encodes them as assertions, and the
implementer is forbidden from correcting a test. A disagreement between
the two halves that survives to stage 1 becomes a wrong requirement
nobody is allowed to fix without going back to the start. This is the
cheapest place to catch it.

Read both halves in full first. They share a number and slug, so the
technical spec's counterpart is the same filename with `-technical`
swapped for `-functional`. If only one half exists, say so and stop —
there is nothing to cross-check.

## What to check

**1. Coverage — every promise has a mechanism.**
Walk the functional spec's numbered _Behavior_ rules one at a time and
find the part of the technical design that delivers each. Do the same for
every _Acceptance criterion_ against the technical spec's _Testing_
section. A rule with no mechanism, or a criterion with no test, is a gap:
it will be built by whoever guesses first.

**2. Contradiction — same subject, different answer.**
Read both halves for each of these and compare literally:

- **Fail-closed vs fail-open.** If the functional spec says a failure
  denies the request, no path in the technical design may forward it.
  This is the single most important check here — it is a security
  property, and it inverts quietly.
- **Status codes and error shapes.** A code named in an acceptance
  criterion must be the code in the technical spec's failure-mode table.
- **Defaults.** The value in the functional _Operator controls_ table
  must equal the value in the technical _Configuration_ table, including
  its on/off posture. These are written at different times and drift.
- **Ordering and short-circuit semantics.** "The first denial stops
  everything" and "every stage observes the outcome" are easy to design
  into conflict.

**3. Scope — nothing designed that was not asked for.**
Anything a caller or operator can _observe_ — a status code, a header, an
error field, a log line, a config key — must trace back to a functional
rule or control. A technical spec that invents caller-visible behavior is
adding requirements nobody agreed to, in the document least likely to be
read by the people affected. Check the functional _Non-goals_ list too: a
design that quietly implements one is scope creep with a paper trail.

**4. Internal consistency of the technical spec.**
The two halves can agree while the technical spec disagrees with itself.
Look for:

- A struct field, type, or config key that nothing sets, nothing reads,
  and no test covers — usually a leftover from an earlier design, and a
  trap for the test writer, who must either invent behavior for it or
  stop and ask.
- A design narrative that contradicts the failure-mode table. When the
  prose says "cap" and the table says "reject", they describe different
  systems.
- An exported signature in _Packages and types_ that no described flow
  calls.

**5. Audience leakage.**
The functional spec must contain no Go types, package names, or
`internal/` paths — if a rule can only be understood after reading the
code, operators cannot review the thing written for them. The technical
spec must not restate functional behavior; it links to it. A restatement
is a second copy that will drift from the first.

**6. The Gateway's standing invariants.**
These hold across every feature; a spec pair that breaks one is wrong
regardless of internal consistency:

- The caller cannot choose or see the upstream provider. No design may
  add a caller-facing provider selector, or let a caller-influenced value
  reach the upstream URL.
- Behavior is rule-driven for the operator, not caller-driven.
- Nothing specified to be logged or returned may carry prompt content,
  API keys, or `Authorization` header values — including via a struct
  logged whole.
- No unbounded buffering of a request or response body. A size limit must
  exist, and the spec must say what happens when it is hit.
- New config needs the full triple specified: struct field, a default in
  both `defaults()` and `Load()`, and an entry in
  `configs/cardea-gateway.example.yaml`.

**7. Unresolved open questions.**
An _Open questions_ entry that defers to a future spec is fine. One that
leaves a value, status code, or default undecided in _this_ feature
blocks stage 1 — the test writer cannot write that assertion without
guessing. Flag those specifically.

## Which half is wrong

Don't assume. The functional spec governs _what_, so it usually wins —
but a technical spec sometimes proves a promised behavior is impossible
or unaffordable, and that is a finding about the functional spec, not a
detail to design around. Say which half you believe is wrong and why; the
human decides. Never resolve a contradiction by picking one side
silently, and never edit the specs as part of this review.

## Reporting

Use the `ReportFindings` tool, most-severe first. For each finding cite
both locations — the functional rule and the technical passage — and
state the concrete consequence: what gets built, and how the resulting
behavior differs from what the functional spec promised. "These are
inconsistent" is not a finding; "rule 9 requires a denial, but the
failure-mode table forwards the request, so a crashing stage becomes a
bypass" is.

An empty list is a real result. Say plainly that the pair is consistent
and the feature is ready for `spec-test-writer`.

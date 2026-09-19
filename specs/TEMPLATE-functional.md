# NNNN — <Feature name> (functional)

**Date:** YYYY-MM-DD
**Technical spec:** specs/NNNN-<feature>-technical.md — or "not written yet"

<!--
Copy this file to specs/NNNN-<feature>-functional.md and fill it in.
Write for the operator who deploys agentkeeper and the developer whose
client calls it: no Go types, no package names, no internal/ paths.
Delete a section that genuinely doesn't apply. Delete these comments.
-->

## Context

<What goes wrong today, and who it hurts. Two or three sentences. No
solution here.>

## Goals

- <What the feature achieves, in user-visible terms.>

## Non-goals

- <What is deliberately not in this version. This is what stops scope
  creep later — be specific.>

## Users and scenarios

<The one or two concrete situations this exists for, narrated end to end.
Who is doing what, and what they get.>

## Behavior

1. <Numbered rules an implementer can't misread. Cover the normal path,
   the edge of each threshold, and what the caller observes in each case —
   "the caller cannot tell" is a valid answer.>

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| <name in plain words> | <effect> | <value + unit> | <what happens at min / max> |

<With an operator who configures nothing, is the feature on or off? Say
which, and why that is the safe default.>

## Failure behavior

<When the feature cannot do its job, is the request rejected or forwarded
unchanged? State it explicitly and justify it. Say what the caller gets.>

## What the operator sees

<Logs, counters, and error messages, described by meaning rather than
format. How do they tell a blocked request from an upstream error?

Nothing surfaced may contain prompt content, API keys, or Authorization
headers.>

## Acceptance criteria

1. **Given** <starting state> **when** <action> **then** <observable
   result>.

<Numbered and independently checkable — the tests are written from these.
If one can't become a test, it's too vague.>

## Open questions

- <Question — who decides it.>

<Delete this section when empty rather than writing "none".>

# 0008 — Authorization and identification (functional)

**Date:** 2026-10-20
**Technical spec:** specs/0008-authorization-and-identification-technical.md

**Supersedes:** specs/0002-agent-identification-functional.md — see that
file's superseded notice for what changes and why.

## Context

Today's identification stage (specs/0002) conflates two different
questions behind one credential check: "is this caller allowed to talk to
the Gateway at all" and "which agent is this." Answering both with one
per-agent API key or JWT means an operator who just wants a simple on/off
gate still has to provision and manage a credential per agent, and an
operator who wants to tell Claude Code traffic apart from a custom agent's
traffic — for policy or tracing purposes — has to invent a credential
scheme to do it, even when they don't need access control at all. The two
needs have different natural shapes (a shared secret vs. a classifier) and
different safe defaults, and today's single stage can't give either one
its own.

## Goals

- Split today's single identification stage into two independent stages:
  **authorization** (may this call reach the Gateway at all) and
  **identification** (what kind of agent is calling).
- Make authorization a simple on/off gate using one shared secret, with
  support for a second secret so an operator can rotate without an outage.
- Make identification a non-blocking classifier: it labels the caller's
  agent type for later stages to use, but never itself rejects a call.
- Let an operator recognize a caller's agent type from request headers
  alone, with ready-made recognition for common agents (Claude Code, Open
  Code) and a way to add their own.
- Give every call an agent type, including one that matches nothing:
  `"unknown"`. No call is ever left without one once identification is
  enabled.

## Non-goals

- JWT-based or any other signed-credential authorization. Authorization in
  this version is a shared secret only; see specs/0002's superseded notice
  for why per-agent JWTs are being removed rather than kept.
- Per-agent credentials of any kind. Authorization is global: every caller
  that knows a valid shared secret is equally authorized: this version
  draws no line between callers once they're past the gate.
- Deciding what an identified agent type is *allowed* to do — the policies
  stage's own spec.
- Any body-content or payload-shape based identification. This version
  recognizes an agent type from request headers only.
- Session-id retrieval living in the agent-type config file. Session
  sources stay exactly where specs/0003 put them (`tracing.sessions`) for
  this version; moving them alongside an agent type's own config is an
  explicitly intended next revision, not part of this one.
- A management API or CLI for authoring agent-type files or rotating
  shared secrets. The operator edits files and environment variables
  directly, as today.
- Resolving which agent type wins when more than one agent-type file's
  rules could match the same call beyond a simple, visible precedence
  order (see *Behavior* § Identification, rule 4) — a static "these two
  files can never both match" check is out of scope this version.

## Users and scenarios

**The operator who only wants a gate.** They run the Gateway behind a
network boundary they don't fully trust (a shared staging environment, a
proxy reachable from more than one team) and want a simple "did this
caller present our shared secret" check, with no interest in telling
agents apart. They turn authorization on, set one shared secret (plus a
second while rotating), and leave identification wherever its default
lands — they never touch an agent-type file.

**The operator who wants agents told apart, not gated.** They run the
Gateway on a network they already trust and have no interest in an
access-control layer, but they want their policy rules and trace records
to say "this was Claude Code" rather than every call looking the same.
They leave authorization off, drop Cardea's built-in `claude_code.yaml`
and `opencode.yaml` into their identification directory (or write their
own alongside them), and from that point every call's audit trail and
policy input carries an `agent_id` of `"claude-code"`, `"opencode"`, or
`"unknown"` for anything that matches neither.

**The operator who wants both.** They turn authorization on for the gate
and configure agent-type files for classification. The two are
independent: a call can fail authorization before identification ever
runs, but nothing about identification's outcome ever feeds back into
whether the call is allowed.

## Behavior

### Authorization

1. Authorization is a staging-pipeline stage (specs/0001) and, when
   enabled, runs first in the fixed order — ahead of identification and
   everything after it.
2. The credential travels in the standard `Authorization` header as a
   bearer value (`Authorization: Bearer <secret>`), the same convention
   specs/0002 used.
3. A call is authorized if its bearer value exactly matches any one of
   the operator's configured shared secrets. There is no per-agent
   distinction: every caller holding a valid secret is authorized
   identically.
4. The following are all denied, and denied identically from the caller's
   point of view: no `Authorization` header; a header that is not a
   recognized bearer shape; and a bearer value that matches none of the
   configured secrets. A caller cannot distinguish any of these from the
   response alone.
5. With zero shared secrets configured, authorization enabled denies
   every call, regardless of what is presented — there is nothing a call
   could match.
6. Disabling authorization (the default) removes this gate entirely:
   every call proceeds regardless of what `Authorization` header it
   carries, exactly as the Gateway behaves today with identification
   disabled.
7. Authorization never attaches an agent identity or type to the call. Its
   only output is allow or deny.
8. The raw shared secret is never included in a denial's caller-visible
   message, its audit-only reason, or any log line.

### Identification

1. Identification is a staging-pipeline stage that, when enabled, runs
   immediately after authorization (or first, if authorization is
   disabled) and before tracing, policies, and filtering.
2. The operator defines zero or more **agent types**. Each agent type
   names itself and lists one or more header-based rules for recognizing
   it: an exact header value, a header-value prefix, or a pattern. A call
   matches an agent type if any one of that type's rules matches.
3. Cardea ships ready-made agent-type recognition for Claude Code and
   Open Code that the operator can adopt as-is or use as a starting point
   for their own.
4. If more than one configured agent type's rules would match the same
   call, the operator's own ordering of their agent-type definitions
   decides which one wins — the first one, in that order, that matches.
   This is a visible, operator-controlled tie-break, not an error.
5. Whenever identification is enabled, every call is assigned exactly
   one agent type: the first matching one, or `"unknown"` if none
   matches or no agent types are configured at all.
6. Disabling identification means no call carries an agent type from this
   stage at all — later stages see the same absence of an identity they
   see today when specs/0002's stage is off. This is different from
   identification being enabled with nothing configured or nothing
   matching, which always produces `"unknown"`, never an absence.
7. Identification never denies a call, never fails it, and never alters
   what is forwarded to the provider or returned to the caller. The only
   thing identification does is attach an agent type for later stages
   (policies, tracing, the audit trail) to read.
8. The assigned agent type is available, unchanged, to every later stage
   — including ones that only observe the completed call — and to the
   audit record, exactly as `agent_id` is available today.

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Enable authorization | Turns the shared-secret gate on or off | Off | Off: every call proceeds regardless of credential, identical to today. On: every call must present a valid shared secret — zero secrets configured denies all traffic. |
| Shared secrets | One or two values a caller's bearer credential may match | None | None + enabled: all calls denied. Two configured: either is accepted, enabling rotation without an outage. |
| Enable identification | Turns agent-type recognition on or off | On | Off: no call carries an agent type, matching today's behavior with identification off. On with nothing configured: every call is `"unknown"` — harmless. |
| Agent-type definitions | The set of recognized agent types and the header rules that identify each | Cardea's built-ins (Claude Code, Open Code) plus whatever the operator adds | None beyond the built-ins: only Claude Code and Open Code calls are recognized, everything else is `"unknown"`. Many definitions: one more header comparison per type per call, no other cost. |

Authorization defaults to **off**, for the same reason specs/0002's stage
did: for every other stage, "enabled with nothing configured" means
"allow everything," but for a credential gate it means "deny everything,"
which an operator must turn on deliberately rather than discover as an
outage after an upgrade.

Identification defaults to **on**, unlike authorization and unlike
specs/0002's single stage. Because identification can no longer deny a
call — the worst case of "enabled with nothing configured" is that every
call is labeled `"unknown"` — there is no outage risk in leaving it on,
so it follows the staging pipeline's general default instead of carving
out an exception.

## Failure behavior

**Every authorization failure denies the call**, per specs/0001's general
contract: the caller receives a generic error naming the authorization
stage and a per-call identifier; the provider never sees the request.
This covers every case in *Behavior*, rules 4 and 5.

**Identification never denies a call.** It cannot fail in the sense of
rejecting a request — matching a header against a rule either succeeds or
it doesn't, and "doesn't" is the ordinary, documented `"unknown"` outcome,
not an error.

An error while evaluating either stage — as opposed to a deliberate
denial or a deliberate `"unknown"` — follows specs/0001's general
stage-failure contract: the call is denied with a "governance unavailable"
error, distinguishable from a normal denial.

Misconfiguration fails the Gateway at startup, matching specs/0001's rule
for a misconfigured stage: for authorization, a shared-secret source that
cannot be read; for identification, two agent-type definitions sharing a
name, a definition that reuses the reserved name `"unknown"`, or a
definition with an unreadable or invalid match rule.

## What the operator sees

- One log line per denied call, per specs/0001's general contract, whose
  audit-only reason additionally names *why* authorization failed: no
  credential presented, credential in an unrecognized shape, or
  credential matching no configured secret.
- No log line for identification succeeding or landing on `"unknown"` —
  it is exactly as quiet as a call that matches an agent type. The
  assigned agent type is visible in the audit record, not in a separate
  log line.
- Nothing surfaced — log line, audit-only reason, or caller-visible
  message — ever contains a shared secret.

## Acceptance criteria

1. **Given** authorization disabled (the default), **when** a request is
   proxied with no `Authorization` header, **then** it proceeds exactly as
   it does today.
2. **Given** authorization enabled with one shared secret configured,
   **when** a request carries that secret as a bearer credential, **then**
   the call proceeds and the provider receives it unchanged.
3. **Given** the same setup, **when** a request has no `Authorization`
   header, or presents a bearer value that matches no configured secret,
   **then** the caller receives a generic denial naming the authorization
   stage and a per-call identifier, and the provider receives no request.
4. **Given** authorization enabled with two shared secrets configured
   (simulating a rotation), **when** a request presents either one,
   **then** the call proceeds.
5. **Given** authorization enabled with zero shared secrets configured,
   **when** any request arrives, with or without a credential, **then**
   it is denied identically to criterion 3.
6. **Given** identification enabled (the default) with no agent-type
   definitions configured, **when** any request is proxied, **then** its
   audit record carries agent type `"unknown"`.
7. **Given** identification enabled with one agent-type definition whose
   rule matches a specific header value, **when** a request carries that
   header value, **then** its audit record carries that agent type, and
   when a request does not, **then** its audit record carries
   `"unknown"`.
8. **Given** Cardea's built-in Claude Code and Open Code agent-type
   definitions in effect, **when** a request carries the headers Claude
   Code's SDK sends, **then** its audit record carries agent type
   `"claude-code"`, and likewise for Open Code's headers and
   `"opencode"`.
9. **Given** two agent-type definitions whose rules would both match the
   same request, defined in a specific order, **when** such a request is
   proxied, **then** its audit record carries the first-defined matching
   type, deterministically and repeatably.
10. **Given** identification disabled, **when** any request is proxied,
    **then** its audit record carries no agent type at all — not
    `"unknown"`, absent, matching today's behavior with the stage off.
11. **Given** both authorization and identification enabled, **when** a
    request fails authorization, **then** it is denied before
    identification runs and its audit record carries no agent type.
12. **Given** authorization enabled and a valid shared secret presented,
    **when** the resulting log lines and audit-only reasons are
    inspected, **then** the shared secret does not appear in any of them.
13. **Given** an agent-type definition named `"unknown"`, or two
    definitions sharing the same name, **when** the Gateway starts,
    **then** it exits with an error naming the conflict rather than
    starting.

## Open questions

- Should an agent type ever be recognized from something other than
  request headers — the request body's shape, or TLS/mTLS material — in a
  later version? Headers are sufficient for every agent SDK seen so far;
  revisit if one shows up that can't be told apart this way.
- specs/0002 left open whether the agent developer should ever see their
  own identity echoed back. The same question now applies to agent type:
  should a caller ever see which type the Gateway classified them as (for
  self-diagnosis), or does that stay purely an operator-side/audit
  concern? Deferred to the audit stage's spec.
- How exactly agent-type files will host session-id retrieval in the
  "next revision" this spec defers (see *Non-goals*) — whether that
  absorbs `tracing.sessions` entirely or sits alongside it — is left for
  that revision's own spec.

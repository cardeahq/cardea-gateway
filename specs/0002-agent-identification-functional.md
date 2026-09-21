# 0002 — Agent identification (functional)

**Date:** 2026-09-19
**Technical spec:** specs/0002-agent-identification-technical.md

## Context

The staging pipeline (specs/0001) gives every governance stage a fixed slot
and a uniform way to allow or deny a call, but today no stage knows *who*
is calling. Every proxied request is anonymous: the gateway forwards it to
the single configured provider without asking which agent it came from.
The RFC's remaining stages — policies, monitoring, filtering,
rate-limiting, audit — all need a per-caller identity ("per-agent quota",
"policy for this agent", "who" in the audit trail) that nothing today
produces. Without an identification stage, each of them would have to
invent its own notion of "who is calling," which is exactly the kind of
quietly-inconsistent behavior the staging pipeline exists to prevent.

## Goals

- Authenticate every proxied call to a specific, operator-registered
  `agent_id` before any later stage or the provider sees it.
- Accept either an opaque per-agent API key or a signed JWT as the
  credential, at the operator's choice per deployment.
- Make the authenticated `agent_id` available to every later stage for the
  rest of that call.
- Give an identical, generic denial for every way a call can fail to be
  identified, so a caller cannot use the response to tell a missing
  credential apart from an unregistered one, or probe for valid agent IDs.
- Fail closed: once enabled, a deployment with no agents registered denies
  every call, because no caller can possibly be identified.

## Non-goals

- Deciding what an identified agent is *allowed* to do (models, tools,
  time-of-day) — that belongs to the policies stage's own spec. This stage
  only answers "who," never "may they."
- Per-agent rate limits and quotas — the rate-limiting stage's spec.
- Audit log format and storage — the audit stage's spec.
- A management API or CLI for creating, rotating, or revoking agent
  credentials. In this version the operator edits the agent registry the
  same way they edit provider config today, and changes take effect on
  reload/restart.
- Fetching JWT signing keys from an external identity provider or JWKS
  endpoint. In this version, the verification material for a JWT-based
  agent is configured locally alongside its registry entry.
- mTLS or client-certificate-based identification.
- Holding more than one live credential per agent (e.g. old and new key
  overlapping during rotation).

## Users and scenarios

**The operator** runs a fleet of sandboxed agents against agentkeeper.
They register each one in agentkeeper's config with an `agent_id` and
either an API key or JWT verification material, then enable the
identification stage. From that point, every call must carry a credential
matching one of those entries. A stray or misconfigured agent whose
credential is missing, malformed, or unrecognized is denied the same way
every time, so the operator's logs cleanly separate real, mis-configured,
and hostile traffic without needing to reason about which specific
failure occurred — that detail is in the log line, not exposed to whoever
sent the bad request.

**The agent developer** points their SDK at agentkeeper instead of the
provider, exactly as the RFC's integration example shows: the credential
agentkeeper issued them goes wherever their SDK would normally put a
provider API key (`Authorization: Bearer <credential>`), not a real
upstream key. A correctly credentialed call behaves exactly like calling
the provider directly. A bad credential gets a generic, immediate error
rather than silently reaching the provider or leaking whether the problem
was their token or agentkeeper's configuration.

## Behavior

1. Identification is a staging-pipeline stage (specs/0001) and runs first
   in the fixed order, before policies, monitoring, filtering,
   rate-limiting, and audit.
2. The credential travels in the standard `Authorization` header as a
   bearer value (`Authorization: Bearer <credential>`). This header is
   already stripped before any request reaches the provider — providers
   inject their own upstream credential — so repurposing it for agent
   identification changes nothing about what the provider receives.
3. Two credential shapes are recognized: an opaque per-agent API key, and
   a signed JWT. Both may be in use at once across the registered agents;
   there is no separate switch for which shapes are "turned on" — a
   deployment recognizes whichever shapes its registered agents actually
   use.
4. An API key is valid if it matches, exactly, the key registered for
   some agent. A JWT is valid if it identifies which agent's verification
   material to check against (for example, via its subject claim), its
   signature verifies against that agent's registered material, and it is
   currently within its validity window (not expired, not used before its
   "not before" time).
5. Successful validation yields exactly one `agent_id` — the one attached
   to the matched registry entry (API key) or identified by the verified
   JWT.
6. The following are all denied, and denied identically from the caller's
   point of view: no `Authorization` header; a header that is not a
   recognized credential shape; an API key that matches no registered
   agent; a JWT whose signature does not verify; a JWT that identifies an
   agent with no registered JWT material; and a JWT outside its validity
   window. A caller cannot distinguish any of these from the response
   alone.
7. On success, the `agent_id` is attached to the call and is available,
   unchanged, to every later stage — including ones that only observe the
   completed call — and to the audit record. The caller's request and
   response are otherwise unaffected; identifying a call does not alter
   what is forwarded or returned.
8. With zero agents registered, every call is denied, regardless of
   whether or what credential is presented — there is nothing a call could
   match.
9. Disabling the identification stage (per specs/0001's per-stage
   enable/disable) removes authentication entirely for that deployment:
   every call proceeds without an `agent_id`, exactly as agentkeeper
   behaves today. What a stage that depends on `agent_id` should do when
   identification is disabled is for that stage's own spec to decide (see
   *Open questions*).
10. The raw credential value — API key or JWT — is never included in a
    denial's caller-visible message, its audit-only reason, or any log
    line. Only a category (see *What the operator sees*) and, when known,
    the `agent_id` it claimed to be, may be recorded.

## Operator controls

| Control               | What it does                                                                                                              | Default | At the extremes                                                                                                                                                                |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Enable identification | Turns the stage on or off, per specs/0001                                                                                 | Off     | Off: every call is anonymous, identical to today. On: every call must present a credential matching a registered agent — with zero agents registered, this denies all traffic. |
| Agent registry        | The list of agents agentkeeper will recognize, each with an `agent_id` and either an API key or JWT verification material | Empty   | Empty + enabled: all calls denied. A large registry costs one more credential comparison per request but changes no other behavior.                                            |

Identification defaults to **off**, unlike the staging pipeline's general
default of "every stage on." The difference is deliberate: for every other
stage, being enabled with nothing configured to enforce means "allow
everything" (specs/0001, rule 4) — but for identification, "enabled with
nothing configured" means "no caller can ever be identified," which is
indistinguishable from denying all traffic outright. An operator who
upgrades agentkeeper must not discover, by way of an outage, that a new
build silently started rejecting every call. Turning identification on is
therefore a deliberate, visible act — the same one that also requires
registering at least one agent — rather than something that happens to a
deployment by default.

## Failure behavior

**Every identification failure denies the call**, per specs/0001's general
contract: the caller receives a generic error naming the identification
stage and a per-call identifier; the provider never sees the request. This
covers every case in *Behavior*, rule 6, and the zero-agents case in rule
8 — there is no scenario in which identification cannot determine who is
calling and forwards the call anyway.

An error while validating a credential — as opposed to the credential
simply being invalid — follows specs/0001's general stage-failure
contract: the call is denied with a "governance unavailable" error, which
is distinguishable from a deliberate identification denial. This spec
does not add a new failure mode beyond what specs/0001 already defines.

Misconfiguration of the agent registry itself — for example, two entries
sharing an `agent_id`, or an entry whose JWT verification material cannot
be parsed — fails agentkeeper at startup, matching specs/0001's rule for a
misconfigured stage.

## What the operator sees

- One log line per denied call, per specs/0001's general contract, whose
  audit-only reason additionally names *why* identification failed: no
  credential presented, credential in an unrecognized shape, credential
  did not match any registered agent, or credential outside its validity
  window. This lets the operator debug a misconfigured agent without the
  caller ever seeing which case occurred.
- On success, no new log line is added — identified calls are as quiet as
  allowed calls are today (specs/0001).
- Nothing surfaced — log line, audit-only reason, or caller-visible
  message — ever contains the raw credential value, only its category and,
  where known, the `agent_id` it claimed.

## Acceptance criteria

1. **Given** identification disabled (the default), **when** a request is
   proxied, **then** it behaves exactly as it does without this stage —
   no `Authorization` header is required, and the result matches
   specs/0001's criterion 1.
2. **Given** identification enabled with one agent registered under an API
   key, **when** a request carries that key as a bearer credential,
   **then** the call proceeds, the provider receives it unchanged, and
   that agent's `agent_id` is attached to the call's outcome record.
3. **Given** identification enabled with at least one agent registered,
   **when** a request has no `Authorization` header, **then** the caller
   receives a generic denial naming the identification stage and a
   per-call identifier, and the provider receives no request.
4. **Given** the same setup as criterion 3, **when** a request presents an
   API key that matches no registered agent, **then** the response is
   identical in shape to criterion 3.
5. **Given** the same setup, **when** a request presents a syntactically
   valid JWT whose signature does not verify, **then** the response is
   identical in shape to criterion 3.
6. **Given** the same setup, **when** a request presents a JWT with a
   valid signature but an expired validity window, **then** the response
   is identical in shape to criterion 3.
7. **Given** identification enabled with zero agents registered, **when**
   any request arrives, with or without a credential, **then** it is
   denied identically to criterion 3.
8. **Given** two different agents registered, **when** each makes a call
   with its own valid credential, **then** each call's outcome record
   carries that agent's own `agent_id`, never the other's.
9. **Given** calls denied for each of the reasons in criteria 3–6,
   **when** the operator inspects the logs, **then** the audit-only reason
   distinguishes the four cases from one another, even though the
   response each caller received was identical.
10. **Given** an agent registry with two entries sharing the same
    `agent_id`, **when** agentkeeper starts, **then** it exits with an
    error naming the conflict rather than starting.
11. **Given** identification enabled and a valid credential, **when** the
    resulting log lines and audit-only reasons are inspected, **then** the
    raw credential value does not appear in any of them.

## Open questions

- What should a stage that needs `agent_id` (policies, rate-limiting,
  audit) do when identification is disabled and no `agent_id` is
  available? Deferred to each of those specs, per specs/0001's pattern of
  letting the stage that depends on something decide how it degrades
  without it.
- Credential rotation and revocation without a restart — issuing an agent
  a second, overlapping credential, or invalidating one immediately — is
  out of scope for this version (see *Non-goals*) but will need an answer
  once operators are managing registries larger than a handful of agents.
- Should the agent developer ever see their own `agent_id` echoed back
  (e.g. in a response header), to make it easier to tell their operator
  "call X from agent Y failed"? Today the caller sees only the per-call
  identifier from specs/0001, never their own identity. Worth revisiting
  once the audit stage's spec settles what correlation it needs.

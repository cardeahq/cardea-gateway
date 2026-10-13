# 0006 — Filtering (functional)

**Date:** 2026-10-13
**Technical spec:** specs/0006-filtering-technical.md — not written yet

## Context

Agents send whatever their prompt or tool results contain to the
provider, including PII that ends up there incidentally: a customer's
email address copied into a support ticket, an API key pasted into a bug
report, a social security number quoted from a form. Nothing in the
Gateway today stops any of it — every byte the agent sends reaches the
upstream provider verbatim, so a third-party provider ends up holding
PII the operator never meant to send it, with no way for the operator to
know how much or how often.

## Goals

- Detect known categories of PII in a request's conversation content and
  in the tool calls and tool results it already carries, before the
  request leaves the Gateway for the provider.
- Replace each detected occurrence with an opaque placeholder naming its
  category, so the provider never receives the real value, and so that
  one occurrence stays individually identifiable.
- Give every operator this protection for a fixed set of built-in
  categories with zero configuration — no file to write, unlike policies
  (specs/0004).
- Keep every replaced value recoverable by its placeholder, through a
  cache abstraction an operator can run in-memory (the default), swap for
  Redis, or replace with an implementation of their own — so a later
  version of the Gateway can read it back.
- Never let two identical PII values collapse onto the same placeholder
  within one request, so anyone who sees the redacted text cannot learn
  that two fields hold the same value, even without learning what that
  value is.
- Behave exactly as today for a request that contains none of the known
  PII categories.

## Non-goals

- **Restoring PII into the response.** This version writes detected
  values to the cache; nothing reads them back. Responses are relayed
  exactly as the provider sent them (specs/0001 rule 13) — unchanged even
  if the provider echoes a placeholder back verbatim. What restoration
  should do, and what happens when its cache entry is gone, is deferred
  to a later spec; see *Open questions* for the guidance gathered while
  writing this one.
- **A Redis-backed cache.** The pluggable interface is defined (see
  *Operator controls*) so one can be added without changing this spec,
  but only the in-memory implementation ships with this version.
- **Categories beyond email, SSN (US, UK, France) and API key** — phone
  numbers, credit card numbers, physical addresses, personal names, and
  so on.
- **Per-jurisdiction toggles within the SSN category.** An operator
  cannot enable US detection while disabling French detection; the
  category is on or off as a whole.
- **Operator-defined detection patterns.** The three categories and their
  patterns are fixed in this version; only the cache backend is
  pluggable.
- **Scanning request headers or the tools an agent offers** (their names,
  descriptions and declared schemas). Only conversation content and the
  tool calls/results already in the request are scanned — the same scope
  specs/0004 rule 11 reads from, minus the parts of it that aren't text
  an agent or a tool wrote.
- **A time limit on detection.** Unlike policy evaluation (specs/0004
  rule 18), detection is in-process pattern matching with no network
  call, so it has no comparable failure mode to bound.
- **Denying a call because of what PII it contains.** That is a policy
  decision (specs/0004); filtering only rewrites, it never refuses a call
  on its own. Its only way to stop a call is the failure path in
  *Failure behavior*.

## Users and scenarios

**The operator running a support-ticket agent.** The agent's prompts
regularly include a customer's email address, and occasionally a
developer pastes an API key from a bug report straight into the
conversation. The operator upgrades to this version and changes no
config: filtering is already on, with all three categories active. From
that point, the provider never receives the real email address or key —
only placeholders like `[email:f47ac10b-...]` — while the agent and its
caller see no difference in status codes or latency. The operator's logs
show one line per request with something redacted: which categories, how
many occurrences, never the values themselves. They also see a one-time
startup warning that the cache holding those values is in-memory and
will not survive a restart — today that's fine, since nothing reads it
back yet.

**The agent developer.** A permitted call behaves exactly as without
filtering, except that the provider is reasoning over placeholders where
PII used to be. If their prompt asked the model to validate or act on the
actual value — "does this look like a valid SSN?" — the model now sees
only `[ssn:UUID]` and cannot do that; this is the deliberate cost of
redacting before the provider ever sees the value. If the model happens
to echo a placeholder string back in its answer, the developer sees that
literal placeholder text in the response, by design — nothing in this
version disguises it.

## Behavior

1. Filtering occupies the pipeline slot the RFC (specs/0001 Context)
   places after monitoring and before rate-limiting. Monitoring and
   rate-limiting don't exist yet, so today filtering runs immediately
   after policies (specs/0004) in the enabled sequence. Its position is a
   property of the build, not configuration (specs/0001 rule 3).
2. Filtering only ever rewrites the **request**. It never denies a call
   by itself — that's a policy decision (specs/0004) — and it never
   rewrites a response (see *Non-goals*). Every call that reaches
   filtering still reaches the provider; what changes is the content it
   carries.
3. Within a request, filtering scans: the system prompt, every message's
   content, and the arguments and results of tool calls already in the
   conversation — the same material specs/0004 rule 11 exposes as
   `system`, `messages`, `tool_calls` and `tool_results`. It does not
   scan request headers, the model name, generation parameters, or the
   `tools` list of names/descriptions/schemas the agent offers.
4. Every occurrence of a known, enabled category's pattern within that
   scope is replaced by a placeholder of the form `[category:UUID]`. The
   UUID is freshly generated for every occurrence — including a value
   that repeats elsewhere in the same request — and never reused. A
   placeholder that repeated across occurrences would let anyone who
   sees the redacted text learn that two fields hold the same value,
   which defeats part of the point of redacting it.
5. Before the redacted request is forwarded, the real value of every
   occurrence is written to the configured cache, keyed by its
   placeholder and tagged with its category. If that write cannot
   complete, the call is denied — see *Failure behavior* — rather than
   forwarded partially redacted or not redacted at all.
6. The built-in categories, each matched independently:
   - **Email address.**
   - **Social Security Number**, as one category covering the US, UK and
     France national formats; any of the three is detected under this
     one category.
   - **API key**, covering common vendor-prefixed secret formats and
     generic high-entropy bearer-style tokens of typical key length.
     Exact patterns are a technical-spec concern.
7. Each category can be enabled or disabled independently. A disabled
   category's pattern is never matched, and PII of that category passes
   through unredacted while the others are still detected in the same
   request.
8. The stage is enabled, and all three categories are active, by
   default. An operator who configures nothing gets this protection
   immediately — unlike policies (specs/0004), which do nothing until an
   operator writes a rules file.
9. A request containing no PII matching any enabled category is
   forwarded unchanged and takes exactly the same path as before
   filtering existed.
10. The response is never inspected or rewritten by filtering, streamed
    or not: specs/0001 rule 13 continues to hold exactly as before.
    Filtering still observes every call's outcome (specs/0001 rule 10),
    but that observation does not include scanning response content. If
    the provider's model echoes a placeholder-looking string back in its
    response — because it was never given the real value to reason
    about — that text reaches the caller unchanged.
11. A call denied by an earlier stage (for example policies, specs/0004)
    never reaches filtering (specs/0001 rule 6): nothing is detected and
    nothing is written to the cache for it.

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Stage on/off | Whether filtering is in the pipeline path | On | Off: nothing is scanned or cached; every request is forwarded exactly as the caller sent it. |
| Per-category on/off (email / SSN / API key) | Enables or disables detection for one category | All on | All off: equivalent to the stage being off for detection, though the cache backend still initializes. |
| Cache backend | Which implementation holds detected values: in-memory, Redis, or a custom implementation of the Gateway's cache interface | In-memory | Redis is described but not implemented this version (see *Non-goals*); a custom backend that fails to initialize fails the Gateway at startup, like a misconfigured policy (specs/0004 rule 19). |
| Cache entry lifetime | How long a cached value is kept before it's evicted | A few minutes | Too short: entries disappear almost immediately — harmless today since nothing reads them back yet, but would need raising once a restoration feature exists. Too long: memory grows with sustained traffic in a long-running process. |

With an operator who configures nothing, filtering is on, all three
categories are active, and the in-memory cache backend holds detected
values for the lifetime default above. This is a deliberate departure
from how every other stage defaults (specs/0001 rule 4, specs/0004): the
categories are built-in and safe rather than operator-authored, so there
is no equivalent of "enabled but given nothing to enforce" — enabling the
stage is itself enough to start protecting PII.

## Failure behavior

**Requests fail closed**, consistent with every other stage (specs/0001,
specs/0004): if detection errors or panics, or the cache write for a
detected occurrence cannot complete (the backend is unreachable, full, or
otherwise fails), the call is denied with specs/0001's "governance
unavailable" error naming the filtering stage. The provider receives
nothing — not the original request, and not a partially redacted one.

Filtering has no deliberate denial of its own (see *Behavior*, rule 2);
the failure path above is the only way it stops a call.

Misconfiguration — an unknown category name, or a cache backend that
fails to initialize — fails at Gateway startup, not at the first request,
matching the rest of the Gateway's convention (specs/0001, specs/0004
rule 19).

Responses are entirely unaffected by any filtering failure: filtering
never touches a response (see *Non-goals*), so there is nothing for a
filtering failure to corrupt there.

## What the operator sees

- One log line per request where at least one occurrence was redacted:
  the per-call identifier and, per category, how many occurrences were
  redacted — never the detected values, and never a value's placeholder
  UUID alongside anything that could be traced back to it.
- A request with nothing detected adds no log line, matching specs/0001
  and specs/0004's convention for allowed calls.
- One log line at error level per filtering failure, naming the stage and
  the underlying error (detection panic, cache write failure),
  distinguishable from the "governance unavailable" responses other
  stages produce the same way.
- One line at Gateway startup naming the active cache backend. When it is
  the in-memory default, the line is a warning: the cache is volatile and
  its contents do not survive a restart.

Nothing the Gateway logs, traces, or returns to a caller contains a
detected PII value, the text that matched a pattern, an API key, or an
`Authorization` header value — only category names, counts, and (for a
failure) the error that occurred.

## Acceptance criteria

1. **Given** the default configuration, **when** a request's conversation
   contains an email address, **then** the request forwarded to the
   provider has that occurrence replaced by a placeholder of the form
   `[email:UUID]`, and the provider never receives the real address.
2. **Given** the same request, **when** the same email address appears
   twice in it, **then** each occurrence is replaced by a different
   UUID.
3. **Given** a request containing a US-format SSN, a UK-format SSN and a
   French-format SSN, **when** filtering runs, **then** all three are
   detected and each is replaced with its own `[ssn:UUID]` placeholder.
4. **Given** a request containing an API-key-shaped token, **when**
   filtering runs, **then** it is replaced with an `[api_key:UUID]`
   placeholder.
5. **Given** a request with no PII matching any enabled category,
   **when** it is proxied, **then** it reaches the provider
   byte-identical and the caller's response is unaffected.
6. **Given** the email category disabled in configuration, **when** a
   request contains an email address, **then** it reaches the provider
   unredacted, while PII of other enabled categories in the same request
   is still detected and redacted.
7. **Given** the filtering stage disabled, **when** a request contains
   PII of every built-in category, **then** none of it is redacted and
   the request is forwarded unchanged.
8. **Given** a detected occurrence, **when** the redacted request is
   forwarded, **then** the real value has already been written to the
   configured cache, keyed by that occurrence's placeholder, before the
   request leaves the Gateway.
9. **Given** the default in-memory cache, **when** the Gateway restarts,
   **then** any previously cached values are gone, and a startup log line
   warned about this before the restart happened.
10. **Given** a cache write failure (the backend is unreachable), **when**
    a request contains PII, **then** the call is denied with the
    "governance unavailable" error naming the filtering stage, and the
    provider receives nothing.
11. **Given** a provider response that contains a placeholder-looking
    string, **when** the response reaches the caller, **then** it is
    unchanged — filtering neither detects nor rewrites anything in a
    response.
12. **Given** a request whose conversation text contains no PII but whose
    tool call arguments or tool results do, **when** filtering runs,
    **then** that PII is detected and redacted exactly as conversation
    text is.
13. **Given** a request's `tools` definitions (names, descriptions,
    schemas) containing text that matches a PII pattern, **when**
    filtering runs, **then** it is left untouched, since tool definitions
    are outside the scanned scope.
14. **Given** a request denied by an earlier stage, **when** it is
    denied, **then** filtering never runs: nothing is detected and
    nothing is written to the cache for it.
15. **Given** an unknown category name in configuration, **when** the
    Gateway starts, **then** it exits with an error naming the unknown
    category.
16. **Given** a custom cache implementation that fails to initialize,
    **when** the Gateway starts, **then** it exits with an error rather
    than silently falling back to the in-memory default.

## Open questions

- **Restoring PII in responses.** specs/0001 left open whether filtering
  would need an opt-in "buffer the response fully" mode to restore real
  values where a placeholder was. This spec settles that it does **not**
  attempt restoration at all — not even a buffered one. For whoever
  writes that later spec: the preference gathered while writing this one
  is that a cache-miss during restoration (entry expired, evicted, or the
  backend unreachable) should fall back to a generic marker like
  `[redacted]`, rather than leaking the literal placeholder or failing an
  already-sent response. Whether restoration itself can stay streaming,
  or needs to buffer, is also still open.
- **Exact API-key patterns.** Which vendor prefixes and entropy
  thresholds count is left to the technical spec, and will likely need
  tuning against false positives (long opaque IDs that aren't secrets).
- **Per-jurisdiction SSN toggles.** Worth revisiting if operators need US
  detection without French, or vice versa.
- **Cache entry lifetime default.** The few-minutes default above is a
  placeholder for "short enough that an unused cache doesn't grow
  forever"; it hasn't been tuned against real traffic, and will need
  revisiting once a real reader (restoration) exists.

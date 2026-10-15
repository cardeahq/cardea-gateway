# 0007 — Remove filtering cache (functional)

**Date:** 2026-10-15
**Technical spec:** specs/0007-remove-filtering-cache-technical.md
**Amends:** specs/0006-filtering-functional.md (see *Relationship to 0006*)

## Context

Filtering (specs/0006) redacts PII before a request reaches the
provider, and was built to write every detected value to a cache keyed
by its placeholder, "so a later version of the Gateway can read it
back" and restore it into a response. 0006 itself already put that
restoration out of scope for this version (its *Non-goals*): nothing
reads the cache, nothing is ever going to read it in this version, and
there is no version of the Gateway yet that commits to building
restoration at all.

Carrying the cache anyway costs real things for no present benefit: an
operator-visible "cache backend" control and two tuning knobs
(capacity, lifetime) that configure a feature nobody can observe; a
startup warning about the cache's volatility that exists to caveat a
read path that doesn't exist; and a failure mode — a call denied because
a cache write failed — that exists purely to protect data nothing
reads. Issue #20 asks to remove the cache mechanism and its
configuration outright, and keep only what filtering actually needs:
detecting PII and substituting a placeholder.

## Goals

- Stop writing a detected PII value anywhere once its placeholder has
  been substituted. The real value is used only long enough to build the
  placeholder and is then discarded; nothing in the Gateway retains it,
  in memory or otherwise.
- Keep every other part of specs/0006 unchanged: which categories are
  detected, the placeholder format and its one-fresh-UUID-per-occurrence
  guarantee, what is in and out of scan scope, per-category on/off,
  default-on with zero configuration, and that responses are never
  touched.
- Remove the operator controls and configuration that existed only to
  tune the cache, since there is no cache left to tune.
- Remove the failure mode where a call is denied because a cache write
  failed, since there is no cache write left to fail.
- Remove the startup warning about the cache's volatility, since there
  is no cache left to warn about.

## Non-goals

- Restoring PII into a response. Still out of scope, exactly as 0006
  left it — this spec does not reopen that question, it removes the one
  piece of machinery 0006 had built in anticipation of it. See
  *Relationship to 0006* for what that means for whoever designs
  restoration later.
- Any change to which categories are detected, the patterns they match,
  the placeholder format, or the scanned scope (system prompt, message
  content, tool calls and tool results).
- Any change to how filtering fails when detection itself goes wrong —
  a body it cannot parse, or an internal error while scanning. Those
  still deny the call exactly as 0006 specified; only the cache-specific
  failure mode is removed.
- A Redis or any other cache backend. 0006 reserved the option; this
  spec removes the option along with the default it was reserved
  alongside.

## Users and scenarios

**The operator running a support-ticket agent**, continued from 0006's
scenario. They upgrade again and change no config. Filtering still
redacts the same categories the same way: the provider still never sees
the real email address or key, only `[email:f47ac10b-...]`-style
placeholders, and the agent and its caller still see no difference in
status codes or latency. What's different: the Gateway's startup log no
longer warns about a cache being volatile, because there is no longer a
cache to be volatile. Nothing the operator could previously configure —
a cache backend, how long a value was held, how many entries fit — is
present any more; there is nothing left to tune for filtering beyond
which categories are on.

## Behavior

1. Every behavior specs/0006 states that does not mention the cache is
   unchanged by this spec: detection of the three built-in categories,
   their independent on/off switches, the `[category:UUID]` placeholder
   with a fresh UUID for every occurrence (never reused, even for a
   repeated value), the scanned scope, the stage's position in the
   pipeline, the default-on posture, and that responses are never
   inspected or rewritten.
2. 0006 rule 5 — "the real value of every occurrence is written to the
   configured cache, keyed by its placeholder... If that write cannot
   complete, the call is denied" — no longer applies. Filtering
   substitutes the placeholder and discards the real value; nothing
   about that value is written anywhere, kept anywhere, or recoverable
   by any means once the redacted request has been built.
3. Because there is no cache, there is no cache-write failure. The only
   way filtering still denies a call is the same one 0006 kept
   independent of caching: detection itself erroring or panicking (an
   unparseable body, or an internal failure while scanning). See
   *Failure behavior*.
4. The operator controls 0006 named "Cache backend", "Cache entry
   lifetime" and "Cache capacity" no longer exist. There is nothing
   related to caching left to configure for filtering.
5. The startup log line 0006 required, naming the active cache backend
   and warning when it is the volatile in-memory default, is removed.
   There is no cache backend to name.

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Stage on/off | Whether filtering is in the pipeline path | On | Off: nothing is scanned; every request is forwarded exactly as the caller sent it. |
| Per-category on/off (email / SSN / API key) | Enables or disables detection for one category | All on | All off: equivalent to the stage being off for detection. |

With an operator who configures nothing, filtering is on and all three
categories are active — unchanged from 0006, minus any mention of a
cache backend, since none exists any more.

## Failure behavior

**Requests still fail closed**, unchanged from 0006, but on fewer
conditions: if detection errors or panics, the call is denied with
specs/0001's "governance unavailable" error naming the filtering stage,
and the provider receives nothing. A call can no longer be denied
because a cache write failed — that condition no longer exists.

Filtering still has no deliberate denial of its own; the failure path
above is still the only way it stops a call.

Responses remain entirely unaffected by any filtering failure, as
before.

## What the operator sees

- One log line per request where at least one occurrence was redacted,
  unchanged from 0006: the per-call identifier and, per category, how
  many occurrences were redacted — never the detected values.
- A request with nothing detected adds no log line, unchanged.
- One log line at error level per filtering failure, naming the stage
  and the underlying error, unchanged — except that a cache-write
  failure can no longer be among the causes, since there is no cache.
- The startup log line naming the active cache backend, and the warning
  that the in-memory default is volatile, no longer appears. Gateway
  startup says nothing about filtering's cache, because filtering has
  none.

Nothing the Gateway logs, traces, or returns to a caller contains a
detected PII value or the text that matched a pattern, unchanged from
0006.

## Acceptance criteria

1. **Given** the default configuration, **when** a request's
   conversation contains an email address, **then** the request
   forwarded to the provider has that occurrence replaced by a
   `[email:UUID]` placeholder exactly as specs/0006 describes, and the
   call succeeds.
2. **Given** the same request, **when** the same email address appears
   twice in it, **then** each occurrence is still replaced by a
   different UUID — the anti-correlation guarantee is unaffected by
   removing the cache.
3. **Given** a configuration file that still sets any of the removed
   cache controls (a cache backend, capacity, or lifetime) under
   filtering's settings, **when** the Gateway starts, **then** it fails
   to start, naming the setting as unrecognized — the same way an
   unknown setting under any other stage's configuration already does,
   since none of those settings exist any more.
4. **Given** a request containing PII, **when** filtering redacts it,
   **then** the call completes without the Gateway attempting to reach,
   write to, or otherwise depend on any cache or store to do so.
5. **Given** the Gateway starts, **when** its startup log is inspected,
   **then** no line names a filtering cache backend or warns about its
   volatility.
6. **Given** a request whose body filtering cannot parse (malformed
   JSON, a duplicate key), **when** filtering runs, **then** the call is
   still denied with the "governance unavailable" error naming the
   filtering stage — this failure mode, unrelated to caching, is
   unaffected by this spec.
7. **Given** the filtering stage disabled, **when** a request contains
   PII of every built-in category, **then** none of it is redacted and
   the request is forwarded unchanged — unaffected by this spec.

## Relationship to 0006

This spec amends specs/0006-filtering-functional.md: its Goals bullet on
keeping replaced values "recoverable... through a cache abstraction",
its rule 5, its *Operator controls* cache rows, the cache-write line of
its *Failure behavior*, and its startup-warning bullet in *What the
operator sees* are all removed, as described above. Its acceptance
criteria 8, 9 and 10 (write-before-forward, the restart warning, and
denial on a cache write failure) no longer apply and are removed; the
rest of 0006's acceptance criteria stand unchanged — this spec's own
acceptance criteria above restate the ones still relevant to caching's
removal rather than duplicate the full set.

0006's *Non-goals* bullet on restoring PII into responses, and its first
*Open questions* entry on what restoration should do, both assumed a
cache already existed for a future spec to read from. With the cache
gone, whoever designs restoration later starts from nothing: there is no
storage to repurpose, and the preference 0006 recorded about a cache-miss
fallback marker no longer has a cache to miss against. That guidance is
superseded, not carried forward.

## Open questions

- **Restoring PII in responses.** Still deferred, as in 0006 — but
  whoever picks this up next designs its storage from scratch, since
  this spec removes the only storage filtering had.

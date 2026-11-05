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
- Replace each detected occurrence with a placeholder naming its
  category, so the provider never receives the real value.
- Give every operator this protection for a fixed set of built-in
  categories with zero configuration — no file to write, unlike policies
  (specs/0004).
- ~~Keep every replaced value recoverable by its placeholder, through a
  cache abstraction an operator can run in-memory (the default), swap for
  Redis, or replace with an implementation of their own — so a later
  version of the Gateway can read it back.~~ *Removed by specs/0007: no
  detected value is written anywhere once its placeholder has been
  substituted.*
- Never let a placeholder reveal anything about the value it replaced —
  including whether two occurrences hold the same value: every
  occurrence of a category is replaced by the same, fixed placeholder.
- Mark every request record whose content was redacted, naming the
  categories that were found and removed, so an operator can see from
  the trace alone that the provider received an altered request.
- Behave exactly as today for a request that contains none of the known
  PII categories.

## Non-goals

- **Restoring PII into the response.** This version wrote detected
  values to a cache that nothing read back. Responses are relayed
  exactly as the provider sent them (specs/0001 rule 13) — unchanged even
  if the provider echoes a placeholder back verbatim. What restoration
  should do is deferred to a later spec; see *Open questions* for the
  guidance gathered while writing this one. *Amended by specs/0007: the
  cache itself is removed, so that later spec also designs its storage
  from scratch — see specs/0007's Relationship to 0006.*
- ~~**A Redis-backed cache.** The pluggable interface is defined (see
  *Operator controls*) so one can be added without changing this spec,
  but only the in-memory implementation ships with this version.~~
  *Removed by specs/0007: there is no cache, pluggable or otherwise, any
  more.*
- **Categories beyond email, SSN (US, UK, France) and API key** — phone
  numbers, credit card numbers, physical addresses, personal names, and
  so on.
- **Per-jurisdiction toggles within the SSN category.** An operator
  cannot enable US detection while disabling French detection; the
  category is on or off as a whole.
- **Operator-defined detection patterns.** The three categories and their
  patterns are fixed in this version. *Amended by specs/0007: there is no
  longer a cache backend to be pluggable either.*
- **A generic, high-entropy "this looks like a secret" detector for API
  keys.** Only fixed, vendor-prefixed formats are matched; there's no
  reliable way to tell a real secret from an ordinary long ID, hash or
  UUID without more signal than this stage has.
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
only placeholders like `[REDACTED:email]` — while the agent and its
caller see no difference in status codes or latency. The operator's logs
show one line per request with something redacted: which categories, how
many occurrences, never the values themselves. *Amended by specs/0007:
there is no longer a one-time startup warning about a cache's
volatility, since filtering no longer caches anything.*

**The agent developer.** A permitted call behaves exactly as without
filtering, except that the provider is reasoning over placeholders where
PII used to be. If their prompt asked the model to validate or act on the
actual value — "does this look like a valid SSN?" — the model now sees
only `[REDACTED:ssn]` and cannot do that; this is the deliberate cost of
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

   This holds for every provider and every supported operation that
   carries content (specs/0009, *Supported operations*), read in the
   request's own format:

   | Operation | Scanned | Not scanned |
   | --- | --- | --- |
   | Anthropic messages, count tokens | System prompt (string or text blocks); text blocks of every message; `input` of every `tool_use` block; content of every `tool_result` block (string or text blocks) | `thinking` blocks, images, documents, tool definitions, model, generation settings |
   | OpenAI-style chat | Text of every message whatever its role (system, developer, user, assistant, tool), as a plain string or as text parts; argument values of every tool call an assistant message carries | Tool and function definitions, response-format schemas, image and audio parts, model, generation settings |
   | OpenAI-style fill-in-the-middle | The prompt and the suffix | Model, generation settings, stop sequences |
   | OpenAI-style embeddings | Every input text | Model, encoding settings |
   | Gemini generate (streamed or not), count tokens | Text parts of the system instruction and of every content entry; argument values of every function call; the response of every function response; the same parts when count tokens wraps a full generate request | Tool and function declarations, inline binary data, file references, safety and generation settings, cached-content references |
   | Gemini embed, batch embed | Text parts of the content, or of every request's content in a batch | Model, task type, title, output settings |

   Model listings carry no content: filtering runs for them and has
   nothing to redact.

   Tool-call arguments are redacted as values: each string value inside
   them is scanned, and they reach the provider in the form the format
   requires. OpenAI-style arguments are a JSON document carried as a
   string, and remain one. Arguments that are not valid JSON where the
   format expects JSON are scanned as plain text. Redaction changes only
   the scanned text; every other field, including ones the Gateway does
   not know, reaches the provider with the same value.
4. Every occurrence of a known, enabled category's pattern within that
   scope is replaced by the placeholder `[REDACTED:category]` — for
   example `[REDACTED:email]`. The placeholder is the same for every
   occurrence of a category and carries nothing derived from the value,
   so the redacted text reveals neither the value nor whether two
   occurrences held the same one. The `REDACTED:` prefix tells the model
   (and anyone reading the text) that a value was deliberately removed,
   rather than leaving a bare `[email]` that reads like a template slot
   to fill in.
5. ~~Before the redacted request is forwarded, the real value of every
   occurrence is written to the configured cache, keyed by its
   placeholder and tagged with its category. If that write cannot
   complete, the call is denied — see *Failure behavior* — rather than
   forwarded partially redacted or not redacted at all.~~ *Removed by
   specs/0007: the real value is discarded once its placeholder has been
   substituted; nothing is written anywhere, and there is no longer a
   cache-write failure mode.*
6. The built-in categories, each matched independently:
   - **Email address.**
   - **Social Security Number**, as one category covering the US, UK and
     France national formats; any of the three is detected under this
     one category.
   - **API key**, covering common vendor-prefixed secret formats (for
     example a key that starts `sk-`, `sk-proj-`, `ghp_`, `AKIA`). A standalone
     token with no recognizable prefix is not detected under this
     category in this version, even if it looks like a secret — see
     *Non-goals*. Exact patterns are a technical-spec concern.
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
    never reaches filtering (specs/0001 rule 6): nothing is detected for
    it. *Amended by specs/0007: the original wording ("nothing is
    written to the cache for it") no longer applies — there is no
    cache.*

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Stage on/off | Whether filtering is in the pipeline path | On | Off: nothing is scanned; every request is forwarded exactly as the caller sent it. |
| Per-category on/off (email / SSN / API key) | Enables or disables detection for one category | All on | All off: equivalent to the stage being off for detection. |
| ~~Cache backend~~ | ~~Which implementation holds detected values.~~ | — | *Removed by specs/0007: there is no cache.* |
| ~~Cache entry lifetime~~ | ~~How long a cached value is kept before it's evicted.~~ | — | *Removed by specs/0007.* |
| ~~Cache capacity~~ | ~~How many entries the in-memory cache holds before it evicts the oldest to make room.~~ | — | *Removed by specs/0007.* |

With an operator who configures nothing, filtering is on and all three
categories are active. This is a deliberate departure from how every
other stage defaults (specs/0001 rule 4, specs/0004): the categories are
built-in and safe rather than operator-authored, so there is no
equivalent of "enabled but given nothing to enforce" — enabling the
stage is itself enough to start protecting PII. *Amended by specs/0007:
the original wording also described a cache backend default, which no
longer applies.*

## Failure behavior

**Requests fail closed**, consistent with every other stage (specs/0001,
specs/0004): if detection errors or panics, the call is denied with
specs/0001's "governance unavailable" error naming the filtering stage.
The provider receives nothing — not the original request, and not a
partially redacted one. *Amended by specs/0007: the original wording
also denied a call whose cache write failed; there is no cache, so that
condition is removed.*

Filtering has no deliberate denial of its own (see *Behavior*, rule 2);
the failure path above is the only way it stops a call.

Misconfiguration — an unknown category name — fails at Gateway startup,
not at the first request, matching the rest of the Gateway's convention
(specs/0001, specs/0004 rule 19). *Amended by specs/0007: a cache
backend that fails to initialize is no longer a case, since there is no
cache backend.*

Responses are entirely unaffected by any filtering failure: filtering
never touches a response (see *Non-goals*), so there is nothing for a
filtering failure to corrupt there.

## What the operator sees

- One log line per request where at least one occurrence was redacted:
  the per-call identifier and, per category, how many occurrences were
  redacted — never the detected values.
- On the call's request record (specs/0003), `redacted: true` and
  `redacted_categories` listing, sorted, every category found and
  removed (for example `["api_key", "email"]`). A request with nothing
  redacted has `redacted: false` and `redacted_categories: []`. The
  per-category counts stay in the filtering stage's `detail.redactions`.
- A request with nothing detected adds no log line, matching specs/0001
  and specs/0004's convention for allowed calls.
- One log line at error level per filtering failure, naming the stage and
  the underlying error (a detection panic), distinguishable from the
  "governance unavailable" responses other stages produce the same way.
  *Amended by specs/0007: a cache write failure is no longer a possible
  cause, since there is no cache.*
- ~~One line at Gateway startup naming the active cache backend. When it
  is the in-memory default, the line is a warning: the cache is volatile
  and its contents do not survive a restart.~~ *Removed by specs/0007:
  there is no cache backend to name.*

Nothing the Gateway logs, traces, or returns to a caller contains a
detected PII value, the text that matched a pattern, an API key, or an
`Authorization` header value — only category names, counts, and (for a
failure) the error that occurred.

## Acceptance criteria

1. **Given** the default configuration, **when** a request's conversation
   contains an email address, **then** the request forwarded to the
   provider has that occurrence replaced by a placeholder of the form
   `[REDACTED:email]`, and the provider never receives the real address.
2. **Given** the same request, **when** the same email address appears
   twice in it, **then** both occurrences are replaced by the identical
   `[REDACTED:email]` placeholder, exactly as two different addresses
   would be.
3. **Given** a request containing a US-format SSN, a UK-format SSN and a
   French-format SSN, **when** filtering runs, **then** all three are
   detected and each is replaced with a `[REDACTED:ssn]` placeholder.
4. **Given** a request containing an API-key-shaped token, **when**
   filtering runs, **then** it is replaced with an `[REDACTED:api_key]`
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
8. ~~**Given** a detected occurrence, **when** the redacted request is
   forwarded, **then** the real value has already been written to the
   configured cache, keyed by that occurrence's placeholder, before the
   request leaves the Gateway.~~ *Removed by specs/0007: see that spec's
   own acceptance criteria.*
9. ~~**Given** the default in-memory cache, **when** the Gateway
   restarts, **then** any previously cached values are gone, and a
   startup log line warned about this before the restart happened.~~
   *Removed by specs/0007.*
10. ~~**Given** a cache write failure (the backend is unreachable),
    **when** a request contains PII, **then** the call is denied with
    the "governance unavailable" error naming the filtering stage, and
    the provider receives nothing.~~ *Removed by specs/0007: there is no
    cache write to fail.*
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
    are outside the scanned scope. This holds for Anthropic tools,
    OpenAI-style tools and Gemini function declarations alike.
14. **Given** a request denied by an earlier stage, **when** it is
    denied, **then** filtering never runs: nothing is detected for it.
    *Amended by specs/0007: "nothing is written to the cache for it" no
    longer applies.*
15. **Given** an unknown category name in configuration, **when** the
    Gateway starts, **then** it exits with an error naming the unknown
    category.
16. ~~**Given** a cache backend configured to anything other than
    "in-memory" (including "redis"), **when** the Gateway starts,
    **then** it exits with an error naming the value, rather than
    silently falling back to the in-memory default.~~ *Removed by
    specs/0007: see that spec's acceptance criterion 3 for what setting
    any of the removed cache controls now does instead (rejected as an
    unrecognized setting).*
17. **Given** the default configuration and a Mistral provider, **when**
    an OpenAI-style chat request has an email in a system message, in a
    tool-role message and in a string value of an assistant tool call's
    arguments, **then** each is replaced by its own placeholder, and the
    tool call's arguments are still a string holding valid JSON.
18. **Given** the same, **when** a fill-in-the-middle request's prompt
    holds an API key and its suffix an SSN, **then** both are redacted.
19. **Given** the same, **when** an embeddings request's input is a list
    of texts, one holding an email, **then** that text is redacted and the
    others reach the provider unchanged.
20. **Given** the default configuration and a Gemini provider, **when** a
    generate request has an email in its system instruction, in a user
    text part and in a function response, **then** all three are
    redacted.
21. **Given** the same, **when** a streaming generate request carries an
    API key, **then** it is redacted before forwarding, and the streamed
    response reaches the caller unchanged.
22. **Given** the same, **when** a count-tokens request wraps a full
    generate request holding an SSN, or a batch embed request holds an
    email in its second request, **then** each is redacted.
23. **Given** an OpenAI-style or Gemini request with no PII, **when** it
    is proxied, **then** it reaches the provider byte-identical.
24. **Given** filtering on, **when** an OpenAI-style chat or Gemini
    generate request body is not valid JSON, **then** the call is denied
    with "governance unavailable" naming filtering and the provider
    receives nothing.
25. **Given** a request containing an email and an API key, **when** it
    is forwarded, **then** its request record has `redacted: true` and
    `redacted_categories: ["api_key", "email"]`; **given** a request
    with nothing redacted, its record has `redacted: false` and
    `redacted_categories: []`.

## Relationship to 0007

specs/0007 amends this feature: every part of it that depended on the
detected-value cache — the Goals bullet on recoverability, the
"Restoring PII into the response" and "A Redis-backed cache" Non-goals
bullets, rule 5 and the cache mentions in rules 11 and 14, the Cache
backend / entry lifetime / capacity operator controls, the cache-write
line of *Failure behavior*, the startup-warning bullet of *What the
operator sees*, and acceptance criteria 8, 9, 10 and 16 — is removed, as
marked above. What filtering detects, how it places placeholders, its
scope, its defaults, and its remaining (non-cache) failure behavior are
unchanged.

## Open questions

- **Restoring PII in responses.** specs/0001 left open whether filtering
  would need an opt-in "buffer the response fully" mode to restore real
  values where a placeholder was. This spec settles that it does **not**
  attempt restoration at all — not even a buffered one. *Amended by
  specs/0007: the preference originally recorded here — a cache-miss
  during restoration falling back to a generic `[redacted]` marker — no
  longer applies, since specs/0007 removes the cache that preference
  assumed existed. Whoever designs restoration next starts its storage
  from scratch; see specs/0007's Relationship to 0006.*
- **Exact API-key patterns.** Which vendor prefixes and entropy
  thresholds count is left to the technical spec, and will likely need
  tuning against false positives (long opaque IDs that aren't secrets).
- **Per-jurisdiction SSN toggles.** Worth revisiting if operators need US
  detection without French, or vice versa.
- ~~**Cache entry lifetime default.**~~ *Removed by specs/0007: there is
  no cache entry lifetime any more.*

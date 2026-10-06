# 0005 — Cardea sink (functional)

**Date:** 2026-10-06
**Technical spec:** specs/0005-cardea-sink-technical.md
**Amends:** specs/0003-call-tracing-functional.md (see *Relationship to 0003*)

## Context

The Gateway records every request and response it relays (spec 0003),
but it can only write those records locally: to its standard output or to
a file. An operator who runs several Gateways, or who wants to search and
analyze what agents did across an organization, has to collect those
files themselves. The records exist; they go nowhere.

The Cardea platform is the service meant to receive them: it stores the
records of an organization, keeps them for as long as that organization
requires, and lets people filter and search them. The Gateway needs a way
to deliver its records to it.

Delivery has to respect two things the Gateway already promises. Records
carry metadata and keyed fingerprints, never prompt content, tool
arguments or tool results (0003). And the Gateway does not decide
anything about who the data belongs to: a Gateway is deployed by an
organization, and the platform knows which organization from the
credential the Gateway presents, not from anything in the records.

A network service raises a question 0003 left open: today, a call whose
request record the sink cannot take is refused. With a sink that depends
on a remote service, that would make the platform a hard dependency of
every call, and with two sinks, it would let one sink record a call that
the other sink's failure then refused. This spec settles it: **recording
never refuses a call.**

## Goals

- Deliver every request and response record the Gateway produces to the
  Cardea platform, without the operator running a collector.
- Identify the organization (the **tenant**) from the credential the
  Gateway presents to the platform alone. Records and Gateway settings
  carry no tenant identifier, and nothing a caller sends can choose one.
- Keep recording off the path of the agent's calls: no sink, local or
  remote, ever causes a call to be refused, delayed beyond its own write,
  or altered.
- Make loss visible rather than silent: if records cannot be delivered,
  the platform can tell that records are missing, and the operator can
  tell how many.
- Let the operator keep the local record stream alongside delivery to the
  platform.
- Define the delivery contract precisely enough that the platform's
  intake can be built against it independently.

## Non-goals

- The platform itself: how it maps credentials to tenants, stores and
  retains records, and offers filtering, facets and search. That is
  specified on the platform's side. This spec states only what the Gateway
  sends and what it expects back.
- A strict mode in which a call that cannot be recorded is refused. This
  spec removes that behavior; an operator who needs it would need a later
  spec that adds it back as an explicit choice.
- Cryptographic tamper-evidence. This version gives the platform enough
  to detect that records are missing (rule 10); chaining or signing
  records is a later step.
- Delivery of anything other than trace records: policy decisions are
  already part of them (0004); application logs and metrics are not
  delivered.
- Surviving a Gateway restart with undelivered records. Records not yet
  delivered when the Gateway stops are lost; the local record stream is
  the safety net.
- Per-tenant settings in the Gateway, or one Gateway serving several
  tenants with several credentials.
- A default platform address. The operator always states it (it may
  change later).
- Changing the content of records. The record format of 0003 (extended by
  0004) is delivered as it is.

## Users and scenarios

**An operator connects a Gateway to the platform.** They create an API
key for their organization on the platform, put it in the environment of
the Gateway, name the platform's address in the Gateway's settings, and
select the Cardea sink. Agents keep calling the Gateway as before. Within
seconds, the records of those calls are visible on the platform under the
organization the key belongs to. The operator also keeps the local
records, as a copy under their control.

**A developer works locally.** They run a stand-in for the platform on
their own machine and point the Gateway at it with a plain `http://`
address on localhost, with any key. No certificate is needed.

**The platform has an outage.** Agents keep working and keep being
recorded locally. The Gateway holds recent records for a while and
retries. When the platform comes back, the held records arrive. If the
outage is longer than the Gateway can hold records for, new records are
dropped, the operator is told how many, and the platform, on recovery,
can see exactly how many records it never received from that Gateway.

**A key is revoked.** The platform refuses the Gateway's key. The Gateway
keeps serving agents, tells the operator plainly that the platform
rejected its credential, and does not send the same refused records again.

**The local disk is full.** The local sink cannot write. Agents keep
working; the operator sees errors naming the lost records. With
`logging+cardea`, the platform still receives them.

## Behavior

1. **A new sink type, `cardea`.** The operator can select it for call
   tracing. A third choice, `logging+cardea`, selects both the local
   stream and the platform. The default is the local stream only, as
   today.
2. **What is delivered.** Every record the Gateway hands to its sink, as
   specified in 0003 (and extended by 0004), unchanged, request and
   response records alike. The sink adds nothing to a record's content;
   it adds only the delivery details of rule 10.
3. **The sink never waits for the platform.** Handing a record to the
   Cardea sink returns once the record is held for delivery, not once the
   platform has it. Delivery happens in the background. The latency an
   agent sees does not depend on the platform.
4. **Records are delivered in batches,** sent when a batch holds a set
   number of records, reaches a set size, or a set time has passed since
   the first record in it, whichever comes first. A batch never exceeds
   the set size, except that a single record larger than it is sent alone.
   A batch is compressed.
5. **Two settings are mandatory when the Cardea sink is selected: the
   platform address and the API key.** Neither has a default.
6. **Authentication.** Every delivery carries the API key. The key is
   read from an environment variable the operator names; it is never in
   the settings file, never in a record, and never in a log.
7. **Secure connection.** Delivery is only over a secure connection
   (`https`), except to the machine the Gateway runs on (`localhost`,
   `127.0.0.1`, `::1`), where plain `http` is accepted for development.
   The Gateway never follows a redirect: the key is only ever sent to the
   address the operator configured.
8. **At least once.** A batch is retried until the platform accepts it,
   so a record can arrive more than once. Each record is identifiable so
   the platform can discard duplicates (rule 10).
9. **What is retried, and what is not.**
   - A delivery that fails because the platform is unreachable, times
     out, answers that it is busy, or fails on its side is retried after
     a growing pause: 1 second at first, doubling each time up to 30
     seconds, each pause randomly shortened by up to half so that several
     Gateways do not retry in step. If the platform asks the Gateway to
     wait, the pause is at least that long, up to 5 minutes. Retrying
     never stops while the Gateway runs and the batch is still held.
   - If the platform refuses the credential or refuses the batch as
     malformed or too large, resending the same batch cannot help. The
     batch is dropped and counted as lost; the operator is told why; the
     Gateway carries on with the next batch. A credential problem keeps
     being reported for as long as the Gateway produces records, but at a
     limited rate.
   - If the Gateway itself fails while preparing a batch, that batch is
     dropped, counted as lost and reported, and delivery carries on with
     the next batch.
10. **Loss is detectable.** The Gateway has an **instance identity**, a
    random value fixed at startup, and numbers every record it is handed,
    in order, starting at 1, with no number skipped. The platform
    receives both with each record. A record that is lost before delivery
    still uses up its number, so the platform sees a gap exactly where
    records were lost; a repeated number is a duplicate. A restarted
    Gateway has a new identity and starts again at 1.
11. **Limited holding capacity.** The Gateway holds at most a set number
    of undelivered records and a set total size of them. When either is
    reached, a new record is not held: it is counted and reported, and the
    agent's call is unaffected. Records already held are kept.
12. **No tenant in the Gateway.** No setting, record field or header the
    Gateway sends names a tenant. The platform derives it from the key.
    Operators of several organizations run one Gateway per organization,
    each with its own key.
13. **No sink ever refuses a call.** Neither the Cardea sink nor the
    local sink can cause a call to be denied, delayed or altered. A sink
    that cannot take a record loses it: the failure is logged at error
    level and the call carries on. A record the sink fails to write is
    named in the log by its call and record type; records lost in bursts
    (for capacity, or because the platform refused them) are reported as
    counts, as *What the operator sees* describes. With
    `logging+cardea`, each sink receives every record independently; one
    failing never prevents the other from receiving it.
14. **Shutdown.** When the Gateway is asked to stop, it stops accepting
    calls, then tries to deliver what it holds for at most a set time.
    Whatever is left after that is lost and counted.
15. **Startup checks.** The Gateway refuses to start if the Cardea sink is
    selected and the address is missing, malformed, not secure (other than
    the Gateway's own machine) or carries credentials in it; if the key
    variable is unset, empty, or holds a value with spaces or control
    characters; or if any delivery setting is out of range. It does
    **not** check at startup that the platform is reachable or that the
    key is valid: a Gateway must be able to start while the platform is
    down.
16. **Agents cannot tell.** An agent cannot observe whether the Cardea
    sink is on, slow, failing or full.

## Delivery contract

What the Gateway sends and expects. The platform's intake is built to
this.

- **Request.** One `POST` per batch to the configured address, used as
  given (the Gateway appends nothing to it).
  - Headers, all sent on every request:

    | Header | Meaning |
    | --- | --- |
    | `Authorization: Bearer <API key>` | The credential; identifies the tenant |
    | `Content-Type: application/x-ndjson` | The body format |
    | `Content-Encoding: gzip` | The body is compressed |
    | `X-Cardea-Instance` | The instance identity (rule 10), 32 hexadecimal characters |
    | `X-Cardea-First-Seq`, `X-Cardea-Last-Seq` | The numbers of the first and last record in the batch |
    | `X-Cardea-Schema` | The record schema, `agentkeeper.trace/v1` |
    | `User-Agent: agentkeeper-gateway` | Identifies the sender |

  - Body: compressed (gzip) newline-delimited JSON, one line per record.
    Each line is an object with the record's `seq` (its number, rule 10)
    and the `record` itself exactly as in 0003.
  - Records within a batch are in increasing `seq` order. Batches of one
    instance may be retried and so may arrive more than once or after a
    later batch; the platform orders and deduplicates by (instance
    identity, `seq`).
- **Answer.** The Gateway acts on the status code only; it does not read
  the body.

  | Status | Meaning to the Gateway |
  | --- | --- |
  | 2xx | The whole batch is accepted. Never partially. |
  | 400, 413, 422 | The batch is refused as it stands. Dropped, counted, reported. |
  | 401, 403 | The credential is refused. Dropped, counted, reported as a credential problem. |
  | 408, 429, 5xx, no answer | Retry later; honor `Retry-After` when present (rule 9). |
  | Other 4xx, 3xx | Treated like 400. |

- **Idempotence.** Accepting the same batch twice has the same effect as
  accepting it once.

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Sink type | `logging`, `cardea` or `logging+cardea` | `logging` | — |
| Platform address | Where batches are sent | none; required for `cardea` | Must be `https`, or `http` on the Gateway's own machine |
| API key variable | Name of the environment variable holding the key | none; required for `cardea` | Unset, empty, or a value with spaces or control characters: startup fails |
| Holding capacity (records) | Most undelivered records held | 10 000 | Must be at least one batch; when full, new records are dropped |
| Holding capacity (size) | Most undelivered bytes held | 64 MiB | Must be at least one batch; when full, new records are dropped |
| Batch records | Records that trigger sending | 500 | 1 sends every record on its own |
| Batch size | Uncompressed bytes that trigger sending | 1 MiB | Must be below the platform's own limit |
| Batch delay | Longest a record waits for company | 1 s | Lower is more current, more requests |
| Request timeout | Longest one delivery may take | 10 s | A timed-out delivery is retried |
| Shutdown delay | Longest time spent delivering on stop | 10 s | 0 drops everything held |

With an operator who configures nothing, the Cardea sink is **off**: the
default stays the local stream, which needs no credential and no network.
Sending data off the machine must be a deliberate act, which is also why
the address and the key have no defaults.

## Failure behavior

**Recording fails open, always.** No failure of a sink, whether the
platform is slow, unreachable, full or refusing the credential, or the
local file cannot be written, affects an agent: the call is served as it
would have been, and the loss is counted, reported, and visible to the
platform as a gap in numbering. The platform is a separate service the
operator does not control from the Gateway, and the local disk is the
operator's; making either a condition for serving an agent would trade
the agents' availability for completeness.

The price is that a call can be served with no record of it. That is why
loss is made visible (rules 10 and 13 and *What the operator sees*) and
why the operator is encouraged to run `logging+cardea`: a record lost by
one sink is usually held by the other.

Failures of tracing itself, as opposed to a sink, are unchanged by this
spec: 0003 still denies a call whose tool results tracing cannot read.

Misconfiguration fails at startup (rule 15).

## What the operator sees

- At startup: one line saying the Cardea sink is active, with the
  platform host (no path, no credentials) and the instance identity.
- **Every record a sink loses is reported in the Gateway's runtime logs at
  error level**, whatever the cause: dropped for lack of capacity, refused
  by the platform, undeliverable at shutdown, or not written locally. For
  a burst of losses of the same cause (capacity, credential), one line at
  most every 10 seconds carries the number lost since the last, so a long
  outage does not flood the logs; a loss of a single record the sink could
  not write names the call and the record.
- When the platform refuses the credential: an error, at most every 10
  seconds, saying the credential was refused and how many records were
  dropped since. It never prints the key.
- When a batch is refused as malformed or too large, or dropped because
  the Gateway failed preparing it: an error with the reason and the
  number of records dropped.
- When deliveries are failing and being retried: a warning at the first
  failure and again every minute while it persists, with the reason
  (timeout, unreachable, status) and the number of records held; and an
  info line when delivery recovers.
- When a sink loses a record (the local file cannot be written, for
  example): an error naming the call and the record, as 0003 describes.
- At shutdown: an info line with the number delivered and the number
  lost.

A sink failure is never visible to the agent: there is no denial to tell
it apart from.

Nothing surfaced contains prompt content, tool arguments or results, the
API key, an `Authorization` header or the platform address's path or
query.

## Acceptance criteria

1. **Given** the default settings **when** the Gateway starts and relays
   calls **then** nothing is sent to any address and no key is required.
2. **Given** `cardea` selected, a reachable platform and a valid key
   **when** an agent makes a call **then** both its records reach the
   platform within the batch delay plus the request timeout, each exactly
   as 0003 describes it, carrying a number and the instance identity.
3. **Given** `cardea` selected and a platform that never answers **when**
   an agent makes a call **then** the agent receives its response with the
   latency it would have had without the sink.
4. **Given** `logging+cardea` and an unreachable platform **when** an agent
   makes calls **then** every record is written to the local stream.
5. **Given** the platform answers 503 to a batch and 202 to the retry
   **then** the batch is delivered once the retry succeeds and no number is
   missing.
6. **Given** the platform answers 429 with `Retry-After: 5` **then** the
   next attempt is not made before 5 seconds have passed; **and given**
   `Retry-After: 3600` **then** it is not made before 5 minutes have passed
   and is made soon after.
7. **Given** the platform answers 401 **then** the batch is not resent,
   the operator sees a credential error without the key, the records are
   counted as lost, and the Gateway keeps relaying calls.
8. **Given** the platform answers 400 to a batch **then** that batch is
   dropped and the next batch is still sent.
9. **Given** the platform answers with a redirect to another address
   **then** the Gateway does not follow it, the other address never
   receives the key, and the batch is dropped as refused.
10. **Given** holding capacity of 2 records and an unreachable platform
    **when** 5 records are produced **then** the third to fifth are
    dropped, the operator sees a warning, no call is affected, and when
    the platform returns, the numbers delivered show a gap exactly where
    the dropped records were. The operator sees an error-level log line
    stating how many records were dropped.
11. **Given** a holding capacity in bytes smaller than three records and an
    unreachable platform **when** 5 records are produced **then** the
    records that did not fit are dropped and reported, exactly as in
    criterion 10.
12. **Given** a batch size of 1 500 bytes and records of 600 bytes each
    (as sent, envelope included) **when** 5 records are delivered **then**
    no request carries more than two records; **and given** one record of
    4 500 bytes **then** it is sent alone.
13. **Given** the Gateway fails while preparing a batch **then** that batch
    is dropped and counted, an error is logged, and the next batch is
    delivered.
14. **Given** `cardea` selected, an unreachable platform and a full
    holding capacity **when** an agent makes a call **then** the call is
    served normally, not denied.
15. **Given** `logging` selected and a local file that cannot be written
    **when** an agent makes a call **then** the call is served normally,
    an error-level log line names the call and the lost record, and no
    denial results.
16. **Given** `logging+cardea` and a local file that cannot be written
    **when** an agent makes a call **then** the platform still receives
    both records and the call is served normally.
17. **Given** `logging+cardea` and an unreachable platform **when** an
    agent makes a call **then** the local stream still gets both records
    and the call is served normally.
18. **Given** a Gateway restarted **then** its instance identity differs and
    its numbering starts again at 1.
19. **Given** the address `http://platform.example.com` **when** the
    Gateway starts **then** it refuses to start; `http://localhost:9000`,
    `http://127.0.0.1:9000` and `https://platform.example.com` are accepted.
20. **Given** `cardea` selected and no address **when** the Gateway starts
    **then** it refuses to start naming the missing setting.
21. **Given** the key variable is unset, empty, or holds a value with a
    space **when** `cardea` is selected **then** the Gateway refuses to
    start naming the variable, not its value.
22. **Given** `cardea` selected and an unreachable platform **when** the
    Gateway starts **then** it starts, serves calls, and makes no
    connection to the platform before it has a record to deliver.
23. **Given** held records and a reachable platform **when** the Gateway is
    stopped **then** the held records are delivered before it exits, within
    the shutdown delay.
24. **Given** the Gateway starts with `cardea` selected **then** it logs one
    line naming the platform host and the instance identity, and not the
    key or the address's path.
25. **Given** any scenario above **then** no log line, record or request
    contains the key value, prompt content, tool arguments or results.
26. **Given** any settings **then** no setting, record field or sent header
    names a tenant.

## Relationship to 0003

This feature amends 0003, and 0003 is updated in the same change:

- **Rule 23 and *Failure behavior*.** 0003 denied a call whose request
  record the sink did not accept. A sink never refuses a call any more
  (rule 13 here): the failure is logged and the record is lost. Failures
  of tracing itself, such as tool results it cannot read, still deny the
  call as before.
- **Rule 24.** 0003 allowed exactly one active sink. More than one may now
  be active (`logging+cardea`).
- **Acceptance criteria 11 and 12** of 0003 describe a refusing sink; they
  are replaced.

## Open questions

- ~~Header names.~~ Confirmed: the delivery headers are `X-Cardea-*`.
- The use of the configured address as the full endpoint (the Gateway
  appends no path) is proposed here; the platform team confirms it before
  implementation. — platform side.
- Should undelivered records survive a restart (spool to disk)? Proposed:
  no for the MVP; the local stream covers it. — you.
- Should the Gateway expose counters (delivered, dropped, retried, held)
  for scraping, beyond the log lines above? Waits for the Gateway to have
  a metrics surface. — you.

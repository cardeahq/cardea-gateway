# 0005 — Cardea sink (technical)

**Date:** 2026-10-06
**Functional spec:** specs/0005-cardea-sink-functional.md
**Amends:** specs/0003-call-tracing-technical.md (pipeline steps 3 and 6, failure table)

## Context

Spec 0003 hands every request and response record to a `trace.Sink`, and
ships one implementation, `LogSink`, a local append. This spec adds the
second: a sink that delivers records to the Cardea platform over HTTPS.
Behavior, the delivery contract and the acceptance criteria are in the
[functional spec](0005-cardea-sink-functional.md); this one is the design.

Two decisions shape it. First, **no sink ever refuses a call**: 0003's
pipeline denied a call with 503 when the sink refused the request record,
which would make the platform a hard dependency of every call and, with
two sinks, let one record a call that the other's failure then denied.
The pipeline now logs a sink error and carries on. Second, `Write` never
touches the network: it enqueues.

The transport is plain REST: an HTTPS `POST` of gzip-compressed
newline-delimited JSON. It is one-way batch delivery with an ack, which
needs none of what gRPC adds (see *Alternatives considered*).

## Goals

- A `trace.Sink` whose `Write` costs a mutex and a slice append, and
  whose delivery runs in one background goroutine.
- At-least-once delivery with bounded memory, bounded retry pressure and
  gap detection on the receiving side.
- Run beside `LogSink` without either affecting the other.
- A sink failure is an error log and a lost record, never a refused call.
- No tenant anywhere in the Gateway; the credential alone identifies it.
- Nothing about the record format (0003, extended by 0004) changes.

## Non-goals

- The platform's intake and storage.
- Disk spooling, or any state surviving a restart.
- Hash chaining or signing of records.
- A strict mode that refuses calls when recording fails.
- Metrics endpoints (log lines only; see *Open questions*).

## Design

```
pipeline ──Write(rec)──▶ trace.Sink   (an error is logged, never a denial)
                          ├─ LogSink          (local line, unchanged)
                          └─ CardeaSink
                               Write: marshal → lock → seq++ →
                                      enqueue or drop → return
                               worker goroutine:
                                 take head batch → gzip NDJSON → POST
                                 2xx: pop · refused: pop+drop · else: backoff
```

Placement in the data flow is unchanged:

```
cli.serve → config.Config → zap.Logger → provider.Provider → gateway.Gateway
                                                  └─ buildStages → sink
```

The sink observes records only; it sees neither requests nor responses.

**Write path.** `Write` marshals the record with `encoding/json`, then
under one mutex:

1. closed sink → `ErrClosed` (no `seq`);
2. `seq++`. If marshaling failed, the entry is counted lost and the
   marshal error is returned, but **its `seq` is consumed**, so the
   platform sees the gap;
3. queue full (`queue_size` records, or `queue_max_bytes` of encoded
   entries, whichever is hit first): the entry is counted dropped, **its
   `seq` is consumed**, and `Write` returns nil. There is no reject mode;
4. otherwise append `{"seq":N,"record":<json>}\n` to the queue and, if a
   batch threshold is now met, signal the worker through a one-slot
   channel.

Marshaling happens before the lock so that `seq` order equals queue
order. Entries are stored already encoded, so the queue holds bytes, and
the envelope is the only thing `Write` adds around the record (functional
rule 2). Both capacity limits are checked against the entry being added.

**Worker.** One goroutine. It sleeps until a signal, the oldest entry's
age reaches `batch_max_delay`, or `Close`. It then takes entries from the
head **without removing them**: the first entry always, then each next one
only while adding it keeps the batch at or under `batch_max_records` and
`batch_max_bytes`. A single entry larger than `batch_max_bytes` therefore
goes alone (and may be refused 413 and dropped). It builds the body and
delivers:

- body: gzip of the entries' lines, in `seq` order;
- `POST` to `endpoint` as given, with headers

  (the set and meaning of these headers are the functional spec's
  *Delivery contract*; this table is the implementation's source)

  | Header | Value |
  | --- | --- |
  | `Authorization` | `Bearer <key>` |
  | `Content-Type` | `application/x-ndjson` |
  | `Content-Encoding` | `gzip` |
  | `X-Cardea-Instance` | the instance identity (32 hex chars from `crypto/rand`) |
  | `X-Cardea-First-Seq`, `X-Cardea-Last-Seq` | first and last `seq` in the batch |
  | `X-Cardea-Schema` | `cardea-gateway.trace/v1` (`trace.Schema`) |
  | `User-Agent` | `cardea-gateway` |

- the response is classified from its status code alone (the body is
  neither read nor kept; it is drained up to 4 KiB and closed so the
  connection is reused):

  | Status | Verdict | Effect |
  | --- | --- | --- |
  | 2xx | delivered | pop the batch |
  | 401, 403 | refused-credential | pop, count lost, rate-limited error log |
  | 400, 413, 422, 3xx, other 4xx except 408/429 | refused-batch | pop, count lost, error log with status |
  | 408, 429, 5xx, transport error, timeout | retry | keep the batch, back off |

Backoff: `1s · 2^n`, capped at 30 s, each wait multiplied by a random
factor in [0.5, 1]; if the response had `Retry-After` (delta-seconds or
HTTP-date) the wait is at least that, capped at 5 minutes. A successful
delivery resets `n`. The head batch is retried until it is delivered or
popped; nothing is reordered behind it.

**A panic while preparing or sending a batch** is recovered in the worker
loop: the batch in hand is popped, counted lost and logged at error level
(a deterministic panic would otherwise repeat on the same batch forever),
and the loop continues with the next one.

**Redirects are never followed.** The `http.Client` has `CheckRedirect`
returning `http.ErrUseLastResponse`, so a 3xx is a refused batch. The
bearer key must not follow a redirect to another host.

**Close.** Marks the sink closed, signals the worker, and gives it
`shutdown_timeout` to drain: the worker keeps delivering batches (no
`batch_max_delay` wait) until the queue is empty or the deadline passes,
then cancels the in-flight request through its context. While draining,
each backoff wait is **capped at 250 ms** (a drain does not sit out a
30 s backoff, and does not retry in a tight loop either). Entries left are
counted lost. `Close` first flushes any drop or credential-refusal count
the limiters were still holding, as error lines, so the counts across
error lines sum to the total lost. It then logs the summary at info level
with integer fields `delivered` and `lost`, and returns after the worker
exits: `nil` after a clean drain, an error stating the number of
undelivered records when entries were abandoned. It is idempotent: a second
call returns promptly and delivers nothing. `shutdown_timeout: 0` skips
draining and abandons everything held.

**MultiSink.** Writes every record to every sink, in order (Cardea first,
since its write is only an enqueue), and returns the errors joined
(`errors.Join`). One sink failing never skips another. The pipeline logs
the joined error and carries on. `Close` closes all and joins their
errors.

**Logging.** All logs go through the supplied `*zap.Logger`. The fields
are fixed: `host` (the URL's host and port, never the path or query),
`instance`, `status` (integer), `reason` (the fixed vocabulary below),
`count` (integer, the records lost in that line), `failures` (integer,
consecutive failed deliveries), and in the `Close` summary `delivered` and
`lost` (integers). Pipeline lines about a sink error use `call_id`,
`stage`, `record` and `error`. A rate-limited line may report a count
smaller than the total (the first drop logs `count` 1, the remainder at
the next interval or at `Close`); the counts across error lines add up to
the records lost. A panic is logged with the count of the batch dropped,
never the panic value. **Every lost record is reported at error level**
(functional rule 13). A small `limiter` (a mutex, a last-emitted time and
a pending count) collapses repeating events into at most one line per
interval carrying the count accumulated since the last: capacity drops
and credential refusals every 10 s, retry failures at warn level at the
first failure and then every 60 s. Startup logs one info line (host and
instance identity) from `NewCardeaSink`; `Close` logs the delivered and
lost totals. Nothing logged is, or wraps, a record, a header or the key.
Transport errors from `net/http` are `*url.Error`, which embeds the URL,
so the sink never logs `err.Error()` of a transport error:
`reason(err error) string` maps it, first match wins, to `timeout`
(`context.DeadlineExceeded`, or any `net.Error` whose `Timeout()` is
true), `canceled` (`context.Canceled`), `connection refused`
(`syscall.ECONNREFUSED`), `dns` (`*net.DNSError`), `tls` (certificate
verification, unknown authority, hostname or invalid certificate errors)
or, for anything else, `network error`. A 3xx (including one with no
`Location`) is a refused batch whose log line carries `status`; a
credential refusal's line says "credential".

## Packages and types

### `internal/trace` — modified

`sink.go` (modified): add the closeable interface the gateway now holds.

```go
// CloseableSink is a Sink that holds resources until closed.
type CloseableSink interface {
	Sink
	Close() error
}
```

`LogSink` already satisfies it. The comment on `Sink` changes: a non-nil
error means the record was **not** accepted and is lost, and the caller
must not refuse a call because of it.

`multi.go` (new):

```go
// MultiSink writes each record to every one of its sinks.
type MultiSink struct{ sinks []CloseableSink }

func NewMultiSink(sinks ...CloseableSink) *MultiSink

// Write offers the record to every sink, in order, and joins their errors.
func (m *MultiSink) Write(r Record) error
func (m *MultiSink) Close() error
```

`cardea.go` (new):

```go
// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("cardea sink is closed")

// CardeaSink delivers records to the Cardea platform.
type CardeaSink struct { /* cfg, key, instance, client, mu, queue, queueBytes, seq, closed, wake, done, limiters, counters */ }

// NewCardeaSink validates cfg, reads the API key from cfg.APIKeyEnv, logs
// the startup line, and starts the worker. It makes no network call. It
// fails when the key variable is unset, empty, or contains whitespace or
// control characters (header injection), naming the variable but never its
// value.
func NewCardeaSink(cfg config.CardeaSinkConfig, logger *zap.Logger) (*CardeaSink, error)

func (s *CardeaSink) Write(r Record) error
func (s *CardeaSink) Close() error

// InstanceID returns the identity sent with every batch.
func (s *CardeaSink) InstanceID() string
```

Unexported, in the same file or a sibling `cardea_delivery.go`:

```go
type verdict int

const (
	delivered verdict = iota
	retry
	refusedBatch
	refusedCredential
)

func classify(status int) verdict
func retryAfter(h http.Header, now time.Time) (time.Duration, bool)
func backoff(attempt int, floor time.Duration) time.Duration
func backoffFrom(base time.Duration, attempt int, floor time.Duration) time.Duration
func reason(err error) string
```

`backoff` is `backoffFrom` with a base of one second. `retryAfter` returns
`ok == false` for a missing header, a date in the past, a negative value
or anything unparsable, and otherwise the delay capped at 5 minutes.
`backoff(0, d)` for a `Retry-After: 3600` therefore waits exactly 5
minutes.

The test seams are unexported fields on `CardeaSink`, set by tests after
construction and before the first `Write`:

```go
sendBatch   func(ctx context.Context, b *batch) (status int, header http.Header, err error) // defaults to the real sender
backoffBase time.Duration                                                                     // default time.Second; scales the whole schedule
now         func() time.Time                                                                  // default time.Now
```

A retried batch keeps the same gzip body across attempts, so a retry is
byte-identical to the first try. `batch` is unexported: the entries taken
from the head of the queue (their lines, `seq` range, record count and
byte size).

### `internal/pipeline` — modified

`pipeline.go`: this is the 0003 amendment. A sink error no longer changes
the call.

- Step 3 (request record): on `p.sink.Write(rec)` error, log at error
  level (as today) and **do nothing else**: the block that built
  `Denial{Stage: "tracing", Status: 503, ...}` and set `stageErr` is
  removed. The call proceeds exactly as if the sink had accepted the
  record.
- Step 6 (response record): unchanged, an error is already only logged.
- The log line keeps its fields (`call_id`, `stage`, `record`, `error`)
  and its message changes to say the record is lost, not that it was
  refused.

Denials from the tracing *stage* (an unreadable `tool_result`, a panic in
`tracingStage.Request`) are untouched: they are not a sink failure.

Tests changed: `internal/pipeline/pipeline_test.go` and
`internal/gateway/tracing_test.go` cases that expect a 503 when the sink
errors now expect the call to proceed and the error to be logged.

### `internal/config` — modified

`config.go`: `TraceSinkConfig` gains a nested section; `validateTracing`
replaces its `logging`-only check (`config.go:286`).

```go
type TraceSinkConfig struct {
	// Type is "logging", "cardea" or "logging+cardea".
	Type   string           `mapstructure:"type"`
	Path   string           `mapstructure:"path"`
	Cardea CardeaSinkConfig `mapstructure:"cardea"`
}

// CardeaSinkConfig is read only when Type includes "cardea".
type CardeaSinkConfig struct {
	// Endpoint is the full URL batches are POSTed to. Required, no default.
	Endpoint string `mapstructure:"endpoint"`
	// APIKeyEnv names the env var holding the platform API key. Required,
	// no default. The key itself is never in config.
	APIKeyEnv       string        `mapstructure:"api_key_env"`
	QueueSize       int           `mapstructure:"queue_size"`
	QueueMaxBytes   int64         `mapstructure:"queue_max_bytes"`
	BatchMaxRecords int           `mapstructure:"batch_max_records"`
	BatchMaxBytes   int           `mapstructure:"batch_max_bytes"`
	BatchMaxDelay   time.Duration `mapstructure:"batch_max_delay"`
	RequestTimeout  time.Duration `mapstructure:"request_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// UsesCardea reports whether t.Type selects the Cardea sink.
func (t TraceSinkConfig) UsesCardea() bool
// UsesLogging reports whether t.Type selects the local sink.
func (t TraceSinkConfig) UsesLogging() bool
```

`validateTracing` (modified) accepts the three types and, when
`UsesCardea()`, checks that:

- `Endpoint` is non-empty, parses with `url.Parse`, has a host, no `User`
  and no fragment, and its scheme is `https`, or `http` with a loopback
  host (`localhost`, an address in 127.0.0.0/8, or `::1`);
- `APIKeyEnv` is non-empty;
- `BatchMaxRecords ≥ 1`, `1 ≤ BatchMaxBytes ≤ 8 MiB`;
- `QueueSize ≥ BatchMaxRecords` and `QueueMaxBytes ≥ BatchMaxBytes`;
- `BatchMaxDelay > 0`, `RequestTimeout > 0`, `ShutdownTimeout ≥ 0`.

The error text names the field and the rule, and never echoes the
endpoint's userinfo. Whether the key variable is *set and well formed* is
checked in `NewCardeaSink`, as the fingerprint key is checked in
`NewTracer`.

### `internal/gateway` — modified

`gateway.go`: the sink field and the sink return of `buildStages` become
`trace.CloseableSink`, built from the type. 0004 already widened
`buildStages` to return the policy engine; this keeps that and changes
only the second value.

```go
type Gateway struct {
	// ...
	sink trace.CloseableSink // nil when tracing is disabled
}

func buildStages(cfg config.Config, logger *zap.Logger) ([]pipeline.Stage, trace.CloseableSink, *policy.Engine, error)
```

Inside `buildStages`, where `NewLogSink` is called today, a helper builds
the sink so the function body keeps its shape:

```go
// buildSink returns the sink cfg.Tracing.Sink selects: the local log, the
// Cardea sink, or both in a MultiSink. On error nothing it opened stays
// open.
func buildSink(cfg config.TracingConfig, logger *zap.Logger) (trace.CloseableSink, error)
```

The existing error path for the policy engine
(`if sink != nil { _ = sink.Close() }`) stays as is and now closes
whichever sink was built (nothing has been written yet). `New` keeps its
guard so a nil sink is passed to the pipeline as a nil interface. `Run` is
unchanged: it already closes the sink after `server.Shutdown` returns,
which is after in-flight calls finish, so `Close` drains the last records.

### `configs/cardea-gateway.example.yaml` — modified

Documents the new `tracing.sink` keys (below), with the credential note.

## Configuration

```yaml
tracing:
  sink:
    type: logging # logging | cardea | logging+cardea
    path: ""
    cardea: # read only when type includes cardea
      endpoint: ""        # required: full URL; https, or http on localhost
      api_key_env: ""     # required: name of the env var holding the API key
      queue_size: 10000
      queue_max_bytes: 67108864 # 64 MiB
      batch_max_records: 500
      batch_max_bytes: 1048576
      batch_max_delay: 1s
      request_timeout: 10s
      shutdown_timeout: 10s
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `tracing.sink.type` | string | `logging` | `CARDEA_GATEWAY_TRACING_SINK_TYPE` |
| `tracing.sink.cardea.endpoint` | string | `""` (required with `cardea`) | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_ENDPOINT` |
| `tracing.sink.cardea.api_key_env` | string | `"CARDEA_API_KEY"` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_API_KEY_ENV` |
| `tracing.sink.cardea.queue_size` | int | `10000` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_QUEUE_SIZE` |
| `tracing.sink.cardea.queue_max_bytes` | int64 | `67108864` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_QUEUE_MAX_BYTES` |
| `tracing.sink.cardea.batch_max_records` | int | `500` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_BATCH_MAX_RECORDS` |
| `tracing.sink.cardea.batch_max_bytes` | int | `1048576` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_BATCH_MAX_BYTES` |
| `tracing.sink.cardea.batch_max_delay` | duration | `1s` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_BATCH_MAX_DELAY` |
| `tracing.sink.cardea.request_timeout` | duration | `10s` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_REQUEST_TIMEOUT` |
| `tracing.sink.cardea.shutdown_timeout` | duration | `10s` | `CARDEA_GATEWAY_TRACING_SINK_CARDEA_SHUTDOWN_TIMEOUT` |

Each field gets its default in both `defaults()` and `Load()`
(`SetDefault`, which `AutomaticEnv` needs to see nested keys), and an
entry in the example config. The key itself is read from the environment
variable the config *names*, exactly like `provider.api_key_env`; the
endpoint and the variable's name are the only two things an operator must
set to use the sink.

## Failure modes

**Every sink failure fails open.** A failure is an error log and a lost
record; no call is denied, delayed or altered because of it. This matches
the functional spec's *Failure behavior* and replaces 0003's refusal.

| Condition | Result |
| --- | --- |
| Default settings | No sink goroutine, no network, no key needed |
| `cardea` selected, endpoint missing/invalid/insecure/with userinfo | `config.Load` fails; `serve` exits before listening |
| `cardea` selected, `api_key_env` empty | `config.Load` fails |
| `cardea` selected, key env var unset/empty/has whitespace | `gateway.New` fails naming the variable; any opened log file is closed |
| Numeric/duration setting out of range, unknown sink type | `config.Load` fails |
| Platform unreachable at startup | Gateway starts; no connection until the first batch; deliveries retry with backoff |
| Queue full (records or bytes) | New record discarded, `seq` consumed, `Write` returns nil; error log, at most every 10 s, with the count |
| Record cannot be marshaled | `seq` consumed, counted lost, `Write` returns the error; the pipeline logs it and carries on |
| Any sink's `Write` returns an error | The pipeline logs it at error level with `call_id` and the record type; the call proceeds (0003 amended) |
| `MultiSink`, one sink fails | The other sinks are still written; the joined error is logged by the pipeline |
| Local sink cannot write (disk full) | Error log; the Cardea sink still receives the record when both are active |
| Transport error, timeout, 408, 429, 5xx | Batch kept at the head, backoff, warn at first failure and each minute, info on recovery |
| 401, 403 | Batch dropped and counted, error log at most every 10 s; later batches still attempted |
| 400, 413, 422, 3xx, other 4xx | Batch dropped and counted, error log with status |
| Panic preparing or sending a batch | Recovered; that batch dropped and counted, error log; the worker continues with the next |
| `Write` after `Close` | `ErrClosed`; the pipeline logs it |
| Shutdown with records held | Drained for up to `shutdown_timeout`; the remainder counted lost, error log; totals at info |
| Process exits abruptly | Held records lost; `seq` gap visible on the platform |

Nothing logged contains prompt content, tool arguments or results, the key,
an `Authorization` header or the endpoint's path or query. Logged
transport failures are reduced to a fixed vocabulary (`reason`), because
`*url.Error` and some TLS errors quote the URL.

## Testing

`internal/trace/cardea_test.go`, against an `httptest.Server` fake
platform that records the requests it receives and answers with a scripted
status sequence:

- **Delivery:** both record types arrive, as lines `{"seq","record"}`;
  the record bytes equal `json.Marshal` of the same value; gzip body;
  `Authorization`, `X-Cardea-Instance`, `First-Seq`/`Last-Seq`, `Schema`
  headers; a body ordered by `seq` with no gaps.
- **Batching:** by record count, by bytes, by delay; a table over
  thresholds. With `batch_max_bytes` 1 500 and 600-byte entries no request
  has more than two; one 4 500-byte entry is sent alone (criterion 12).
- **Classification table:** every status in the contract → verdict;
  `Retry-After` in seconds and as an HTTP-date; `Retry-After: 3600` waits
  5 minutes, not an hour (criterion 6).
- **Retry:** 503 then 202 delivers once with no missing `seq`; 429 with
  `Retry-After` is not retried earlier (fake clock via the `now` seam); the
  head is never overtaken; the backoff factor stays within [0.5, 1] of the
  schedule.
- **Refusals:** 401 and 400 drop the batch, the next batch is sent, the
  counters are right, and the **log output is captured with
  `zaptest/observer`**: the drop is an error-level line; no entry contains
  the key or the endpoint path.
- **No content in logs (criterion 25):** records carrying sentinel strings
  in their prompt-adjacent fields (model, tool names, reason) and in a
  field value, driven through every failure path (marshal error, 400
  refusal, 401, capacity drop, shutdown loss, panic), never appear in any
  log entry; only counts, statuses, the host and fixed reasons do.
- **Capacity:** capacity 2 records, platform blocked → third to fifth
  discarded, an error-level log with the count, `seq` gap of exactly 3 on
  recovery; the same with a byte capacity smaller than three entries
  (criteria 10, 11); `Write` always returns nil for a drop.
- **Marshal failure:** a record that cannot be encoded consumes a `seq`,
  returns the error, and leaves a gap.
- **Non-blocking:** with a platform that never answers, 1 000 `Write`s
  finish within a small bound; run under `-race`.
- **Redirect:** a 307 to a second `httptest.Server` is not followed and the
  second server never sees the `Authorization` header.
- **Panic recovery:** a panic injected through `sendBatch` drops that batch,
  logs an error, counts it lost, and the next batch is delivered
  (criterion 13).
- **Close:** drains within the timeout against a slow-but-working
  platform; abandons at the timeout against a dead one and reports the lost
  count; `Write` after `Close` → `ErrClosed`; `Close` is idempotent
  (criterion 23).
- **Instance identity:** two sinks have different identities; each starts at
  `seq` 1; `InstanceID` equals the value in the startup log line
  (criteria 18, 24).
- **Construction:** `NewCardeaSink` makes no network call (a fake that
  fails the test on any connection), and logs exactly one startup line with
  the host and instance identity, and neither key nor path (criteria 22,
  24); a key with a space, a newline, or empty fails naming the variable
  but not the value (criterion 21).

`internal/trace/multi_test.go` (stub sinks): every sink written in order;
an error from one does not stop the rest; errors joined; `Close` closes
all and joins errors.

`internal/pipeline/pipeline_test.go` (amended): a sink that errors on the
request record does not deny an allowed call — the provider is called, the
response is relayed, and an error-level line names `call_id` and the
record type (criteria 14, 15); a sink that errors on a response record
leaves the response intact (unchanged); a call already refused by a stage
still carries that refusal; a `tracingStage` error (unreadable
`tool_result`) **still** denies the call with 503 (unchanged).

`internal/config/config_test.go`: defaults; env override of each field;
table of invalid values (`http://localhost` and `http://127.0.0.1:9`
accepted, `http://example.com` rejected, userinfo, fragment, missing host,
empty endpoint, empty `api_key_env`, ranges, `QueueMaxBytes <
BatchMaxBytes`, unknown type); `cardea` fields ignored (and unvalidated)
when type is `logging` (criteria 19, 20).

`internal/gateway/gateway_test.go`: `cardea` without the key env var fails
`New` and leaves no open file; `logging+cardea` writes the local file and
reaches a fake platform (criteria 4, 17 with the platform down); a local
file that cannot be written does not deny a call, and with `logging+cardea`
the fake platform still receives both records (criteria 15, 16); `Run`'s
shutdown delivers held records; default config builds no `CardeaSink`
(criterion 1); a Gateway with `cardea` selected and an unreachable
platform starts and serves (criterion 22); a policy-engine build failure
closes the sink that was already built.

A tenant check (criterion 26): decode every line the fake platform
receives and assert no key or header name contains `tenant`, and that
`TraceSinkConfig`/`CardeaSinkConfig` have no such field (reflection over
the struct's `mapstructure` tags).

An end-to-end check through the pipeline: an allowed call against a fake
provider and fake platform produces a request and a response record there,
and the agent's response is unaffected when the platform is down or its
queue is full (criteria 2, 3, 14).

## Alternatives considered

- **Fail closed when the sink refuses (0003's behavior), kept as `reject`
  overflow** — rejected: it makes the platform a hard dependency of every
  call, and with two sinks it records a call in one that the other's
  failure then denies, which no write order can fix without a way to recall
  a queued record. Removing it also removes `ErrQueueFull`, the gate logic
  in `MultiSink` and the case where `reject` left a platform gap. A strict
  mode could return later as an explicit spec.
- **A gate sink in `MultiSink` (write Cardea first, skip the local write on
  refusal)** — rejected: needed only while a sink could refuse a call.
- **gRPC** — rejected: this is one-way batched delivery with an ack, so
  streaming and RPC latency buy nothing. gRPC needs HTTP/2 end to end, which
  proxies and some serverless platforms break; it would add a second
  (protobuf) schema beside the JSON one of 0003; and it is harder to stand
  in for locally. Can be added beside REST later if volume warrants.
- **POST inside `Write`** — rejected: puts the platform's latency in every
  call's path.
- **Vector, Fluent Bit or another shipper tailing the local file** —
  rejected for this feature: the operator must run and secure another
  component, and there is nowhere for the Gateway to put the delivery
  numbering. Still possible: the `logging` sink is unchanged.
- **OTLP/OpenTelemetry export** — rejected: records are not spans, and
  forcing them into OTel's model couples the format to it. Revisit as its
  own sink.
- **`seq` inside the record** — rejected: the record format is fixed by
  0003 and shared with the local sink; the number is delivery metadata, so
  it lives in the envelope.
- **Tenant in Gateway config or in records** — rejected by decision: the
  credential identifies the organization, so no setting can misattribute
  data and the Gateway needs no notion of tenants.
- **API key literal in config** — rejected: keys get committed. The config
  names an environment variable, as `provider.api_key_env` does.
- **Per-record hash chain in the Gateway** — deferred: needs ordered,
  stateful writes, persistence across restarts and multi-instance
  coordination. `seq` plus instance identity detects loss now and can be
  extended to a chain without changing the envelope's shape.
- **Disk spool** — deferred: file management and replay for a case the
  local `logging` sink already covers.
- **Partial acceptance (per-record statuses)** — rejected: complicates both
  sides; whole-batch acceptance plus idempotence is enough.
- **Separate `type: cardea` only** (no `logging+cardea`) — rejected:
  operators wanting both would need a second config mechanism; the combined
  type costs one `MultiSink`.

## Open questions

- ~~`X-Cardea-*` header names.~~ Confirmed.
- Whether the use of the whole endpoint URL (rather than a base URL plus a
  fixed path) suits the platform's intake. — platform side.
- Whether to send the Gateway's version: `internal/cli` holds it in an
  unexported variable and `gateway.New` does not receive it, so passing it
  needs a signature change. Left out for now. — you.
- Counters for scraping once the Gateway has a metrics surface. — you.

# 0003 — Call tracing (technical)

**Date:** 2026-09-27
**Functional spec:** specs/0003-call-tracing-functional.md

## Context

The functional spec asks for two JSON records per call: a `request`
record handed to a trace sink before the request is forwarded, and a
`response` record handed over once the response has ended and every
stage has observed it. Together they carry every stage's decision and
timing, every new tool result in the request, and every tool call in the
response, whatever the response's size.

Today's pipeline (0001, 0002) cannot produce either record:

- Stage decisions are not kept. `Handler` logs a denial and moves on, and
  no stage is timed.
- A `Stage` sees only the stages before it. Tracing sits second in the
  fixed order, after identification, so it cannot report what
  `token_counting` decided.
- Nothing runs between "every stage allowed" and "forward". A request
  record written by a stage would miss later stages; one written by an
  observer arrives after the provider already has the request.
- The response is visible only as `Call.ResponseBody`, capped at
  `max_body_bytes` and read after the response ended. That loses tool
  calls past the cap (rule 9) and any time at which each tool call was
  relayed (`relayed_at`).
- A provider that never answers is indistinguishable from a provider
  that answered 502: `httputil.ReverseProxy`'s default error handler
  writes a 502 on the same `ResponseWriter`.

This spec makes the pipeline own record emission, adds a tap on the
response stream, and puts everything that knows the record format, the
Anthropic wire format, sessions, fingerprints and the sink in a new
`internal/trace` package.

## Goals

- Every call gets a request record, and every forwarded call a response
  record, emitted from one place in `pipeline.Handler`, so no path can
  skip either.
- The request record reaches the sink before `next.ServeHTTP` runs; a
  sink failure denies the call (rule 5, *Failure behavior*).
- Tool calls are read from the relayed stream as it passes, with no size
  cap and no change to what the agent receives (rules 3, 9).
- No state survives a call (rule 1): every per-call value lives on the
  `Call` or on objects created for that call.
- Default behavior for requests and responses is unchanged except that
  records appear on stdout.

## Non-goals

- Everything the functional spec excludes: tool-call decisions, network
  and OpenTelemetry sinks, sampling, collecting or assembling traces.
- Provider formats other than the Anthropic Messages API. With any other
  provider, records are still written, but the fields read from bodies
  (`model`, `tool_results`, `tool_calls`, `response_id`, …) take their
  not-applicable values. The next provider's spec adds its parser.
- Content encodings other than identity and gzip on responses.
- A running token total. The `token_counting` stage (0001) was removed
  after this spec: its only output was a log line, and each response
  record's `tokens` now carries the call's usage.

## Design

### Where it sits

```
cli.serve → config.Config → zap.Logger → provider.Provider → gateway.Gateway
                                                              │
      gateway.New builds: trace.Tracer, trace.Sink, stages ───┘
      pipeline.New(logger, maxBody, sink, identification, tracing)
```

Tracing inspects both requests and responses and changes neither.

`internal/trace` owns the record schema, the Anthropic parsing, session
reading, fingerprinting and the logging sink. It knows nothing about the
pipeline. `internal/pipeline` imports it to build and emit records.
`internal/gateway` wraps a `*trace.Tracer` in a thin `tracingStage`, the
same shape as `identityStage`.

### The call, step by step

`pipeline.Handler`, with the new steps in bold:

1. Build the `Call` and read the body (unchanged).
2. Run stages in order. **Time each one** (`started`, `duration`) and
   keep its result: `allowed`, `denied` or `failed`. **Stages after a
   denial or failure are kept as `not_run`**. `tracingStage` returns
   `AllowTraced(info, tap)`; the pipeline copies `info` onto
   `Call.Trace` and keeps `tap` for step 4, the same way it copies
   `AllowAs`'s agent ID.
3. **If a sink is set, build the request record and hand it to the
   sink.** The record's `duration_us` runs from `Received` to this moment.
   If the sink returns an error and the call was allowed, the call is now
   failed: `Denial{Stage: "tracing", Status: 503, Message: "governance
   unavailable", Reason: "trace sink: <err>"}`, logged at error level. If
   the call was already refused, the refusal stands and the sink error is
   only logged.
4. Dispatch. If allowed, stamp `forwardedAt`, then run `next` with the
   `bodyRecorder`, which **also feeds the tap**. Otherwise write the
   denial (unchanged).
5. Populate outcome fields (unchanged), then run observers in reverse
   order. **Each observer is timed and its result kept: `completed`, or
   `failed` if it panicked.**
6. **If a sink is set and the call was forwarded, build the response
   record and hand it to the sink.** A sink error is logged at error
   level with `call_id`; nothing else happens (*Failure behavior*).

Records are built only when the pipeline has a sink. `gateway.New` passes
a sink exactly when `pipeline.stages.tracing` is true, so disabling
tracing produces no records at all.

A call denied before tracing runs (by the body check or identification)
still gets a request record. Tracing's own fields then take their
not-applicable values: `session.status: "absent"`, `model: null`,
`tool_results: []`.

### Reading the request

`Tracer.Request(r, body, agentID)` returns a `RequestInfo`:

- **Session.** The source list for `agentID` is chosen: the list naming
  that agent, else the default, else none. `agentID` is `""` when
  identification is disabled, so only the default applies. Sources are
  tried in order:
  - `header`: `r.Header.Get(name)`. An empty value counts as absent.
  - `body_field`: a dot-separated path of object keys into the JSON body
    (`metadata.user_id`); no array indexes. A missing path or a non-JSON
    body counts as absent. A present value that is not a JSON string
    makes the session `invalid`.
  - `baggage`: the W3C `baggage` header, parsed as comma-separated
    `key=value` members with `;` properties dropped and the value
    percent-decoded. The first member with the key wins.

  A source's `pattern`, compiled at startup, applies to the raw value.
  No match counts as absent. On a match, capture group 1 is the value.
  The first source that yields a value decides: `claimed` if it passes
  rule 17 (1–128 bytes, each `0x21`–`0x7E`), `invalid` otherwise. The
  value of an invalid session is dropped at once and never logged.
  `source` and `source_name` name that source either way.
- **Request metadata.** The body is decoded, if it is a JSON object, into
  `{model, stream, messages []json.RawMessage}`. `model` is `null` if
  absent; `stream` is `false` if absent.
- **New tool results.** Only the last element of `messages` whose `role`
  is not `system` is decoded; trailing `system` messages are skipped,
  since Claude Code appends one after the tool results. If its `role` is
  `user` and its `content` is an array, every block with
  `type: "tool_result"` becomes a `ToolResult`:
  - `tool_call_id` comes from `tool_use_id`.
  - `is_error` defaults to `false`.
  - `size_bytes` and `fingerprint` cover the canonical form of the
    block's `content` value. When `content` is absent, `size_bytes` is 0
    and `fingerprint` is `null`.

  A `tool_result` block whose `tool_use_id` is not a non-empty string, or
  whose `content` is not valid JSON, is a read failure: `Request` returns
  an error, and the pipeline fails the call closed with stage `tracing`.
  A body that is not JSON, or has no `messages`, is not an error: it has
  no tool results, and the provider judges it.

### Reading the response: the tap

`bodyRecorder` gains an optional `*trace.ResponseTap`:

- In `WriteHeader`, it calls `tap.Start(status, header)` and stamps the
  first-byte time.
- In `Write`, it relays `b` first, then calls `tap.Write(b[:n],
  relayedAt)` with the bytes that were actually relayed.
- If the real writer returns an error, the agent has gone:
  `tap.Disconnected()`.
- Every tap call is wrapped in `recover`, as a last line of defense (see
  *Crash protection* below). The relay carries on untouched either way.
- Time spent inside tap calls is summed. The first call's start and that
  sum become tracing's `started_at` and `duration_us` in the response
  record's `stages[]`: tracing's real cost on the relay path.

`Start` picks a mode from the response headers:

| `Content-Type` | Mode |
| --- | --- |
| `text/event-stream` | **SSE**: parse events incrementally as bytes arrive |
| `application/json` | **JSON**: buffer the body; parse it in `Result` |
| anything else | **none**: nothing is parsed |

With `Content-Encoding: gzip`, SSE mode pipes bytes through a
`gzip.Reader` in one goroutine per call, and JSON mode decompresses in
`Result`. Any other encoding leaves the tap unable to read: its result
is `Failed` with no tool calls, and an error is logged.

In SSE mode the tap splits lines, collects `data:` payloads until the
blank line that ends an event, and decodes each event:

| Event | What the tap takes |
| --- | --- |
| `message_start` | `message.id`, `message.model`, input usage |
| `content_block_start`, block `tool_use` or `server_tool_use` | opens a tool call at `index`: `id`, `name`, `server_side` |
| `content_block_delta`, delta `input_json_delta` | appends `partial_json` to that tool call's buffer |
| `content_block_stop` for an open tool call | closes it: canonicalize the buffer, then set `argument_names`, `size_bytes`, `fingerprint`, and `relayed_at` = the `relayedAt` of the write carrying this event |
| `message_delta` | `delta.stop_reason`, `usage.output_tokens` |
| anything else | ignored |

In JSON mode, `Result` decodes the buffered body as a Messages response
(`id`, `model`, `stop_reason`, `usage`, `content[]`). Every `tool_use` or
`server_tool_use` block becomes a tool call, and `relayed_at` is the time
of the last relayed write.

#### Read limit

Everything the tap buffers is capped by `tracing.max_read_bytes`
(default 16 MiB; see *Configuration*). The cap is separate from
`pipeline.max_body_bytes`, which rule 9 says does not apply to tool
calls. It sits well above anything one response can carry, since the
request's `max_tokens` bounds output. The cap applies to:

- **One streamed tool call's arguments.** Past the cap, buffering stops
  for that tool call. It closes as usual (`delivered` and `relayed_at`
  are still set) with `recorded: false` and `null` `argument_names`,
  `size_bytes` and `fingerprint`.
- **One SSE event** (the `data:` payload between blank lines). An event
  past the cap is skipped. If it belonged to an open tool call, that tool
  call ends up with `recorded: false`.
- **A non-streamed body**, compressed or decompressed. Past the cap,
  buffering stops, `Result` parses nothing, and the record has
  `tool_calls: []`, `null` `response_id`, `response_model`, `stop_reason`
  and `tokens`, and the finding `{"kind": "tool_calls_unread"}`.

Hitting the cap is a limit, not a failure. Tracing's observation stays
`completed`, nothing is logged at error level, and the record alone says
what was not read. Only a real error sets `Failed`: arguments that are
not valid JSON, an unreadable encoding, or a panic.

#### Crash protection

Parsing runs on bytes the provider controls, so a bug there must never
reach the relay or the process. Two `recover`s cover it:

- **Per event.** The tap handles each SSE event (and, in JSON mode, the
  whole body) inside a `recover`. A panic while handling an event marks
  the tap `Failed`. If the event carried a tool call's `index`, that tool
  call gets `recorded: false`. Parsing then goes on with the next event,
  so later tool calls and `message_delta`'s `stop_reason` are still
  recorded (acceptance criterion 26).
- **gzip goroutine.** The goroutine's body starts with a deferred
  `recover`. On a panic it marks the tap `Failed`, then drains the pipe
  with `io.Copy(io.Discard, pr)`, so the relaying goroutine's writes
  never block and the process never crashes. `Result` closes the pipe's
  writer and waits for the goroutine to finish before reading state.
  In this mode, tap state is guarded by a mutex, since `Disconnected` and
  `Result` run on the relaying goroutine.

The recorder's own `recover` around each tap call catches anything
outside event handling, such as line splitting. After such a panic the
tap is not called again. Every tool call still open gets `recorded:
false`, and events after the panic are not read.

In gzip SSE mode, `relayed_at` is the time the parser goroutine decodes
`content_block_stop`. The pipe hands bytes over synchronously, so that
time falls within the relay of the compressed bytes carrying the event,
up to the `gzip.Reader`'s internal buffering.

#### Result

`Result()` is called once, by the pipeline, after `next` returns:

- A tool call still open when the relay ended is `not_delivered`,
  with `null` argument fields and `relayed_at`. That covers both an
  agent that disconnected and a provider stream that ended early.
- A tool call opened after `Disconnected` is `not_delivered` too.
- A tool call closed before `Disconnected` is `delivered`.

`stop_reason`, `response_id` and `response_model` stay `null` whenever
their event never came, which covers early-ended streams (functional
spec, *`response` record*).

### Canonical JSON and fingerprints

Tool-call arguments and tool-result content are fingerprinted over their
canonical form:

- Decode with `json.Decoder.UseNumber()`, so number text is preserved.
- Re-encode with `json.Encoder`, `SetEscapeHTML(false)`, and the trailing
  newline trimmed. Go sorts map keys and emits no whitespace, so a
  streamed and a non-streamed tool call with equal arguments produce the
  same bytes.

`size_bytes` is the length of the canonical form, so it agrees across
modes too. `argument_names` are the top-level keys of the canonical
object, sorted; they are `[]` when the arguments are not an object.

`fingerprint` is `"fp_"` followed by the first 16 hex characters of
HMAC-SHA256 over the canonical bytes, keyed by the deployment's key (see
*Configuration*). When no key is configured, `NewTracer` generates a
random 32-byte key with `crypto/rand` and logs a warning. Fingerprints
then match only within that process: a restart or a second instance
fingerprints the same content differently.

### Detecting an unanswered provider

`internal/provider` gains one `ErrorHandler`, set on both reverse
proxies. It writes a bare 502 (as the default does) and, first, calls
`UpstreamFailed()` on the `ResponseWriter` if it implements it:

```go
if f, ok := w.(interface{ UpstreamFailed() }); ok {
	f.UpstreamFailed()
}
```

`bodyRecorder` implements `UpstreamFailed()` by setting a flag, so the
response record's `outcome.result` is `unanswered` with `upstream_status:
null` and `status: 502`. `ReverseProxy` also calls the handler when the
agent disconnects before the provider answers; that call is recorded as
`unanswered` too. The interface check keeps `internal/provider` from
importing `internal/pipeline`.

### Emitting records

`trace.Sink` is one method, `Write(Record) error`. It must be safe for
concurrent use, and a `nil` error means the record is accepted (rule
23). Records are passed as values, never pre-encoded, so a later sink
encodes them as it likes while the fields stay fixed (rule 22).

`LogSink` marshals the record with `encoding/json`, appends `\n`, and
issues a single `Write` of that line under a mutex, to stdout or to a
file opened at startup with `O_WRONLY|O_APPEND|O_CREATE`, mode `0o640`.
Records are never pretty-printed. There is no `fsync`: "accepted" means
the write returned. zap already writes application logs to stderr (both
the development and production configs in `internal/logging`), so
stdout carries only records (acceptance criterion 15).

## Packages and types

### `internal/trace` — new

Package doc: `// Package trace builds agentkeeper's call-trace records
from the requests and responses it relays, and hands them to a sink.`

`record.go`: the v1 schema. Every field is always emitted; pointers and
`*Timestamp` encode `null`, and slices are initialized so they encode
`[]`.

```go
const Schema = "agentkeeper.trace/v1"

// Record is a RequestRecord or a ResponseRecord.
type Record interface{ recordType() string }

// Timestamp marshals as RFC 3339 UTC with exactly six fractional digits.
type Timestamp time.Time

func (t Timestamp) MarshalJSON() ([]byte, error)

type RequestRecord struct {
	Schema      string         `json:"schema"`
	Record      string         `json:"record"` // "request"
	CallID      string         `json:"call_id"`
	AgentID     *string        `json:"agent_id"`
	Session     Session        `json:"session"`
	ReceivedAt  Timestamp      `json:"received_at"`
	DurationUS  int64          `json:"duration_us"`
	Model       *string        `json:"model"`
	Stream      bool           `json:"stream"`
	SizeBytes   *int64         `json:"size_bytes"` // nil when the body was not read in full (400, 413)
	Outcome     RequestOutcome `json:"outcome"`
	Stages      []RequestStage `json:"stages"`
	ToolResults []ToolResult   `json:"tool_results"`
	Findings    []Finding      `json:"findings"`
}

type ResponseRecord struct {
	Schema        string          `json:"schema"`
	Record        string          `json:"record"` // "response"
	CallID        string          `json:"call_id"`
	AgentID       *string         `json:"agent_id"`
	Session       Session         `json:"session"`
	StartedAt     Timestamp       `json:"started_at"`
	FirstByteUS   *int64          `json:"first_byte_us"`
	DurationUS    int64           `json:"duration_us"`
	Outcome       ResponseOutcome `json:"outcome"`
	Stages        []ResponseStage `json:"stages"`
	Streamed      bool            `json:"streamed"`
	ResponseID    *string         `json:"response_id"`
	ResponseModel *string         `json:"response_model"`
	StopReason    *string         `json:"stop_reason"`
	Tokens        *Tokens         `json:"tokens"`
	ToolCalls     []ToolCall      `json:"tool_calls"`
	Findings      []Finding       `json:"findings"`
}

// Session.Status is one of "claimed", "invalid", "absent"; Source one of
// "header", "body_field", "baggage", or nil when absent.
type Session struct {
	ID         *string `json:"id"`
	Status     string  `json:"status"`
	Source     *string `json:"source"`
	SourceName *string `json:"source_name"`
}

// Result is one of "forwarded", "denied", "failed".
type RequestOutcome struct {
	Result string  `json:"result"`
	Status *int    `json:"status"`
	Stage  *string `json:"stage"`
	Reason *string `json:"reason"`
}

// Result is one of "delivered", "unanswered".
type ResponseOutcome struct {
	Result         string `json:"result"`
	UpstreamStatus *int   `json:"upstream_status"`
	Status         int    `json:"status"`
}

// Decision is one of "allowed", "denied", "failed", "not_run"; timings
// are nil for "not_run".
type RequestStage struct {
	Name       string     `json:"name"`
	StartedAt  *Timestamp `json:"started_at"`
	DurationUS *int64     `json:"duration_us"`
	Decision   string     `json:"decision"`
}

// Observation is one of "completed", "failed".
type ResponseStage struct {
	Name        string    `json:"name"`
	StartedAt   Timestamp `json:"started_at"`
	DurationUS  int64     `json:"duration_us"`
	Observation string    `json:"observation"`
}

type ToolResult struct {
	ToolCallID  string  `json:"tool_call_id"`
	IsError     bool    `json:"is_error"`
	SizeBytes   int64   `json:"size_bytes"`
	Fingerprint *string `json:"fingerprint"`
}

// Outcome is one of "delivered", "not_delivered".
type ToolCall struct {
	ToolCallID    string     `json:"tool_call_id"`
	ToolName      string     `json:"tool_name"`
	ServerSide    bool       `json:"server_side"`
	ArgumentNames []string   `json:"argument_names"` // nil encodes null: not recorded
	SizeBytes     *int64     `json:"size_bytes"`
	Fingerprint   *string    `json:"fingerprint"`
	RelayedAt     *Timestamp `json:"relayed_at"`
	Outcome       string     `json:"outcome"`
	Recorded      bool       `json:"recorded"`
}

type Tokens struct {
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
}

// Kind is "response_truncated" or "tool_calls_unread" in v1.
type Finding struct {
	Kind string `json:"kind"`
}
```

`tracer.go`: the per-deployment object, built once at startup.

```go
type Tracer struct { /* fingerprint key, compiled session lists, maxReadBytes */ }

// NewTracer reads the fingerprint key from the env var cfg names. If the
// var is unset or empty, it generates a random 32-byte key and logs a
// warning naming the var; if it is set but shorter than 32 bytes, it
// fails. It compiles every session pattern, rejecting one that does not
// compile or has a capture-group count other than one. Its errors name
// the offending source by list and position.
func NewTracer(cfg config.TracingConfig, logger *zap.Logger) (*Tracer, error)

// RequestInfo is what tracing contributes to a request record.
type RequestInfo struct {
	Session     Session
	Model       *string
	Stream      bool
	ToolResults []ToolResult
}

// Request never returns an error for a missing or invalid session, only
// for tool_result blocks it cannot read.
func (t *Tracer) Request(r *http.Request, body []byte, agentID string) (RequestInfo, error)

func (t *Tracer) NewResponseTap() *ResponseTap
```

`tap.go`:

```go
// ResponseTap reads one call's response as it is relayed. Not safe for
// concurrent use; the pipeline calls it from the relaying goroutine only.
type ResponseTap struct { /* mode, SSE line state, open tool calls, gzip pipe and mutex, maxReadBytes */ }

func (t *ResponseTap) Start(status int, h http.Header)
func (t *ResponseTap) Write(p []byte, relayedAt time.Time)
func (t *ResponseTap) Disconnected()

// ResponseInfo is what tracing contributes to a response record.
type ResponseInfo struct {
	Streamed      bool
	ResponseID    *string
	ResponseModel *string
	StopReason    *string
	Tokens        *Tokens
	ToolCalls     []ToolCall
	Failed        bool // tracing's observation is "failed"; hitting the read limit does not set it
	Unread        bool // non-streamed body past the read limit: finding "tool_calls_unread"
}

// Result closes the tap and must be called exactly once, after the relay
// has ended.
func (t *ResponseTap) Result() ResponseInfo
```

`session.go` holds session-source matching and baggage parsing.
`anthropic.go` holds the Messages request and response shapes.
`canonical.go` holds `canonicalJSON(raw []byte) ([]byte, error)` and the
HMAC. None of these export anything beyond the above.

`sink.go`:

```go
type Sink interface {
	Write(r Record) error
}

type LogSink struct { /* mu, w io.Writer, f *os.File */ }

// NewLogSink writes to stdout when path is "", else opens path for
// appending, creating it if needed.
func NewLogSink(path string) (*LogSink, error)

func (s *LogSink) Write(r Record) error

// Close closes the file, if any. It never closes stdout.
func (s *LogSink) Close() error
```

### `internal/pipeline` — modified

`pipeline.go`:

```go
type Call struct {
	// ...existing fields...

	// Trace is set by the pipeline from the tracing stage's Decision
	// (AllowTraced). Nil when tracing is disabled or did not run.
	Trace *trace.RequestInfo
}

type Decision struct {
	// ...existing fields...
	trace *trace.RequestInfo
	tap   *trace.ResponseTap
}

// AllowTraced allows the call and attaches what tracing read from the
// request, plus the tap that will read its response.
func AllowTraced(info trace.RequestInfo, tap *trace.ResponseTap) Decision

// New's sink is nil when tracing is disabled; the pipeline then builds
// no records.
func New(logger *zap.Logger, maxBodyBytes int64, sink trace.Sink, stages ...Stage) *Pipeline
```

- `observe` returns whether the observer completed, so a panic becomes
  `observation: "failed"`.
- `Handler` gains the bold steps from *Design*.
- New file `records.go`: `buildRequestRecord` and `buildResponseRecord`,
  which turn the `Call`, the kept stage results and the tap's
  `ResponseInfo` into `trace` records. This is the only place pipeline
  facts meet the schema.
- Outcome mapping for the request record:
  - `forwarded`: no denial.
  - `failed`: a stage error or panic, or a request record the sink
    refused (in the latter case the record never reaches the sink, but
    the mapping is the same).
  - `denied`: every other denial, including the pipeline's own 400 and
    413.
- Response record `stages[]` lists the tracing entry (the stage whose
  `Decision` carried the tap) first, then every observer in the order it
  ran. `findings` gets `{"kind": "response_truncated"}` when
  `Call.ResponseTruncated`, and `{"kind": "tool_calls_unread"}` when the
  tap's `Unread` is set.

`recorder.go`: `bodyRecorder` gains `tap *trace.ResponseTap`,
`firstByte time.Time`, `upstreamFailed bool`, `disconnected bool`,
`tapStarted time.Time`, `tapTime time.Duration` and `tapFailed bool`, as
described under *Reading the response*, plus:

```go
func (r *bodyRecorder) UpstreamFailed()
```

`newBodyRecorder(w, maxBody, tap)` takes the tap, which may be nil.

### `internal/gateway` — modified

`tracing.go` (new):

```go
type tracingStage struct {
	tracer *trace.Tracer
}

func (s *tracingStage) Name() string { return "tracing" }

func (s *tracingStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	info, err := s.tracer.Request(c.Request, c.Body, c.AgentID)
	if err != nil {
		return pipeline.Decision{}, err
	}
	return pipeline.AllowTraced(info, s.tracer.NewResponseTap()), nil
}
```

`gateway.go`:

- `buildStages` inserts tracing after identification, and also returns
  the sink:

  ```go
  func buildStages(cfg config.Config, logger *zap.Logger) ([]pipeline.Stage, *trace.LogSink, error)
  ```

  With tracing disabled, the sink is nil and is passed to `pipeline.New`
  as a nil `trace.Sink` (not a typed nil).
- `Gateway` keeps the `*trace.LogSink` and closes it when `Run` returns.

### `internal/provider` — modified

`provider.go` gains the unexported `errorHandler(w http.ResponseWriter, r
*http.Request, err error)` described under *Detecting an unanswered
provider*. `anthropic.go` and `httpproxy.go` set it as `ErrorHandler` on
their `ReverseProxy`.

### `internal/config` — modified

```go
type Config struct {
	// ...existing fields...
	Tracing TracingConfig `mapstructure:"tracing"`
}

type StagesConfig struct {
	Identification bool `mapstructure:"identification"`
	Tracing        bool `mapstructure:"tracing"` // new, defaults true
}

type TracingConfig struct {
	// FingerprintKeyEnv names the env var holding the HMAC key for
	// fingerprints (at least 32 bytes). The key itself is never in config.
	// Unset var: a random per-process key is generated, with a warning.
	FingerprintKeyEnv string              `mapstructure:"fingerprint_key_env"`
	// MaxReadBytes caps what the response tap buffers: one streamed tool
	// call's arguments, one SSE event, or a whole non-streamed body.
	MaxReadBytes      int64               `mapstructure:"max_read_bytes"`
	Sink              TraceSinkConfig     `mapstructure:"sink"`
	Sessions          []SessionSourceList `mapstructure:"sessions"`
}

type TraceSinkConfig struct {
	// Type is "logging", the only sink in this version.
	Type string `mapstructure:"type"`
	// Path is where the logging sink appends records; "" means stdout.
	Path string `mapstructure:"path"`
}

// SessionSourceList applies to the named Agents, or to every agent
// without a list of its own when Default is true. Exactly one of the
// two is set.
type SessionSourceList struct {
	Agents  []string        `mapstructure:"agents"`
	Default bool            `mapstructure:"default"`
	Sources []SessionSource `mapstructure:"sources"`
}

// SessionSource sets exactly one of Header, BodyField, Baggage. Pattern
// is an optional RE2 regexp with exactly one capture group.
type SessionSource struct {
	Header    string `mapstructure:"header"`
	BodyField string `mapstructure:"body_field"`
	Baggage   string `mapstructure:"baggage"`
	Pattern   string `mapstructure:"pattern"`
}
```

`Load` adds shape checks, alongside the existing agent checks:

- `tracing.sink.type` must be `logging`.
- `tracing.max_read_bytes` must be positive.
- Every sessions entry sets exactly one of a non-empty `agents` or
  `default: true`, and at most one entry is the default.
- No agent appears in two entries, and every named agent is in `agents[]`.
- Every entry has at least one source, and every source sets exactly one
  of `header`, `body_field`, `baggage`.

`tracing.sessions` is decoded with `ErrorUnused`, like `agents`. Pattern
compilation, the key lookup and opening the file happen later, in
`trace.NewTracer` and `trace.NewLogSink`, matching how
`identity.NewRegistry` handles the environment.

### `internal/cli` — unchanged

`serve.go` already returns `gateway.New`'s error, which is how startup
failures from the tracer and sink surface.

## Configuration

```yaml
pipeline:
  stages:
    identification: false
    tracing: true

tracing:
  # Env var holding the fingerprint HMAC key (>= 32 bytes). If unset,
  # a random key is generated at startup (with a warning), and
  # fingerprints no longer match across restarts or instances.
  fingerprint_key_env: "TRACE_FINGERPRINT_KEY"
  # Largest part of a response tracing reads to record tool calls: one
  # streamed tool call's arguments, or a whole non-streamed response.
  # Past it, the tool call (or response) is recorded as unread.
  max_read_bytes: 16777216 # 16 MiB
  sink:
    type: logging
    path: "" # "" = stdout; otherwise a file agentkeeper appends to
  sessions: []
  # - agents: ["agent-alpha"]
  #   sources:
  #     - body_field: metadata.user_id
  #       pattern: '_session_([0-9a-f-]{36})$'
  # - default: true
  #   sources:
  #     - header: X-Session-Id
  #     - baggage: session.id
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.stages.tracing` | bool | `true` | `AGENTKEEPER_PIPELINE_STAGES_TRACING` |
| `tracing.fingerprint_key_env` | string | `"TRACE_FINGERPRINT_KEY"` | `AGENTKEEPER_TRACING_FINGERPRINT_KEY_ENV` |
| `tracing.max_read_bytes` | int64 | `16777216` | `AGENTKEEPER_TRACING_MAX_READ_BYTES` |
| `tracing.sink.type` | string | `"logging"` | `AGENTKEEPER_TRACING_SINK_TYPE` |
| `tracing.sink.path` | string | `""` | `AGENTKEEPER_TRACING_SINK_PATH` |
| `tracing.sessions` | `[]SessionSourceList` | `[]` | not settable via env (list) |

Each scalar needs an entry in `defaults()` and a `SetDefault` in `Load()`.
`pipeline.stages.tracing` must be registered explicitly, since it
defaults to `true` (0001's rule). All of it goes into
`configs/agentkeeper.example.yaml`.

**The fingerprint key is optional.** With `TRACE_FINGERPRINT_KEY` unset,
a fresh install starts normally, as the functional spec's defaults
require. It generates a random key and logs once, at warn level:

```
fingerprint key env var TRACE_FINGERPRINT_KEY is not set; using a random
per-process key — fingerprints will not match across restarts or instances
```

The warning names the variable and never the generated key. A key that
is set but shorter than 32 bytes is a misconfiguration and fails
startup: an operator who set one meant to use it.

## Failure modes

| Condition | Result |
| --- | --- |
| `tool_result` block in the last message unreadable | `Tracer.Request` errors; 503 `agentkeeper_unavailable`, stage `tracing`; request record `outcome.result: failed`; provider never called |
| `tracingStage.Request` panics | Recovered by the pipeline (0001); same as above |
| Sink refuses the request record, call allowed | 503 `agentkeeper_unavailable`, stage `tracing`; error log with `call_id`; provider never called |
| Sink refuses the request record, call already refused | The refusal stands; error log with `call_id` |
| Tap panics while handling an event | Relay unaffected; that event's tool call `recorded: false`; parsing continues with the next event; tracing `observation: failed`; error log with `call_id` |
| Tap panics outside event handling | Relay unaffected; tap not called again; open tool calls `recorded: false`; later events not read; tracing `observation: failed`; error log |
| gzip parser goroutine panics | Recovered in the goroutine; pipe drained so the relay never blocks; tracing `observation: failed`; error log |
| Tool-call arguments not valid JSON | That tool call `recorded: false`; relay unaffected; tracing `observation: failed` |
| Tool-call arguments or one SSE event over `max_read_bytes` | That tool call `recorded: false`; tracing `observation: completed`; no error log |
| Non-streamed body over `max_read_bytes` | No tool calls read; finding `tool_calls_unread`; tracing `observation: completed`; no error log |
| Response encoding other than identity/gzip | No tool calls read; tracing `observation: failed`; error log |
| Observer panics | Recovered (0001); that stage `observation: failed` |
| Sink refuses the response record | Error log with `call_id`; the record is lost; the next call's request record hits the same sink |
| Provider unreachable, or agent leaves before the provider answers | 502 to the agent; response record `unanswered` |
| Agent disconnects mid-stream | Relay stops (as today); tool calls not yet closed are `not_delivered` |
| Session absent, malformed body, invalid value | No effect on the call; `session.status` is `absent` or `invalid` |
| Fingerprint key env var unset or empty | Random per-process key; one warn-level log line at startup; tracing works, fingerprints match only within the process |
| Fingerprint key shorter than 32 bytes; bad pattern; sink file not openable | `gateway.New` fails; `serve` exits before listening |
| Unknown sink type; `max_read_bytes <= 0`; sessions entry malformed, duplicated default, unknown or repeated agent; source setting zero or two kinds | `config.Load` fails; `serve` exits before listening |

Requests **fail closed**: a request that cannot be recorded never
reaches the provider. Responses **never fail**: every response failure
above leaves the relay byte-identical and shows up only in the record
and the logs. Both come straight from the functional spec's *Failure
behavior*.

Nothing logged contains prompt content, argument or result values, a
session value that failed validation, an API key or an `Authorization`
header. Log lines carry `call_id`, the stage, the record type and an
error built in `internal/trace`. Those errors never wrap a decoder's
error: `encoding/json`'s syntax errors quote the offending input
character. Instead they name what failed and where, e.g. `tool call
index 2: arguments are not valid JSON at byte 118`, taking the offset
from `*json.SyntaxError.Offset`.

## Testing

`internal/trace` (table-driven):

- **Canonical JSON.** Key order, whitespace and `<` vs `<` do not
  change the output; numbers keep their text (`1.0` vs `1`); non-object
  arguments give `argument_names: []`.
- **Fingerprints.** Equal canonical input gives equal fingerprints;
  another key gives another; format is `fp_` + 16 hex.
- **`NewTracer`:**
  - an unset key var succeeds, logs one warning naming the var (checked
    through `zaptest/observer`), and never logs key bytes;
  - two tracers built without a key give different fingerprints for the
    same input, and two built with the same key give equal ones;
  - rejects a key shorter than 32 bytes, a pattern that does not
    compile, and a pattern with zero or two groups, naming the source.
- **Sessions** (criteria 17–22):
  - header, body field and baggage sources each work;
  - order and fallback between sources;
  - per-agent list vs default, and no list at all;
  - a pattern match extracts only group 1, and a non-match counts as
    absent;
  - a 300-byte value and a value containing `\n` are `invalid`, with no
    trace of the value in the `Session`;
  - a non-string body field is `invalid`;
  - baggage properties and percent-encoding are handled.
- **`Request`:**
  - only the last message's `tool_result` blocks are read (criterion 6);
  - `is_error` defaults to false;
  - absent `content` gives size 0 and a `null` fingerprint;
  - a missing `tool_use_id` is an error;
  - a non-JSON body gives `model: nil` and no error.
- **Tap, SSE mode.** A recorded Anthropic stream with text, a
  `tool_use` and a `server_tool_use` block:
  - fed whole, byte by byte, and split mid-line and mid-event, with the
    same `ResponseInfo` every time;
  - `relayed_at` is the time passed with the write carrying
    `content_block_stop`;
  - `Disconnected` before a block closes makes it `not_delivered`;
  - a truncated stream leaves `stop_reason` nil;
  - gzip-encoded input gives the same result, with `relayed_at` inside
    the write that carried the compressed event.
- **Tap, JSON mode.** A non-streamed response with a tool call gives the
  same argument fingerprint as the streamed version of the same call.
  Error bodies give no tool calls and nil identifiers.
- **Read limit** (criteria 30, 31):
  - arguments over `max_read_bytes` give `recorded: false` with
    `delivered` and `relayed_at` set, and `Failed` unset;
  - an SSE event over the cap is skipped, and its tool call is
    `recorded: false`;
  - a non-streamed body over the cap sets `Unread` and leaves
    `tool_calls` empty, both plain and gzip-compressed (a small gzip body
    that inflates past the cap counts);
  - invalid argument JSON gives `recorded: false` and `Failed`.
- **Crash protection** (criterion 26):
  - a handler forced to panic on one tool call's `content_block_stop`
    gives that tool call `recorded: false`, a later tool call recorded in
    full, `stop_reason` from `message_delta`, and `Failed`;
  - a panic in the gzip goroutine does not crash the test process, does
    not block `Write`, and `Result` returns with `Failed`;
  - a panic in line splitting, caught by the recorder, leaves open tool
    calls `recorded: false`.
- **`LogSink`:**
  - one line per record;
  - concurrent writers never interleave;
  - every field present, and `null`/`[]` encoded as such (criterion 15);
  - timestamps have exactly six fractional digits and end in `Z`;
  - appends to an existing file;
  - an unwritable path fails at construction.

`internal/pipeline` (extending the `fakeStage` table; a `fakeSink`
records calls and can be told to fail):

- **Records per call.** Exactly one request and one response record per
  forwarded call, sharing a `call_id`. A denied call gets only a request
  record (criteria 2, 3).
- **Order.** The request record is written before the upstream handler
  runs: assert the order of events in a shared log (criterion 2). The
  response record is written after every observer has returned.
- **Stage results.** Allowed / denied / not_run across three stages,
  with `null` timings for `not_run` (criterion 3). An observer panic
  gives `observation: "failed"` with `delivered` (criterion 28).
- **Sink failures:**
  - A failing sink denies with 503 and never calls upstream, and the next
    call succeeds once the sink recovers (criterion 11).
  - A sink failing only on the response record logs the `call_id`, and
    the agent has the full body (criterion 12).
- **Streaming unchanged.** The 0001 SSE test, rerun with a tap attached,
  shows byte-identical chunks delivered as produced (criterion 1). A
  panicking tap leaves the relay byte-identical (criterion 26).
- **Tool calls past the cap.** A response over `max_body_bytes` with a
  tool call at its end still lists the tool call (criterion 13).
- **Unanswered.** A writer's `UpstreamFailed()` gives `unanswered`,
  `upstream_status: null` (criterion 14).
- **Disconnect.** An upstream streaming two tool calls, and a client
  writer that starts failing after the first tool call's
  `content_block_stop`: the recorder calls `Disconnected`, and the record
  has the first tool call `delivered` and the second `not_delivered`
  (criterion 29).
- **Refused bodies.** A 413 (over `max_body_bytes`) and a 400 (read
  error) each give a request record with `size_bytes: null`. An accepted
  body gives its exact length.
- **`pipeline.New` with a nil sink** writes no records.

`internal/provider`: the error handler writes 502 and calls
`UpstreamFailed` on a writer that has it. An upstream that closes the
connection makes the proxy call it.

`internal/gateway`:

- `buildStages` order with all three stages on;
- tracing off gives no sink and no records;
- a short key or a bad pattern makes `gateway.New` fail, and an unset
  key does not;
- end to end against an `httptest` Anthropic fake: call A returns a
  `tool_use`, call B sends its `tool_result`, and joining the two
  records gives B's `received_at` minus T's `relayed_at` (criterion 5);
- a caller `traceparent` appears in no record (criterion 10);
- criterion 18's pattern leaves no `abc`/`def` in records or logs, with
  logs captured through `zaptest/observer`;
- **no argument values anywhere** (criterion 4): tool-call arguments and
  tool-result content carry marker strings. That covers a well-formed
  call, one whose arguments are invalid JSON (so a decode error is
  logged), and one over the read limit. No marker appears in any
  record line or any captured log entry, fields included;
- **unknown tool result** (criterion 7): call B's last message carries a
  `tool_result` for an ID no earlier response contained. B reaches the
  fake provider unchanged, its request record lists the result like any
  other, and `findings` is `[]`;
- **model alias** (criterion 25): the fake answers a request for `M`
  with model `M-1`, id `R` and stop reason `tool_use`, once streamed and
  once not. Both times the request record has `model: "M"`, and the
  response record has `response_model: "M-1"`, `response_id: "R"` and
  `stop_reason: "tool_use"`.

`internal/config`:

- `tracing` defaults to true, with the default sink, key env and
  `max_read_bytes`;
- every `Load` rejection listed above;
- env overrides for the scalar fields;
- an unknown key inside a sessions entry is rejected.

Criteria 8, 9 and 16 (restart, two instances, assembly from records
alone) follow from nothing being kept between calls. Cover them with one
test that builds two separate gateways, sends call A through the first
and call B through the second, and joins the records. Run it twice:

- with the same key, where B's records equal those from a single gateway;
- with no key, where the join still holds and B's records differ from a
  single gateway's only in `fingerprint` fields.

## Alternatives considered

- **Tracing as an ordinary stage and observer, with a "before forward"
  hook** — rejected: tracing is second in the order and cannot see later
  stages' decisions, so the pipeline would have to expose them anyway.
  The rule "no request reaches the provider without its record" would
  then hang on a hook contract rather than one line in `Handler`.
- **Reading tool calls from `Call.ResponseBody` after the response** —
  rejected: the body is capped at `max_body_bytes` (rule 9), and by then
  the time each tool call was relayed is gone.
- **Buffering tool-call arguments and non-streamed bodies with no cap**
  — rejected: one pathological response would cost unbounded memory. The
  cap shows up in the record as `recorded: false` or `tool_calls_unread`,
  never silently.
- **Reusing `pipeline.max_body_bytes` as the read limit** — rejected:
  rule 9 says that limit does not apply to tool calls, and an operator
  who lowers it to protect memory on requests would lose tool calls
  without meaning to.
- **Treating the read limit as a tracing failure** — rejected: nothing
  broke, and an error-level log for every large file write would bury
  real failures. The record says what was unread.
- **Parsing non-streamed JSON incrementally instead of capping it** —
  rejected for now: it needs a streaming JSON tokenizer for a case the
  16 MiB default already covers.
- **Fingerprints over raw relayed bytes** — rejected: streamed and
  non-streamed forms of the same arguments would fingerprint differently,
  which breaks matching against the agent's own logs.
- **Requiring the fingerprint key** — rejected: with tracing on by
  default, a fresh install would refuse to start, breaking the functional
  spec's promise that defaults pass traffic as today. A generated key
  with a warning keeps zero-config working, and cross-process matching is
  one env var away.
- **Turning tracing off by default until a key is set** — rejected: the
  audit trail would be missing by default, which is the gap tracing
  exists to close.
- **Per-agent `session_sources` inside `agents[]`** — rejected: one list
  could not be shared across agents, and "the default given twice" would
  have no natural home.
- **Writing records through the zap logger** — rejected: zap wraps each
  entry in its own fields and would share a stream with application logs.
  Rule 22 and the logging sink's format forbid both.
- **Stripping `Accept-Encoding` so the tap never sees gzip** — rejected:
  it alters the request, and the functional spec forwards requests
  unchanged.
- **Detecting "unanswered" from a 502 with an empty body** — rejected: a
  provider or a proxy in front of it can send exactly that.

## Open questions

- **`relayed_at` for non-streamed responses** is the time of the last
  relayed write, since a JSON body can only be parsed once complete.
  That slightly overstates tool duration's start for non-streamed calls.
  Decide whether the functional spec should say so.
- **0001 technical spec** must be updated in the same change: `New`'s
  signature, `Call.Trace`, `AllowTraced`, and timed observers.

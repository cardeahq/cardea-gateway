# 0001 — Staging pipeline (technical)

**Date:** 2026-09-13
**Functional spec:** specs/0001-staging-pipeline-functional.md

## Context

The gateway currently wraps the provider handler in an ad-hoc middleware
chain built by `stages()` in `internal/gateway/gateway.go`, holding one
token counter. The RFC needs six governance steps that can each refuse a
call, all producing an identical denial to the caller and an outcome
record to audit. `func(http.Handler) http.Handler` cannot express that:
"deny" becomes "don't call next and write something", which every stage
would spell differently and any stage could forget to record.

This spec replaces that chain with an explicit pipeline. It defines the
contract only — the six stages are separate specs.

## Goals

- One `Stage` contract, with denial and outcome recording owned by the
  pipeline rather than by each stage.
- Read the request body once, for all stages.
- Preserve streaming responses exactly as today.
- Leave behavior unchanged under the default config, where every stage is
  enabled but none has a rule to enforce.

## Non-goals

- Implementing any of the six stages.
- Request or response mutation by stages (deferred to the filtering
  spec).
- Per-stage timeouts, retries, parallel stage execution.

## Design

A new package `internal/pipeline` owns the sequence. `internal/gateway`
builds the ordered stage slice from config and mounts the pipeline
between the mux and the provider handler:

```
mux "/" -> pipeline.Handler(provider.Handler())
mux "/healthz" -> gateway.handleHealthz          (never enters the pipeline)
```

Per call, `Handler` does:

1. Build a `Call`: generate `ID`, stamp `Received`, read and cap the
   request body, restore `r.Body` so the provider still sees it.
2. Run each stage's `Request` in slice order. First `Decision` with
   `Allow == false`, or first non-nil `error`, short-circuits.
3. On allow, invoke `next` with a `recorder` wrapping the
   `ResponseWriter`. On deny, write the error body; `next` is never
   called.
4. Fill in the outcome on `Call` (status, duration, denier, response
   bytes), then invoke `Response` on every enabled stage that implements
   `ResponseObserver`, in reverse slice order, regardless of how the call
   ended.

Reverse order in step 4 mirrors the nesting the middleware chain had, and
means audit — last to decide — is first to see the outcome.

Stage ordering is a compiled-in slice in `internal/gateway`, in the RFC's
order. Config contributes booleans only; a new stage spec adds its
constructor at its position in that slice. This is why order is not a
config key: putting rate-limiting before identification would produce
denials with no `agent_id` to attribute them to.

## Packages and types

### `internal/pipeline` — new

`pipeline.go`:

```go
// Call is the per-request state shared by every stage. Stages read it;
// only the pipeline writes it.
type Call struct {
	ID       string
	Received time.Time
	Request  *http.Request

	Body          []byte
	BodyTruncated bool

	Status   int
	Duration time.Duration
	Denial   *Denial

	ResponseBody      []byte
	ResponseTruncated bool
	ResponseErr       error
}

// Denial records which stage refused a call and why. Message is
// caller-visible; Reason is audit-only and must never be written to the
// response.
type Denial struct {
	Stage   string
	Status  int
	Message string
	Reason  string
}

type Decision struct {
	allow   bool
	status  int
	message string
	reason  string
}

func Allow() Decision
func Deny(status int, message, reason string) Decision

type Stage interface {
	Name() string
	Request(ctx context.Context, c *Call) (Decision, error)
}

// ResponseObserver is implemented by stages that need the call's
// outcome. Response runs even when another stage denied the call, so
// implementations must handle c.Denial != nil and an empty
// c.ResponseBody.
type ResponseObserver interface {
	Response(ctx context.Context, c *Call)
}

type Pipeline struct { /* stages, maxBody, logger */ }

func New(logger *zap.Logger, maxBodyBytes int64, stages ...Stage) *Pipeline

func (p *Pipeline) Handler(next http.Handler) http.Handler
```

`Decision`'s fields are unexported so `Allow()`/`Deny()` are the only way
to build one — a zero-value `Decision{}` returned by a buggy stage must
not read as "allow".

Panic containment, in `Handler`:

```go
func (p *Pipeline) call(ctx context.Context, s Stage, c *Call) (d Decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("stage %s panicked: %v", s.Name(), r)
		}
	}()
	return s.Request(ctx, c)
}
```

`recorder.go` — the `bodyRecorder` moved out of
`internal/gateway/tokens.go`, unchanged in behavior: write-through plus
`Flush` pass-through so SSE keeps streaming, with a cap at `maxBodyBytes`
that sets `ResponseTruncated` instead of dropping the write. Gzip
decoding moves here too, so each stage does not redo it.

`errors.go` — denial rendering. Response body:

```json
{
  "error": {
    "type": "agentkeeper_denied",
    "stage": "rate_limiting",
    "message": "request quota exceeded",
    "request_id": "ak_7f3c2b1d9e8a4f60"
  }
}
```

`type` is `agentkeeper_unavailable` with status 503 for the stage-failure
case. `Content-Type: application/json`, plus
`X-Agentkeeper-Request-Id: <Call.ID>`. `Denial.Reason` is not marshalled.

`Call.ID` is `"ak_" + 16 hex chars` from `crypto/rand`. Any inbound
`X-Agentkeeper-Request-Id` is deleted from the request before stages run,
so a caller cannot seed or collide the audit trail.

### `internal/gateway` — modified

- `gateway.go`: `stages()` and `chain()` are deleted;
  `logRequests`/`displayTokenUsage` fold into the token stage. `New`
  builds the ordered slice:

```go
func buildStages(cfg config.Config, logger *zap.Logger) []pipeline.Stage
```

  It appends, in the RFC's fixed order, each stage whose config boolean
  is true. Today only `token_counting` exists, so the slice has at most
  one element; each future stage spec inserts its constructor at its
  position.

- `tokens.go`: `countTokensStage` becomes a `Stage`:

```go
type tokenStage struct {
	logger  *zap.Logger
	counter *tokenCounter
}

func (s *tokenStage) Name() string
func (s *tokenStage) Request(ctx context.Context, c *Call) (pipeline.Decision, error) // always Allow
func (s *tokenStage) Response(ctx context.Context, c *Call)
```

  `Response` runs `provider.ParseUsage(c.ResponseBody)` and logs the
  running total — the same two log lines as today, from one place. The
  gzip handling in the current `countTokensStage` is dropped; the
  recorder does it.

### `internal/config` — modified

New `PipelineConfig` on `Config`, validated in `Load`: unknown keys under
`pipeline.stages` are rejected (Viper `UnmarshalExact` on that subtree)
and `max_body_bytes <= 0` is an error.

## Configuration

```yaml
pipeline:
  # Largest request body read for stage inspection, and the cap on how
  # much of a response is retained for observation.
  max_body_bytes: 10485760 # 10 MiB
  stages:
    token_counting: true
```

| Field                            | Type  | Default    | Env var                                      |
| -------------------------------- | ----- | ---------- | -------------------------------------------- |
| `pipeline.max_body_bytes`        | int64 | `10485760` | `AGENTKEEPER_PIPELINE_MAX_BODY_BYTES`        |
| `pipeline.stages.token_counting` | bool  | `true`     | `AGENTKEEPER_PIPELINE_STAGES_TOKEN_COUNTING` |

Both need a default in `defaults()` and `Load()`, plus the entry in
`configs/agentkeeper.example.yaml`. Each future stage spec adds one
`pipeline.stages.<name>` boolean defaulting to `true`, and its own config
section for that stage's settings — which is where that stage becomes a
gate. A stage whose own settings are empty must allow every call
(functional spec § *Behavior*, rule 4), so that a default install runs
the full pipeline and denies nothing.

Because the defaults are `true`, every stage boolean must be registered
in `defaults()`: an unset key would otherwise unmarshal to Go's zero
value and silently disable the stage.

## Failure modes

| Condition                                               | Result                                                                                                |
| ------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `Stage.Request` returns `error`                         | 503 `agentkeeper_unavailable`, stage named; `next` not called; logged at error                        |
| `Stage.Request` panics                                  | Recovered into an error; identical to the above; process survives                                     |
| `Decision` is `!allow`                                  | Its status/message; short-circuit; logged at warn with `Reason`                                       |
| Request body exceeds `max_body_bytes`                   | 413 `agentkeeper_denied`, stage `"pipeline"`, before any stage runs                                   |
| Reading the request body fails                          | 400 `agentkeeper_denied`; not forwarded                                                               |
| Response exceeds `max_body_bytes`                       | Streamed to the caller in full; `ResponseTruncated = true` and observers see a partial `ResponseBody` |
| `ResponseObserver.Response` panics                      | Recovered and logged; the caller's response is untouched                                              |
| Unknown `pipeline.stages` key, or `max_body_bytes <= 0` | `config.Load` error; `serve` exits non-zero at startup                                                |

Requests fail **closed** — the rationale is in the functional spec §
*Failure behavior*. Responses cannot: by the time an observer runs, the
caller already has the bytes.

## Testing

`internal/pipeline` (new, table-driven, with `fakeStage` variants:
allow / deny / error / panic / observer):

- Order: stages run in slice order; first denial short-circuits and later
  stages' `Request` is never called.
- Observers run in reverse order, exactly once each, for allowed, denied
  and failed calls alike, with `c.Denial` populated in the latter two.
- Deny rendering: status, JSON body, `X-Agentkeeper-Request-Id` header,
  and — asserted explicitly — that `Denial.Reason` does not appear in the
  body.
- Request ID: generated, unique across calls, and an inbound
  `X-Agentkeeper-Request-Id` is neither honored nor echoed.
- Body: read once, `r.Body` still readable by `next` and byte-identical;
  over-limit denied with 413 and `next` never invoked.
- Panic in `Request` and in `Response` are both contained.
- Streaming: an `httptest` SSE upstream that flushes three chunks with a
  gap; assert each chunk reaches the client before the handler returns
  and that `Response` runs only after the last one.
- Zero-value `Decision{}` from a stage denies rather than allows.

`internal/gateway`: `/healthz` answers 200 with a deny-everything stage
enabled; `buildStages` honors the config booleans and produces the fixed
order. Existing `tokens_test.go` is rewritten against `tokenStage`,
keeping the gzip and `ParseUsage` cases (gzip now asserted via the
recorder).

`internal/config`: defaults — every stage boolean is `true` with an empty
config file — env-var override, explicit `false` honored,
unknown-stage-key rejection, non-positive `max_body_bytes` rejection.

## Alternatives considered

- **Keep `func(http.Handler) http.Handler`** — rejected: denial shape and
  audit recording would be re-implemented per stage, and a stage that
  returns without calling `next` and without recording is an invisible
  hole in the audit trail.
- **Interface plus a middleware adapter** — rejected: two models for one
  gateway; the adapter exists only to preserve a chain nothing needs.
- **Config-driven stage order** — rejected: order is a security property.
  Filtering before identification yields denials that cannot be
  attributed.
- **Fail open on stage error** — rejected: it makes crashing a stage the
  cheapest bypass of the control the stage implements.
- **Provider-shaped denial errors** — rejected: indistinguishable from a
  real upstream failure, so operators debug the wrong system.
- **`Call` as a `context.Context` value** — rejected: the compiler stops
  helping, and every stage would need the same unchecked type assertion.

## Open questions

- Whether `Stage.Request` should receive a per-stage `context.Context`
  with its own deadline, or inherit the request's. Deferred until a stage
  makes a network call (the policies stage, first).
- Buffering the whole request body defeats streaming *uploads*. No
  current provider request streams, so this is acceptable now; revisit if
  one does.

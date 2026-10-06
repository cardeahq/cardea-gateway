# 0001 — Staging pipeline (technical)

**Date:** 2026-09-15
**Functional spec:** specs/0001-staging-pipeline-functional.md

## Context

Today's gateway forwards every request to the configured provider through
`stages()` in `internal/gateway/gateway.go`, a `chain()` of
`func(http.Handler) http.Handler` middleware holding a single token
counter. Nothing in that chain can refuse a call.

The functional spec calls for six governance stages — identification,
policies, monitoring, filtering, rate-limiting, audit — each able to deny
a call, all producing an identical caller-facing error and an outcome
record for audit. Ordinary middleware has no vocabulary for "deny": a
stage that wants to refuse a call has to skip calling `next` and write its
own response, and nothing stops it from doing neither — forgetting both
the write and the audit record. Six independently-written stages sharing
that one failure mode is not a design, it's six chances to get it wrong.

This spec replaces the middleware chain with a typed pipeline that owns
denial and outcome recording itself, so a stage cannot skip either. It
defines the pipeline's contract only; the six stages are separate specs
that each implement `Stage`.

## Goals

- One `Stage` contract where denying a call and recording its outcome are
  the pipeline's job, not something every stage re-implements.
- Read the request body once, and hand every stage the same bytes.
- Keep streaming responses streaming, exactly as today.
- Under the default config — every stage enabled, none configured to
  enforce anything — behave exactly as the current gateway does.

## Non-goals

- The six stages themselves.
- Stages rewriting requests or responses (that's the filtering spec).
- Per-stage timeouts, retries, or running stages concurrently.

## Design

A new `internal/pipeline` package owns the stage sequence and the call
lifecycle. `internal/gateway` assembles the stage list from config and
inserts the pipeline between the mux and the provider handler:

```
mux "/healthz" -> gateway.handleHealthz         (bypasses the pipeline)
mux "/"        -> pipeline.Handler(provider.Handler())
```

`Handler` drives four steps for every call that reaches it:

1. **Build the `Call`.** Generate its `ID`, stamp `Received`, then read
   the entire request body and restore `r.Body` from the bytes so the
   provider still sees them. If the body exceeds `maxBodyBytes`, or the
   read itself fails, the call is denied right here — no stage runs,
   because a stage cannot evaluate bytes it never received — but the call
   still proceeds to step 4 so it still gets recorded.
2. **Run stages in order.** Call each stage's `Request` in the slice's
   order. The first `Decision` with `Allow == false`, or the first
   non-nil `error`, stops the loop immediately.
3. **Dispatch.** If every stage allowed the call, invoke `next` with a
   `recorder` wrapping the `ResponseWriter` so the response body can be
   captured without breaking streaming. If any stage (or step 1) denied
   the call, write the denial response instead and never call `next`.
4. **Notify observers.** Populate `Call`'s outcome fields — status,
   duration, response size, who denied it if anyone — then call
   `Response` on every enabled stage implementing `ResponseObserver`, in
   the *reverse* of request order. This runs unconditionally: allowed,
   denied by a stage, denied in step 1, or failed with an error. There is
   no exit from `Handler` that skips it.

Reversing the observer order in step 4 mirrors how a middleware chain
naturally nests — the outermost layer is the first thing to see a
response coming back — so the audit stage, being last in request order,
is first to observe the outcome.

The stage sequence itself is a slice compiled into `internal/gateway`, in
the order the functional spec fixes. Config can only toggle stages on or
off; it has no way to reorder or insert one, and a new stage spec adds its
constructor at its fixed position in that slice. Ordering is deliberately
not configurable: a config typo that put rate-limiting ahead of
identification would produce denials with no `agent_id` behind them.

## Packages and types

### `internal/pipeline` — new

`pipeline.go` defines the shared per-call state and the stage contract:

```go
// Call is the state every stage shares over the life of one request.
// Stages read it; only the pipeline mutates it.
type Call struct {
	ID       string
	Received time.Time
	Request  *http.Request

	// Body holds the whole request body. An over-limit request is denied
	// outright rather than truncated, so every stage that runs sees
	// exactly what the provider receives.
	Body []byte

	Status   int
	Duration time.Duration
	Denial   *Denial

	// ResponseBody is capped at maxBodyBytes, with ResponseTruncated set
	// when it was cut. The cap only affects what stages retain for
	// observation — the caller already has the full response by the time
	// this is populated, so there's no "deny" option here, only "keep
	// less of it."
	ResponseBody      []byte
	ResponseTruncated bool
	ResponseErr       error
}

// Denial is attached to Call when a stage — or the pipeline itself —
// refuses it. Message goes to the caller; Reason is for the audit trail
// only and must never leave the process in a response.
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

// ResponseObserver is the optional half of Stage for stages that need to
// see how a call ended. Response fires for every call the pipeline
// handles, including ones another stage denied, so an implementation
// must tolerate c.Denial != nil and a c.ResponseBody that's empty or
// absent.
type ResponseObserver interface {
	Response(ctx context.Context, c *Call)
}

type Pipeline struct { /* stages, maxBody, logger */ }

func New(logger *zap.Logger, maxBodyBytes int64, stages ...Stage) *Pipeline

func (p *Pipeline) Handler(next http.Handler) http.Handler
```

`Decision`'s fields are unexported on purpose: `Allow()` and `Deny(...)`
are the only way to produce one, so a stage that returns a bare
`Decision{}` by mistake denies rather than silently allowing.

`Handler` wraps every stage call so a panicking stage can't take the
process down with it:

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

`recorder.go` carries the `bodyRecorder` type over from
`internal/gateway/tokens.go` unchanged in behavior — it still writes
through to the real `ResponseWriter` and passes `Flush` along so SSE
keeps working, and still caps what it retains at `maxBodyBytes`, setting
`ResponseTruncated` instead of dropping bytes. Gzip decoding also moves
into this file, so it happens once instead of once per stage.

`errors.go` renders denials as:

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

A stage failure (error or recovered panic) uses `type:
"agentkeeper_unavailable"` with status 503 instead. A deliberate denial
(`agentkeeper_denied`) also sets `X-Should-Retry: false`, the header the
Anthropic and OpenAI SDKs honor to skip their automatic retry (functional
rules 7 and 9); a failure sets none, so SDK defaults apply. Both cases set
`Content-Type: application/json` and `X-Agentkeeper-Request-Id:
<Call.ID>`; `Denial.Reason` never appears in the body.

`Call.ID` is generated as `"ak_"` followed by 16 hex characters from
`crypto/rand`. Any inbound `X-Agentkeeper-Request-Id` header is stripped
before stages run, so a caller has no way to plant or collide with the ID
that ends up in the audit trail.

### `internal/gateway` — modified

- `gateway.go`: `stages()` and `chain()` go away. `logRequests` and
  `displayTokenUsage` fold into the token-counting stage's `Response`.
  `New` builds the fixed, config-filtered slice:

  ```go
  func buildStages(cfg config.Config, logger *zap.Logger) []pipeline.Stage
  ```

  It walks the RFC's fixed order and appends the constructor for each
  stage whose config boolean is `true`. Since only token counting exists
  today, the slice has at most one element; each later stage spec inserts
  its constructor at its position in this function.

- `tokens.go`: `countTokensStage` becomes a `pipeline.Stage`.

  > **Removed later.** The token counting stage, `tokens.go`,
  > `provider.ParseUsage` and the `pipeline.stages.token_counting` key
  > were deleted after call tracing (0003) shipped: the stage's only
  > output was a running-total log line, and every response record now
  > carries its call's `tokens`. The design below is kept as history.


  ```go
  type tokenStage struct {
  	logger  *zap.Logger
  	counter *tokenCounter
  }

  func (s *tokenStage) Name() string
  func (s *tokenStage) Request(ctx context.Context, c *Call) (pipeline.Decision, error) // always Allow
  func (s *tokenStage) Response(ctx context.Context, c *Call)
  ```

  `Response` calls `provider.ParseUsage(c.ResponseBody)` and logs the same
  two lines the gateway logs today, now from a single place instead of
  two middleware layers. The gzip handling that used to live in
  `countTokensStage` is dropped — the recorder does it now.

### `internal/config` — modified

`Config` gains a `PipelineConfig`. `Load` rejects unknown keys under
`pipeline.stages` (via Viper's `UnmarshalExact` scoped to that subtree)
and rejects `max_body_bytes <= 0`.

## Configuration

```yaml
pipeline:
  # Largest request body the pipeline will read for stage inspection;
  # also the cap on how much of a response is retained for observation.
  max_body_bytes: 10485760 # 10 MiB
  stages:
    token_counting: true
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.max_body_bytes` | int64 | `10485760` | `AGENTKEEPER_PIPELINE_MAX_BODY_BYTES` |
| `pipeline.stages.token_counting` | bool | `true` | `AGENTKEEPER_PIPELINE_STAGES_TOKEN_COUNTING` |

Both fields need entries in `defaults()` and `Load()`, plus documentation
in `configs/agentkeeper.example.yaml`. Every future stage adds its own
`pipeline.stages.<name>` boolean, defaulting to `true`, plus whatever
settings section actually turns it into an enforcing gate — a stage with
an empty settings section must allow everything (functional spec §
*Behavior*, rule 4), so a fresh install runs the whole pipeline without
denying a single call.

Because every stage boolean defaults to `true`, each one must be
registered explicitly in `defaults()`. An unregistered key would
unmarshal to Go's zero value — `false` — and silently pull a stage out of
the path.

## Failure modes

| Condition | Result |
| --- | --- |
| `Stage.Request` returns an `error` | 503 `agentkeeper_unavailable`, names the stage; `next` never called; logged at error level |
| `Stage.Request` panics | Recovered into an error, same handling as above; the process keeps running |
| A `Decision` has `allow == false` | Its status and message go to the caller; the pipeline short-circuits; logged at warn with `Reason` |
| Request body exceeds `max_body_bytes` | 413 `agentkeeper_denied`, stage recorded as `"pipeline"`, before any stage's `Request` runs; observers still fire |
| Request body read fails | 400 `agentkeeper_denied`; nothing forwarded; observers still fire |
| Response exceeds `max_body_bytes` | Still streamed to the caller in full; `ResponseTruncated = true` and observers see a partial `ResponseBody` |
| `ResponseObserver.Response` panics | Recovered and logged; does not touch the response the caller already received |
| Unknown key under `pipeline.stages`, or `max_body_bytes <= 0` | `config.Load` fails; `serve` exits non-zero before listening |

Requests fail **closed**; the functional spec's § *Failure behavior*
covers the reasoning. Responses are the one thing that can't fail closed
— once an observer runs, the caller already has whatever bytes were sent.

## Testing

`internal/pipeline` (new; table-driven, backed by a `fakeStage` with
allow / deny / error / panic / observer variants):

- Stage order: stages run in slice order, and a denial stops later
  stages' `Request` from running at all.
- Observer order and coverage: observers run in reverse order, exactly
  once per call, for allowed, denied, and failed calls alike, with
  `c.Denial` populated for the latter two.
- Denial rendering: correct status, JSON shape, `X-Agentkeeper-Request-Id`
  header, and an explicit assertion that `Denial.Reason` never appears in
  the body.
- Do-not-retry signal: a denial (including the pipeline's own 413 and 400)
  carries `X-Should-Retry: false`; a stage failure carries no
  `X-Should-Retry` header.
- Request IDs: generated, unique per call, and an inbound
  `X-Agentkeeper-Request-Id` is neither honored nor echoed back.
- Body handling: read once; `r.Body` is still fully and identically
  readable by `next`; an over-limit body is denied with 413 and `next` is
  never invoked.
- Panic containment in both `Request` and `Response`.
- Pipeline-level denial (step 1): an over-limit or unreadable body denies
  the call before any stage's `Request` runs, and every observer still
  fires exactly once with `c.Denial` set.
- Streaming: an `httptest`-based SSE upstream that flushes three chunks
  with delays between them; assert each chunk reaches the client as it's
  produced, and that `Response` only runs after the last one.
- A zero-value `Decision{}` from a stage denies, it does not allow.

`internal/gateway`: `/healthz` still answers 200 even with a
deny-everything stage enabled; `buildStages` respects the config booleans
and preserves the fixed order. `tokens_test.go` is rewritten against
`tokenStage`, keeping its gzip and `ParseUsage` coverage (gzip is now
exercised through the recorder instead of directly).

`internal/config`: defaults produce every stage boolean as `true` from an
empty config file; env-var overrides and explicit `false` are honored;
unknown stage keys and non-positive `max_body_bytes` are rejected.

## Alternatives considered

- **Stay with `func(http.Handler) http.Handler`** — rejected: every stage
  would reinvent denial shape and audit recording on its own, and a stage
  that returns without calling `next` and without recording anything is
  an audit gap no one would notice until it mattered.
- **A `Stage` interface adapted onto the existing middleware chain** —
  rejected: running two models side by side just to keep a chain
  abstraction that nothing else needs isn't worth the complexity.
- **Config-driven stage ordering** — rejected: order is load-bearing for
  security, not a preference. Filtering ahead of identification produces
  denials nobody can attribute to an agent.
- **Fail open when a stage errors** — rejected: that makes crashing a
  stage the cheapest way to bypass whatever it enforces.
- **Denial errors shaped like the provider's own error responses** —
  rejected: operators couldn't tell a governance denial from a real
  upstream outage, and would debug the wrong system.
- **Passing `Call` through `context.Context` instead of as a parameter** —
  rejected: it trades compile-time checking for a type assertion every
  stage would have to repeat.

## Open questions

- Whether `Stage.Request` should get a context scoped to that stage, with
  its own deadline, or simply inherit the request's context. This is
  deferred to whichever stage first makes a network call — expected to be
  policies — since a deadline is meaningless until something can actually
  be slow. Note that a deadline by itself bounds nothing: nothing forces
  a goroutine to stop, so `ctx` only helps a stage that checks
  `ctx.Done()` or passes `ctx` into something that does. Actually
  enforcing a limit on a stage that doesn't cooperate means running it in
  its own goroutine and walking away from it at the deadline — which
  leaks that goroutine and whatever it's holding. Whoever resolves this
  is choosing between cooperative cancellation with a known gap, and
  enforced cancellation with a known leak.
- Reading the full request body up front rules out streaming *uploads*.
  No current provider request streams its body, so this is fine for now;
  worth revisiting the day one does.

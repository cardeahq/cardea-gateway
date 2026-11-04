# 0009 — Multi-provider routing (technical)

**Date:** 2026-11-03
**Functional spec:** specs/0009-multi-provider-routing-functional.md

## Context

Today `cli.serve` builds one `provider.Provider` and passes it to
`gateway.New`. Three places fix the provider for the life of the process:
`pipeline.New(…, provider string, …)` writes it into every trace record,
`policy.New(…, providerName, …)` puts it into every policy input and gates
conversation parsing on it, and `filtering.New(…, providerName, …)` gates
redaction on it. This design makes the provider a per-call value, chosen
by a new routing stage, and keeps everything else in the request path.

## Goals

- Choose a provider per call: model first, URL format as fallback, with a
  format guard and one default per format (functional rules 5–11).
- Carry the provider's name and kind per call to tracing, policies and
  filtering.
- Add a Gemini API provider.
- Strip every caller credential, including Gemini's, in one place.
- Keep the single `provider:` block behaving as before.
- Leave room for a provider that speaks several formats (specs/0011)
  without changing the routing algorithm.

## Non-goals

- Everything excluded by the functional spec: Bedrock (0010), Vertex
  (0011), format translation, fallback, load balancing, content readers
  for non-Anthropic formats, generic passthrough providers in a list.

## Design

### Request flow

```
cli.serve → config.Config → zap.Logger → provider.Set → gateway.Gateway

pipeline: body limit → authorization → identification → routing
          → tracing → policies → filtering → upstream dispatcher
```

1. The **routing stage** always runs, whether a provider list is
   configured or not. It is not a `pipeline.stages` toggle.
2. It detects the request's format (`routing.DetectFormat`), extracts the
   model (`routing.ExtractModel`), and asks the routing table for a
   provider (`routing.Table.Resolve`).
3. On success it returns `pipeline.AllowRouted(route, format, model)`. The
   pipeline copies these into `Call.Route`, `Call.Format` and `Call.Model`,
   which later stages read.
4. On failure it returns `pipeline.DenyWithModel(status, message, reason,
   model)`, with the status from the functional failure table. The
   pipeline copies the model into `Call.Model`, then takes its existing
   denial path: it writes the denial envelope, logs the existing "call
   denied" warning, and writes a request trace record. That record exists
   because the pipeline, not the tracing stage, builds request records for
   every call when a sink is configured (`pipeline.go`, the
   `buildRequestRecord` call). This is how functional rule 17 holds for
   routing denials, model included.
5. Before calling the upstream handler, the pipeline attaches the route to
   the request context. The gateway's **dispatcher** reads it and calls
   the chosen provider's handler.
6. Each provider's `Rewrite` strips caller credentials with one shared
   helper, then sets its own key.

### Two routing modes

- **List mode** (`routing.providers` is non-empty): the full algorithm in
  functional rules 5–11.
- **Single mode** (no list): the table has one route and resolves every
  call to it, including calls of unknown format and calls with no model.
  Single mode never denies. The stage skips format detection and model
  extraction entirely: it sets `Call.Format` from the route's kind, using
  `routing.FormatOf`, so a passthrough provider gets `FormatUnknown` and
  a Mistral provider gets `FormatOpenAI` whatever path the caller used.
  `Call.Model` is left nil, so request records take the model from
  tracing, exactly as today. This keeps policies, filtering and tracing
  gated on the provider's kind, as they are now (functional rule 4).

### Format detection

`DetectFormat` checks in this order and returns the first match:

1. An `anthropic-version` header is present → `anthropic`.
2. The path matches `/models/{model}:{action}` at its end, where `{model}`
   has no `/` or `:` and `{action}` is one of `generateContent`,
   `streamGenerateContent`, `countTokens`, `embedContent`,
   `batchEmbedContents`; or the path starts with `/v1beta/` → `gemini`.
   Matching the end of the path lets specs/0011 reuse it under Vertex's
   longer prefix.
3. The path is `/v1/messages` or `/v1/messages/count_tokens` → `anthropic`.
4. The path is `/v1/chat/completions`, `/v1/fim/completions`,
   `/v1/embeddings`, `/v1/models`, or starts with `/v1/models/` →
   `openai`.
5. Otherwise → unknown (`""`).

The path is `r.URL.Path`, compared exactly (no trailing-slash folding).

### Model extraction

- `anthropic` and `openai`: the top-level `model` member of the JSON body.
  The body is scanned token by token at the top level only. A second
  top-level `model` member returns `ErrRepeatedModel`. A body that is not
  a JSON object, or a `model` that is not a JSON string, or an empty
  string, yields "no model".
- `gemini`: the `{model}` segment from rule 2 above, taken from
  `r.URL.Path`, which Go has already percent-decoded once. It is not
  decoded again. This matches what the Gemini API reads after its own
  single decode, so `gemini-%2570ro` is matched and recorded as
  `gemini-%70ro`. The body is not read.
- unknown: no model.

Matching always uses the full model name. Only logs, denial reasons and
trace records shorten it, to `routing.MaxModelLen` (256) bytes on a UTF-8
boundary, using `routing.Truncate`.

### Resolution

`Table.Resolve(format, model, found)` in list mode:

1. Format unknown → deny 404, reason `unknown_format`.
2. `found` is true: collect routes, in list order, whose patterns match
   `model`. If any exist, return the first whose `Formats` contains
   `format`. If none of them speak it → deny 400, reason
   `format_mismatch`.
3. Otherwise (no model, or no route matched): return the default for
   `format` if one is configured, else deny 404, reason `no_provider`.

A pattern ending in `*` matches when `model` starts with the text before
the `*`. Any other pattern matches only the identical string.

`Route.Formats` is a slice so a multi-format provider (specs/0011) needs
no change here. Every kind in this spec has exactly one format.

Kind to format: `anthropic` → `anthropic`, `mistral` → `openai`,
`gemini` → `gemini`.

### Credentials

`provider.stripCallerCredentials(out *http.Request)` deletes the
`Authorization`, `Proxy-Authorization`, `x-api-key` and `x-goog-api-key`
headers and the `key` query parameter from the outbound request. The
Anthropic, Mistral and Gemini providers call it first in `Rewrite`, after
`SetURL`, then set their own credential:

| Kind | Credential set |
| --- | --- |
| anthropic | `x-api-key: <key>` |
| mistral | `Authorization: Bearer <key>` |
| gemini | `x-goog-api-key: <key>` |

The generic passthrough (`NewHTTPProxy`, single mode only) is unchanged
and forwards the request as sent (functional rule 4).

### What later stages read

- **Tracing** reads `model` and `stream` from any JSON body, as today,
  and also sets `stream` for a Gemini `streamGenerateContent` action. It
  reads tool results for every call routed by the single route
  (`c.Route.Single`), exactly as today, and in list mode only when the
  format is `anthropic`. The response tap reads token usage for single-
  route calls and for `anthropic`-format calls; otherwise it leaves
  `Tokens` nil, because the Anthropic usage reader would record an
  OpenAI-style `usage` object as zero tokens.
- **Request records** take `provider` from `Call.Route`. They take `model`
  from `c.Trace.Model` when the call went through the single route (as
  today), and otherwise from `Call.Model`, never falling back to tracing.
  Either value goes through `routing.Truncate` in `buildRequestRecord`. A
  call denied before or by routing has no route, and its record has no
  `provider` key at all.
- **Policies** put the route's provider name in `input.provider` and read
  conversation content only when the format is `anthropic`.
- **Filtering** redacts only when the format is `anthropic`.

### Unscanned providers

In `gateway.New`, "content checks are on" means
`Pipeline.Stages.Filtering`, or `Pipeline.Stages.Policies` with
`Policies.Path != ""`. When they are on:

- **List mode:** every route with a format that
  `filtering.ReadsFormat` rejects must have `AcceptUnscanned`. Otherwise
  `gateway.New` returns an error naming the provider. Each accepted route
  gets one warning at startup.
- **Single mode:** if the single provider's kind is not `anthropic`, one
  warning at startup. No error (functional rule 4).

Each warning names the provider and says that redaction and
conversation-based policy rules do not apply to its calls.

## Packages and types

### `internal/routing` — new

`format.go`, `model.go`, `table.go`, and their tests. Imports nothing
from `internal/`.

```go
type Format string

const (
    FormatUnknown   Format = ""
    FormatAnthropic Format = "anthropic"
    FormatOpenAI    Format = "openai"
    FormatGemini    Format = "gemini"
)

// DetectFormat applies the rules in Design → Format detection.
func DetectFormat(r *http.Request) Format

// MaxModelLen bounds a model name in logs, denial reasons and trace records.
const MaxModelLen = 256

// Truncate shortens s to at most MaxModelLen bytes on a UTF-8 boundary.
func Truncate(s string) string

// FormatOf returns the format a provider kind speaks; FormatUnknown for
// "" (passthrough) or an unknown kind.
func FormatOf(kind string) Format

var ErrRepeatedModel = errors.New("request names its model more than once")

// ExtractModel returns the model for format f (Design → Model extraction).
// found is false when there is no usable model. err is ErrRepeatedModel or nil.
func ExtractModel(f Format, r *http.Request, body []byte) (model string, found bool, err error)

// GeminiStreams reports whether a Gemini path asks for a streamed response.
func GeminiStreams(path string) bool

type Route struct {
    Provider string   // configured provider name
    Kind     string   // "anthropic", "mistral", "gemini"; "" for passthrough
    Formats  []Format // empty for passthrough
    Patterns []string // model patterns; empty in single mode
    Single   bool     // true only for the route built by NewSingleTable
}

type Rejection string

const (
    RejectNone           Rejection = ""
    RejectUnknownFormat  Rejection = "unknown_format"
    RejectFormatMismatch Rejection = "format_mismatch"
    RejectNoProvider     Rejection = "no_provider"
)

type Table struct { /* routes in order, defaults by format, single flag */ }

// NewTable builds a list-mode table. It errors when a default names no
// route or names a route that does not speak that format.
func NewTable(routes []Route, defaults map[Format]string) (*Table, error)

// NewSingleTable builds a single-mode table that resolves every call to r.
func NewSingleTable(r Route) *Table

// Resolve applies Design → Resolution. rej is RejectNone exactly when r is usable.
func (t *Table) Resolve(f Format, model string, found bool) (r Route, rej Rejection)

func (t *Table) Routes() []Route
func (t *Table) DefaultFormats(provider string) []Format
func (t *Table) Single() bool
```


In single mode, `Resolve` returns the single route with `RejectNone` for
every call. The single-mode stage does not call `DetectFormat` or
`ExtractModel` (Design → Two routing modes).

### `internal/provider` — modified

New `gemini.go` and `set.go`. `anthropic.go` and `mistral.go` call
`stripCallerCredentials`. `provider.go` gains the helper.

```go
// NewGeminiProvider forwards to the Gemini API with x-goog-api-key.
func NewGeminiProvider(baseURL, apiKeyEnv string) (Provider, error)

// Set holds every built provider by configured name.
type Set struct { /* map[string]Provider */ }

// NewSet builds one provider per entry by kind. It fails on an unknown
// kind or any constructor error, including an unset key variable.
func NewSet(entries []config.ProviderEntry) (*Set, error)

// NewSingleSet wraps the single provider under its configured name.
func NewSingleSet(name string, p Provider) *Set

func (s *Set) Get(name string) (Provider, bool)
```

`NewAnthropicProvider`, `NewMistralProvider` and `NewHTTPProxy` keep their
signatures. `Provider` is unchanged.

### `internal/config` — modified

```go
type Config struct {
    // ... existing fields
    Routing RoutingConfig `mapstructure:"routing"`
}

type RoutingConfig struct {
    Providers []ProviderEntry `mapstructure:"providers"`
    // Defaults maps a format ("anthropic", "openai", "gemini") to a provider name.
    Defaults map[string]string `mapstructure:"defaults"`
}

type ProviderEntry struct {
    Name            string   `mapstructure:"name"`
    Kind            string   `mapstructure:"kind"`
    BaseURL         string   `mapstructure:"base_url"`
    APIKeyEnv       string   `mapstructure:"api_key_env"`
    Models          []string `mapstructure:"models"`
    AcceptUnscanned bool     `mapstructure:"accept_unscanned"`
}

// ListMode reports whether a provider list is configured.
func (r RoutingConfig) ListMode() bool
```

`Load` decodes `routing` with `ErrorUnused`, as it does other sections, and
validates. The single provider counts as set when the config file sets
`provider` (`v.InConfig("provider")`) or any of
`CARDEA_GATEWAY_PROVIDER_NAME`, `CARDEA_GATEWAY_PROVIDER_BASE_URL`,
`CARDEA_GATEWAY_PROVIDER_API_KEY_ENV` is present (`os.LookupEnv`). When it
is set and `routing.providers` is non-empty, error. Then: names unique
and non-empty;
`kind` in `anthropic`, `mistral`, `gemini`; `base_url` absolute with a
host, `https`, or `http` on a loopback host (reuse `isLoopback`), no
userinfo; `api_key_env` non-empty; `models` non-empty, each entry
non-empty, `*` only as the last character; `defaults` keys in the three
formats; `defaults` with no provider list is an error. Checking that a
default's provider speaks its format happens in `routing.NewTable`, which
owns the kind-to-format mapping. Checking that a key variable is set
happens in `provider.NewSet`.

Viper reads `provider` from `defaults()` whether or not it was set, so
the conflict check must use `InConfig` and `os.LookupEnv`, not the decoded
struct.

### `internal/pipeline` — modified

```go
type Call struct {
    // ... existing fields
    Route  *routing.Route // set by the routing stage; nil before or on denial
    Format routing.Format
    Model  *string        // as extracted by routing; nil when none
}

// AllowRouted allows the call and attaches its route, format and model.
func AllowRouted(r routing.Route, f routing.Format, model *string) Decision

// DenyWithModel denies like Deny and records the model routing found.
func DenyWithModel(status int, message, reason string, model *string) Decision

// RouteFrom returns the route the pipeline attached to an upstream request.
func RouteFrom(ctx context.Context) (routing.Route, bool)

func New(logger *zap.Logger, maxBodyBytes int64, sink trace.Sink, stages ...Stage) *Pipeline
```

`Handler` copies a routed decision into `Call.Route`, `Call.Format` and
`Call.Model`, like it does `agentID`, and copies a `DenyWithModel` model
into `Call.Model`. A later stage cannot overwrite them. Before `serveNext`,
when `c.Route != nil`, it calls
`r = r.WithContext(withRoute(ctx, *c.Route))`. With no route it passes the
request unchanged, so a pipeline built without a routing stage (as in
existing tests) still works, and the dispatcher's no-route path is
reachable. `buildRequestRecord` and `buildResponseRecord` drop their
`provider` parameter and read `c.Route.Provider` when `c.Route != nil`.
`buildRequestRecord` takes `Model` from `c.Trace.Model` when
`c.Route != nil && c.Route.Single`, else from `c.Model`, then applies
`routing.Truncate`.

### `internal/trace` record types — modified

`RequestRecord.Provider` and `ResponseRecord.Provider` change from
`string` with `json:"provider"` to `*string` with
`json:"provider,omitempty"`, so a record with no route has no `provider`
key. The schema's top-level rule that every field is present (null when
it does not apply) gains an exception for `provider`, which already sits
outside `required`.

### `internal/gateway` — modified

New `routing.go`:

```go
type routingStage struct { table *routing.Table }

func (s *routingStage) Name() string // "routing"
func (s *routingStage) Request(ctx context.Context, c *pipeline.Call) (pipeline.Decision, error)

// dispatcher sends a routed request to its provider's handler.
type dispatcher struct { set *provider.Set }

func (d *dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

`Request` calls `DetectFormat`, then `ExtractModel`. `ErrRepeatedModel`
denies 400 with message "the request names its model more than once".
Otherwise it calls `Resolve` and maps rejections:

| Rejection | Status | Message to caller |
| --- | --- | --- |
| `format_mismatch` | 400 | the model is not available for this API |
| `no_provider` | 404 | no provider serves this request |
| `unknown_format` | 404 | no provider serves this request |

The `Deny` reason (logged, traced, never returned) is
`<rejection> format=<format> model=<model>`, with `model=` omitted when
none was found. `repeated_model` is the reason kind for the 400 above.

`dispatcher.ServeHTTP` reads `pipeline.RouteFrom`. A missing route or an
unknown provider name is a programming error. It responds 500 and logs at
error level, and never forwards. This can only happen if the pipeline is
wired wrongly.

`gateway.New` becomes:

```go
func New(cfg config.Config, logger *zap.Logger, set *provider.Set, table *routing.Table) (*Gateway, error)
```

It runs the unscanned-provider check (Design → Unscanned providers) and,
in list mode only, logs one startup line per route: `provider`, `kind`,
`models`, and `default_for`. `buildStages` inserts `routingStage` after identification,
unconditionally. The upstream becomes `pl.Handler(&dispatcher{set})`.
`policy.New` and `filtering.New` lose their provider argument.

### `internal/cli` — modified

`serve.go` builds the set and the table:

- **List mode:** `provider.NewSet(cfg.Routing.Providers)`, a `Route` per
  entry with `Formats: []routing.Format{routing.FormatOf(entry.Kind)}`,
  then `routing.NewTable(routes, defaults)`.
- **Single mode:** the existing `switch cfg.Provider.Name` plus a new
  `case "gemini"`, wrapped with `provider.NewSingleSet`, and
  `routing.NewSingleTable` with `Kind` set to the name for the three known
  kinds (else `""`) and `Formats` from `FormatOf`.

### `internal/policy` — modified

```go
type Request struct {
    // ... existing fields
    Provider string
    Format   string // routing format; conversation is read only for "anthropic"
}

func New(cfg config.PoliciesConfig, logger *zap.Logger) (*Engine, error)
```

`buildInput(r Request)` uses `r.Provider` for `input.provider` and
`r.Format == "anthropic"` where it used `provider == "anthropic"`. In single
mode with a passthrough provider, `Provider` is the configured name and
`Format` is `""`, the same result as today.

### `internal/filtering` — modified

```go
type Request struct {
    // ... existing fields
    Format string
}

func New(cfg config.FilteringConfig, logger *zap.Logger) (*Engine, error)

// ReadsFormat reports whether filtering can scan requests of format f.
func ReadsFormat(f string) bool // true only for "anthropic"
```

`buildTarget` takes the format instead of the provider name.

### `internal/trace` — modified

```go
// RequestOptions tells Request what it may read for this call.
type RequestOptions struct {
    ToolResults  bool // read tool_result blocks
    GeminiStream bool // the call is a Gemini streaming action
}

func (t *Tracer) Request(r *http.Request, body []byte, agentID string, opts RequestOptions) (RequestInfo, error)
func (t *Tracer) NewResponseTap(readUsage bool) *ResponseTap
```

The tracing stage sets `ToolResults` and `readUsage` to
`c.Route == nil || c.Route.Single || c.Format == routing.FormatAnthropic`.
A nil route only happens in a pipeline built without a routing stage
(stage tests); it keeps today's behavior. `GeminiStream` is
`c.Format == routing.FormatGemini && routing.GeminiStreams(path)`. `Request` reads session, `model` and
`stream` as today, sets `Stream` when `GeminiStream`, and reads tool
results only when `ToolResults`. With `readUsage` false the tap leaves
`Tokens` nil; everything else it reads is unchanged.
`schemas/trace-record.v1.schema.json`: the
`provider` description changes from "every call" to "the provider this
call was routed to", lists `gemini`, and says the key is absent when the
call was refused before or by routing. The `model` description says it
may come from the URL.

## Configuration

```yaml
# Existing single-provider block. Used only when routing.providers is empty.
provider:
  name: anthropic
  base_url: "https://api.anthropic.com"
  api_key_env: ANTHROPIC_API_KEY

routing:
  providers: []
  defaults: {}
```

Example list mode, for `configs/cardea-gateway.example.yaml` (commented):

```yaml
routing:
  providers:
    - name: anthropic
      kind: anthropic
      base_url: "https://api.anthropic.com"
      api_key_env: ANTHROPIC_API_KEY
      models: ["claude-*"]
    - name: mistral
      kind: mistral
      base_url: "https://api.mistral.ai"
      api_key_env: MISTRAL_API_KEY
      models: ["mistral-*", "codestral-*", "devstral-*"]
      accept_unscanned: true
    - name: gemini
      kind: gemini
      base_url: "https://generativelanguage.googleapis.com"
      api_key_env: GEMINI_API_KEY
      models: ["gemini-*"]
      accept_unscanned: true
  defaults:
    openai: mistral
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `routing.providers` | list | `[]` | none (lists are file-only) |
| `routing.providers[].name` | string | required | none |
| `routing.providers[].kind` | string | required | none |
| `routing.providers[].base_url` | string | required | none |
| `routing.providers[].api_key_env` | string | required | none |
| `routing.providers[].models` | list of string | required, non-empty | none |
| `routing.providers[].accept_unscanned` | bool | `false` | none |
| `routing.defaults` | map format → name | `{}` | none |

`defaults()` sets `Routing: RoutingConfig{Providers: nil, Defaults: nil}`.
`Load` adds `v.SetDefault("routing.providers", d.Routing.Providers)` and
`v.SetDefault("routing.defaults", d.Routing.Defaults)`. Provider keys are
read from the environment variables named by `api_key_env`; no key is
ever read from the file. `provider.name` gains the value `gemini`.

The example file's `provider:` block stays active, since it is the
default setup. The commented `routing:` example above it says to remove
or comment out `provider:` before enabling the list; otherwise the
Gateway refuses to start with the conflict error.

## Failure modes

The routing stage fails closed. It has no path that forwards a call it
could not route.

| Condition | Result |
| --- | --- |
| Repeated top-level `model` | Deny 400, reason `repeated_model` |
| `format_mismatch` | Deny 400 |
| `no_provider`, `unknown_format` | Deny 404 |
| Routing stage panics | The pipeline's existing panic recovery fails the call with 503 "governance unavailable" naming `routing` |
| Dispatcher finds no route or no provider | 500, error log, never forwarded |
| Provider unreachable | Existing `errorHandler`: 502 |
| Body over limit, unreadable body | Existing pipeline denial (413, 400) before any stage |

The first three rows apply in list mode only. The rest apply in both
modes.

Every routing denial goes through the pipeline's denial path, so it gets
the standard envelope, `X-Should-Retry: false`, the "call denied"
warning, and a request record when tracing has a sink.

Startup errors, returned before the listener opens: every validation in
`config.Load`, `routing.NewTable`'s default check, `provider.NewSet`'s
kind and key checks, and the unscanned-provider check in `gateway.New`.
None of these errors includes a key value.

Logging: the startup line per route has the name, kind, patterns and
default formats, never `api_key_env`'s value. The denial reason contains
the format and the truncated model only. `stripCallerCredentials` runs in
the providers' `Rewrite`, which is after every stage, so a stage that
logs headers still sees the caller's values. Policy input already drops
credential headers (`credentialHeaders` in `policy/input.go`). The new
`x-goog-api-key` header is added to that set.

## Testing

- `internal/routing`
  - `DetectFormat`: each rule in order; `anthropic-version` overrides an
    OpenAI path; Gemini suffix under a long prefix; trailing slash is
    unknown.
  - `ExtractModel`: string model; missing; non-string; empty; non-object
    body; invalid JSON; repeated top-level `model`; a nested `model` key
    is ignored; Gemini path model, with `%2570` matched as `%70` (AC 26).
  - `Truncate`: a 300-byte model is cut to at most 256 bytes on a UTF-8
    boundary; `Resolve` matches the full untruncated name.
  - `Resolve`: exact and prefix match; list order beats specificity
    (functional AC 11); format mismatch is 400 even with a default (AC 4);
    no model or no match goes to the default (AC 5, 7, 8); no default is
    404 (AC 6); unknown format is 404 (AC 9); single mode resolves
    everything.
  - `NewTable`: default naming an unknown provider; default of the wrong
    format (AC 19).
- `internal/config`: each validation rule; `provider` set in file plus
  `routing.providers` is an error (AC 13); `CARDEA_GATEWAY_PROVIDER_NAME`
  set plus `routing.providers` is an error (AC 25); defaults-only
  `provider` plus a list is accepted; `ErrorUnused` on a typo under
  `routing`.
- `internal/provider`: `stripCallerCredentials` removes all four headers
  and the `key` parameter and keeps other parameters; each provider sets
  only its own credential (AC 14); `NewGeminiProvider` input validation;
  `NewSet` rejects an unknown kind and an unset key.
- `internal/pipeline`: `AllowRouted` populates the call and cannot be
  overwritten; `RouteFrom` sees the route in the upstream handler; a
  pipeline with no routing stage forwards without panicking and with no
  route in the context; a `DenyWithModel` request record names the model,
  has no `provider` key, and has no response record (AC 18).
- `internal/gateway`, end to end with `httptest` upstreams (one per kind,
  plus a second Mistral upstream for AC 10) recording what they receive:
  - AC 1–3, 5, 7, 8, 10: the right upstream receives the call with its
    own credential, and the others receive nothing.
  - AC 4, 6, 9, 22: the right status, envelope and `stage: routing`, and
    no upstream receives anything.
  - AC 12, 24: single mode with a passthrough upstream forwards every
    path; single Anthropic forwards a repeated-`model` body as today.
  - Single mode with Mistral: a `/v1/messages` body containing an email
    is not redacted and policy input has no conversation groups, as
    today.
  - AC 15, 16, 23: startup error, warnings (with their wording about
    redaction and policy rules) captured with `zaptest/observer`, and an
    email forwarded unredacted to the Mistral upstream.
  - AC 17: both trace records and policy input name `mistral`.
  - AC 20: authorization denies before routing, and no routing denial is
    logged.
  - AC 28: in single mode, an authorization denial's request record has no
    `provider` key.
  - AC 27: rewrite the config file, call `ReloadPolicy`, and assert the
    next call still routes by the original list.
  - Dispatcher with no route in context responds 500 and forwards nothing.
  - AC 21: run each path with keys and caller credentials set to unique
    marker strings, capture logs, trace sink output and response bodies,
    and assert no marker appears in any of them.
- `internal/policy`, `internal/filtering`, `internal/trace`: existing
  tests updated to the new signatures; a non-Anthropic format yields only
  generic policy groups and no redaction; tracing reads tool results and
  usage for the single route whatever the format (single Mistral on
  `/v1/messages` keeps its `tool_results`, as today), for `anthropic` in
  list mode, and not for list-mode Mistral; a list-mode Mistral JSON
  response with an OpenAI-style `usage` has `tokens: null`; a 300-byte
  model is truncated in the request record in both modes; in list mode an
  OpenAI-style body `{"Model":"x"}` (wrong case, so routing finds no
  model and uses the default) records `model: null`;
  tracing sets `stream` for a Gemini streaming action.

## Alternatives considered

- **Model only, no format guard** — rejected: an Anthropic-format body for
  a Mistral model would reach Mistral and fail with an upstream error the
  caller cannot interpret.
- **URL format first, model second** — rejected: it cannot separate two
  providers of the same format, such as two OpenAI-style hosts.
- **One global default** — rejected by the functional spec: a default of
  one format cannot serve a request of another.
- **Path prefix or header selecting the provider** — rejected: a
  caller-facing selector, and clients would need different base URLs.
- **Routing in the dispatcher instead of a stage** — rejected: tracing,
  policies and filtering run before the upstream and need the provider.
- **Routing as an optional stage toggle** — rejected: single mode still
  needs a route for records and the dispatcher, so the stage is always on.
- **Stripping credentials in the dispatcher** — rejected: it would also
  strip them for the single-mode passthrough provider, changing its
  behavior (functional rule 4). Keeping the strip in each provider's
  `Rewrite`, next to where it sets its key, makes each provider
  self-contained.
- **Bedrock and Vertex in this spec** — rejected: signing, event streams
  and token refresh are separate designs (0010, 0011).

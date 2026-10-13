# 0006 — Filtering (technical)

**Date:** 2026-10-13
**Functional spec:** specs/0006-filtering-functional.md

## Context

The functional spec asks for a fourth pipeline stage that detects email
addresses, US/UK/France SSNs and API keys inside a request's conversation
text and tool calls/results, replaces each occurrence with a
`[category:UUID]` placeholder (never reusing a UUID), and writes the real
value to a pluggable cache for a later version to read back. Responses are
untouched; restoration is explicitly out of scope (specs/0006-functional,
*Non-goals*).

Nothing in the Gateway today rewrites a request body. Every existing
stage (`identification`, `tracing`, `policies`) only reads `pipeline.Call`
to decide allow/deny; none of them need to change what reaches the
provider. This is also the first feature that needs to walk and partially
rewrite the Anthropic Messages JSON shape rather than just read a view of
it — `internal/policy`'s `addAnthropicConversation` builds a read-only,
lossy projection for OPA, which is fine for a rule to read but would drop
unknown block fields (`cache_control`, `citations`, future block types)
if used to reconstruct a request.

## Goals

- A `filtering` stage, appended after `policies` in the enabled sequence,
  that rewrites `c.Body` (and the live `*http.Request`) in place before
  the call reaches the provider.
- Three fixed categories — `email`, `ssn` (US, UK, France combined),
  `api_key` (vendor-prefixed formats only; see *Non-goals*) — each
  independently enabled/disabled, on by default.
- A `Cache` interface with one shipped implementation, `MemoryCache`
  (TTL + FIFO eviction, a startup warning that it's volatile).
- Redact only within scope: `system`, each message's text/tool-call/
  tool-result content. Everything else — `thinking`/`redacted_thinking`
  blocks, `image` blocks, unrecognized block types, and every top-level
  field outside `system`/`messages` — passes through byte-identical.
- Fail the call closed on any detection or cache error, never forwarding
  a partially redacted or unredacted body.
- Keep the request's `Content-Length` consistent with the rewritten body
  so the upstream transport doesn't misread it.
- Nothing logged — at any level, on any path — contains a detected value,
  the text that matched, or a cache key tied to one.

## Non-goals

- Everything specs/0006-functional excludes: response rewriting/
  restoration, a Redis cache, categories beyond the three fixed ones,
  per-jurisdiction SSN toggles, operator-defined patterns, scanning
  headers or tool *definitions*.
- A public (non-`internal/`) package for `Cache`. It stays under
  `internal/filtering`, extended the way `trace.Sink` is today: by
  forking and adding an implementation, then wiring it into
  `buildStages` — not a runtime-loadable plugin.
- A generic, entropy-based "this looks like a secret" heuristic for
  `api_key`. Only fixed vendor-prefixed patterns ship in this version;
  see *Alternatives considered*.
- Redacting `thinking`/`redacted_thinking` blocks. Anthropic signs these;
  a later turn resends a prior assistant turn's thinking block verbatim
  as part of the conversation, and rewriting its text would invalidate
  that signature and break the call. They are always passed through
  untouched, even when they contain text matching a category.
- True LRU eviction for the in-memory cache. Nothing reads an entry back
  yet, so there's no access pattern to make recency-aware eviction
  meaningful to build or test; FIFO-past-capacity is enough.
- Byte-for-byte fidelity of the rewritten JSON. Every top-level field
  other than `system` and `messages` is copied as raw, untouched bytes.
  Inside `system` and `messages`, keys within a rewritten object are not
  guaranteed to keep their original order — JSON key order has no
  semantic meaning, and the Anthropic API does not depend on it.

## Design

```
request ─▶ pipeline.Handler ─▶ identification ─▶ tracing ─▶ policies ─▶ filtering ─▶ forward
                                                                          │
                                                       filtering.Engine.Redact
                                                                          │
                              r := filtering.Request{CallID, Path, Body: c.Body}
                              top, isTarget := buildTarget(r.Body)   (provider==anthropic, op in {messages,count_tokens})
                                          │
                       not a target / no category enabled ─▶ Result{Body: r.Body} unchanged, no error
                                          │
                              top["system"]   ─▶ redactSystem   (string, or blocks; only "text" blocks touched)
                              top["messages"] ─▶ redactMessages (per-message, per-block — see redactBlock)
                              every other top-level key        ─▶ copied as raw bytes, untouched
                                          │
                              body := json.Marshal(top)   ─▶ Result{Body: body, Counts: {category: n, ...}}
```

**Where it sits.** `buildStages` appends `filteringStage` right after
`policiesStage`, guarded by `cfg.Pipeline.Stages.Filtering` (default
`true`). A call a policy already denied never reaches it (specs/0001
rule 6, unchanged).

**Request only, and the first stage to rewrite one.** `filteringStage`
implements `pipeline.Stage` and nothing else — no `ResponseObserver`,
matching the functional non-goal that responses are untouched. Unlike
every stage so far, it doesn't just read `*pipeline.Call` to decide — it
mutates it: on a successful redaction it sets `c.Body`, `c.Request.Body`,
`c.Request.ContentLength` and the `Content-Length` header directly. No
change to `pipeline.Decision` or `pipeline.go` is needed for this:
`Request(ctx, c *pipeline.Call)` already receives `c` as a pointer, and
nothing downstream needs a generic "rewrite the body" hook since
filtering is the only caller (see *Alternatives considered*).

**One pipeline.go fix this causes.** `buildRequestRecord` currently
computes the trace record's `size_bytes` from `len(c.Body)` *after every
stage has run* (`pipeline.go`'s `Handler` calls it once per request, past
the stage loop). Once a stage can shrink or grow `c.Body`, that field
would silently start reporting the post-filtering size instead of what
the caller actually sent — a quiet change to an existing, already-shipped
field with no privacy angle either way, just a numeric drift. `Handler`
now captures the body's length right after reading it, before any stage
runs, and passes that to `buildRequestRecord` instead of re-measuring
`c.Body` later.

**Why filtering decodes and re-walks the body itself, not via
`internal/policy`.** `policy.addAnthropicConversation` builds a
*read-only* projection: a `tool_result`'s `content` becomes joined text
plus a byte count, an `image` block becomes `{"type":"image"}` with no
data, and any field the `block` struct doesn't declare (`cache_control`,
`citations`, a future block property) is silently dropped. That's exactly
right for a rule to read and exactly wrong for reconstructing a request —
filtering must put back an `image` block's base64 data unchanged and
every field it has never heard of. `internal/filtering` therefore has its
own decoder, built on `map[string]json.RawMessage` so unrecognized keys
are carried over as opaque bytes rather than lost. This repeats the same
"why not share" reasoning specs/0004 gave for not reusing tracing's
parser: the two packages read the same wire format for different
purposes and should stay free to diverge.

**Redaction is structure-aware, not a body-wide regex pass.** Only three
places are in scope, matching the functional spec's scope decision:

1. `system` — a string, or an array of blocks; only `type: "text"`
   blocks' `text` is touched.
2. Each message's `content` — a string, or an array of blocks, redacted
   by block `type` (see `redactBlock` below).
3. Nothing else. `model`, `tools`, `stream`, and every other top-level
   field are copied as raw `json.RawMessage` bytes and never decoded.

`redactBlock`'s per-type rules:

| Block `type` | What's redacted |
| --- | --- |
| `text` | Its `text` field. |
| `tool_use` | Its `input` field, walked recursively (`redactTree`) — arguments are arbitrary JSON with no fixed shape, so every string *value* at any depth is a candidate; map/array keys are never touched. |
| `tool_result` | Its `content` field, handled exactly like `system`: a string, or blocks where only `text`-typed ones are touched. |
| `thinking`, `redacted_thinking` | Nothing — passed through byte-identical (see *Non-goals*). |
| anything else (`image`, `server_tool_use`, a type this version doesn't know) | Nothing — passed through byte-identical. |

Every other key on a message or a block (`id`, `name`, `tool_use_id`,
`is_error`, `cache_control`, `citations`, …) is copied as raw bytes,
whether this package recognizes it or not.

**Non-overlapping matches, one fresh UUID per occurrence.** Within one
string, every enabled category's compiled patterns are tried; matches are
sorted by start offset (a tie broken by the longer match), and a match
that starts before the previous one ended is dropped as an overlap. Each
surviving match gets its own `newUUID()` — never reused, even for a value
that repeats verbatim elsewhere in the same string or request — written
to the cache as `Entry{Category, Value}` under that UUID, then replaced
with `[category:UUID]` in the output.

**No per-call state on `Engine`.** `Engine` is built once at startup and
shared across every concurrent request, so it cannot hold a request's
match counts as a field — that would race. Every helper
(`redactText`, `redactTree`, `redactBlock`, …) is a method on an
unexported `*scan`, one of which is created fresh per `Redact` call and
carries that call's `context.Context` and its `counts map[Category]int`
accumulator. `Engine` itself holds only what's immutable for the process:
the compiled patterns for each enabled category, the cache, and the
per-entry TTL.

**Gating, like `policy`'s.** Filtering only attempts to parse a body when
the provider is `anthropic` and the path is `/v1/messages` or
`/v1/messages/count_tokens` — the same two conditions
`policy.addAnthropicConversation` gates on, duplicated rather than
shared for the same reason the Anthropic reader itself is duplicated.
Outside that gate, or with every category disabled, `Redact` returns the
body unchanged and no error — not a failure, just nothing to scan.

**Content-Length.** The pipeline already fully reads the request body
into memory before any stage runs (`io.ReadAll(io.LimitReader(...))` in
`pipeline.go`), so there's no streaming request body to worry about.
After a redaction, `filteringStage.Request` sets
`c.Request.ContentLength = int64(len(res.Body))` — the field
`httputil.ReverseProxy` and `http.Transport` actually use when cloning
and sending the outbound request — and also sets the `Content-Length`
header on `c.Request`, belt-and-suspenders. A placeholder can be longer
than the PII it replaces, so the rewritten body can grow; that growth is
bounded by how many matches exist in a body `pipeline.max_body_bytes`
already capped on the way in, and is not re-checked against that limit —
the limit governs what the pipeline *reads*, not what it forwards.

## Packages and types

### `internal/filtering` — new

Files: `engine.go`, `anthropic.go`, `cache.go`, `memory_cache.go`,
`patterns.go`, `errors.go`, plus tests.

```go
// Category names a built-in PII category. Fixed in this version —
// operators enable or disable one, they cannot define a new one.
type Category string

const (
	CategoryEmail  Category = "email"
	CategorySSN    Category = "ssn"
	CategoryAPIKey Category = "api_key"
)

// Entry is one detected value, held by the cache under its placeholder's
// UUID. Value is the real PII text; nothing in the Gateway logs it.
type Entry struct {
	Category Category
	Value    string
}

// Cache is the pluggable store filtering writes detected values into, for
// a later version to read back. MemoryCache is the only implementation
// this version ships; a different one is added to this package and
// wired into buildStages, the way trace.Sink implementations are.
type Cache interface {
	// Set stores entry under key (the placeholder's UUID alone, not the
	// full "[category:uuid]" string) with the given time-to-live. An
	// error here fails the call closed — see Engine.Redact.
	Set(ctx context.Context, key string, entry Entry, ttl time.Duration) error
	// Get returns the entry for key. ok is false if it was never set, has
	// expired, or was evicted.
	Get(ctx context.Context, key string) (entry Entry, ok bool, err error)
	// Close releases any resources (goroutines, connections) the cache
	// holds.
	Close() error
}

// Request is what the pipeline hands the engine for one call.
type Request struct {
	CallID string
	Path   string
	Body   []byte
}

// Result is one Redact call's outcome. Counts is nil when nothing was
// redacted (not a target body, or nothing matched); Body equals the
// input body in that case.
type Result struct {
	Body   []byte
	Counts map[Category]int
}

// Engine detects and redacts PII in a request body. Safe for concurrent
// use: it holds no per-call state (see scan).
type Engine struct { /* unexported: patterns, cache, ttl */ }

// New builds an Engine from configuration, starting the configured cache
// backend (today: only "memory"; any other value is a config error, see
// internal/config). With every category disabled, the returned Engine's
// Redact is a no-op that never touches the cache.
func New(cfg config.FilteringConfig, providerName string, logger *zap.Logger) (*Engine, error)

// Redact scans r.Body for every enabled category and returns the
// rewritten body plus how many occurrences of each category were found.
// Redact recovers its own panics into a *FilterError{Kind: "internal"}
// so a panic value that might carry request content never reaches the
// pipeline's generic stage-panic recovery (which logs the panic's %v).
func (e *Engine) Redact(ctx context.Context, r Request) (Result, error)

// Close releases the cache.
func (e *Engine) Close() error

// FilterError classifies a Redact failure; it never carries request
// content, matching policy.EvalError.
type FilterError struct{ Kind string } // "input", "cache_write", "internal"

func (e *FilterError) Error() string
```

**`engine.go`.** `New`:

1. Builds the enabled pattern set from `cfg.Categories` and the
   package-level `categoryPatterns` (below).
2. Builds the cache: today only `cfg.Cache.Backend == "memory"` is valid
   (checked in `internal/config`, not here — see *Configuration*);
   `NewMemoryCache(cfg.Cache.Capacity)` is called and, on success, logs
   `logger.Warn("filtering cache is in-memory and will not survive a
   restart; any value cached for a future restoration feature is lost on
   restart")`.
3. Returns the `Engine{patterns, cache, ttl: cfg.Cache.TTL}`.

`Redact`:

1. If every category is disabled, return `Result{Body: r.Body}, nil`
   without inspecting the body at all.
2. `top, isTarget, err := buildTarget(providerName, r.Path, r.Body)`; a
   non-target body (wrong provider, wrong path, not JSON) returns
   `Result{Body: r.Body}, nil`; a target body that fails to parse (bad
   JSON, a duplicate key, `messages` not an array) returns
   `Result{}, &FilterError{"input"}`.
3. `sc := &scan{ctx: ctx, engine: e, counts: map[Category]int{}}`.
4. `newSystem, err := sc.redactSystem(top["system"])` when `system` is
   present; same error handling.
5. `newMessages, err := sc.redactMessages(top["messages"])`.
6. `top["system"], top["messages"] = newSystem, newMessages`; every other
   key of `top` is untouched.
7. `body, _ := json.Marshal(top)` (marshal of `map[string]json.RawMessage`
   cannot fail: every value is already valid JSON).
8. `len(sc.counts) == 0` (nothing matched): return `Result{Body: r.Body}`
   — the *original* bytes, since step 7's `body` differs from `r.Body`
   only in key order even when nothing was redacted, and returning it
   anyway would make "nothing detected" indistinguishable from "detected
   but happened to redact to the same text," which it isn't.
9. Otherwise return `Result{Body: body, Counts: sc.counts}, nil`.

A deferred `recover()` wraps steps 3–9: any panic becomes
`&FilterError{"internal"}`, logging nothing about the recovered value.

**`anthropic.go`.**

```go
// buildTarget reports whether body is an Anthropic messages or
// count_tokens request (provider == "anthropic" and path is one of the
// two conversation routes). Non-target bodies return isTarget == false,
// err == nil — not a failure, nothing to scan. A target body that is not
// valid JSON, repeats a key at any depth, or whose top level is not an
// object, or whose messages is not an array, is an *FilterError{"input"}.
func buildTarget(providerName, path string, body []byte) (top map[string]json.RawMessage, isTarget bool, err error)

// scan carries the state of one Redact call. Engine itself holds no
// per-call data, so this is what every helper method hangs off — see
// *Design*, "No per-call state on Engine."
type scan struct {
	ctx    context.Context
	engine *Engine
	counts map[Category]int
}

// redactSystem rewrites a system prompt — a JSON string, or an array of
// content blocks — redacting only "text"-typed blocks' text (or the
// whole string, if it's the plain-string form). Any other block type is
// returned byte-identical. nil when raw is absent.
func (s *scan) redactSystem(raw json.RawMessage) (json.RawMessage, error)

// redactMessages rewrites the messages array: each message's content —
// string or blocks — redacted via redactBlock. Every key on a message
// other than "content" is copied as raw bytes.
func (s *scan) redactMessages(raw json.RawMessage) (json.RawMessage, error)

// redactBlock redacts one content block by its "type", per the table in
// *Design*. Every key other than the one its type redacts ("text",
// "input" or "content") is copied as raw bytes, known or not.
func (s *scan) redactBlock(raw json.RawMessage) (json.RawMessage, error)

// redactTree walks v — the result of decoding a tool_use block's "input"
// with json.Number so integers round-trip — and redacts every string
// leaf. Map keys and non-string leaves (numbers, bools, null) are never
// touched.
func (s *scan) redactTree(v any) (any, error)

// rejectDuplicateKeys and operationName duplicate policy's own (package-
// private) versions rather than import internal/policy, for the same
// reason policy's own Anthropic reader isn't shared with tracing: the
// two stages read the same wire format for different purposes and
// should stay free to diverge.
func rejectDuplicateKeys(body []byte) error
func operationName(path string) string
```

**`patterns.go`.**

```go
// categoryPatterns holds every enabled category's compiled patterns,
// tried in the order listed; all are attempted, not just the first.
var categoryPatterns = map[Category][]*regexp.Regexp{
	CategoryEmail:  {reEmail},
	CategorySSN:    {reSSNUS, reSSNUK, reSSNFR},
	CategoryAPIKey: {reAPIKeyOpenAI, reAPIKeyGitHub, reAPIKeyAWS, reAPIKeySlack, reAPIKeyGoogle},
}

var (
	// reEmail: local-part@dotted-host, at least one dot in the host.
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+`)

	// reSSNUS: ###-##-####, excluding the area codes 000, 666 and 900-999
	// and the group 00, which the SSA never issues.
	reSSNUS = regexp.MustCompile(`\b(?:(?!000|666|9\d{2})\d{3})-(?:(?!00)\d{2})-(?:(?!0000)\d{4})\b`)

	// reSSNUK: a National Insurance Number. First letter excludes
	// D,F,I,Q,U,V; second excludes D,F,I,Q,U,V,O; six digits; suffix A-D.
	reSSNUK = regexp.MustCompile(`\b[A-CEGHJ-PRSTW-Z][A-CEGHJ-NPRSTW-Z]\d{6}[A-D]\b`)

	// reSSNFR: a French NIR — sex digit (1/2), YY, a valid MM, a
	// department code (01-95, 2A/2B for Corsica, or 971-976 overseas),
	// commune (3 digits), order (3 digits), 2-digit key. Groups may be
	// space-separated.
	reSSNFR = regexp.MustCompile(`\b[12]\d{2}(?:0[1-9]|1[0-2])\s?(?:0[1-9]|[1-8]\d|9[0-5]|2[AB]|97[1-6])\s?\d{3}\s?\d{3}\s?\d{2}\b`)

	// Vendor-prefixed secrets only — see Non-goals and Alternatives
	// considered for why no generic high-entropy pattern ships.
	reAPIKeyOpenAI = regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)
	reAPIKeyGitHub = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36}\b`)
	reAPIKeyAWS    = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reAPIKeySlack  = regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)
	reAPIKeyGoogle = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)
)
```

**`redactText` (on `*scan`), the leaf-level primitive every string
field goes through:**

```go
func (s *scan) redactText(str string) (string, error) {
	type match struct {
		start, end int
		category   Category
	}
	var matches []match
	for cat, patterns := range s.engine.patterns {
		for _, re := range patterns {
			for _, loc := range re.FindAllStringIndex(str, -1) {
				matches = append(matches, match{loc[0], loc[1], cat})
			}
		}
	}
	if len(matches) == 0 {
		return str, nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		return matches[i].end > matches[j].end // longer wins at the same start
	})

	var b strings.Builder
	last := 0
	for _, m := range matches {
		if m.start < last {
			continue // overlaps an already-consumed match
		}
		b.WriteString(str[last:m.start])
		id := newPlaceholderID() // crypto/rand-backed UUIDv4
		if err := s.engine.cache.Set(s.ctx, id, Entry{Category: m.category, Value: str[m.start:m.end]}, s.engine.ttl); err != nil {
			return "", &FilterError{Kind: "cache_write"}
		}
		b.WriteString("[" + string(m.category) + ":" + id + "]")
		s.counts[m.category]++
		last = m.end
	}
	b.WriteString(str[last:])
	return b.String(), nil
}
```

### `internal/gateway` — modified

New file `filtering.go`:

```go
type filteringStage struct {
	engine *filtering.Engine
	logger *zap.Logger
}

func (s *filteringStage) Name() string { return "filtering" }

func (s *filteringStage) Request(ctx context.Context, c *pipeline.Call) (pipeline.Decision, error)
```

`Request`:

1. `res, err := s.engine.Redact(ctx, filtering.Request{CallID: c.ID, Path: c.Request.URL.Path, Body: c.Body})`.
2. On error: classify via `errors.As` into `*filtering.FilterError` (kind
   `"internal"` if not one), log
   `s.logger.Error("filtering failed", zap.String("call_id", c.ID), zap.String("kind", kind))`,
   return `pipeline.Decision{}, err` — the pipeline turns that into the
   existing 503 `governance unavailable` denial (specs/0001), exactly as
   a policy evaluation failure does.
3. `len(res.Counts) == 0`: return `pipeline.Allow(), nil` — no detail, no
   log line, matching "a request with nothing detected adds no log line."
4. Otherwise:
   ```go
   c.Body = res.Body
   c.Request.Body = io.NopCloser(bytes.NewReader(res.Body))
   c.Request.ContentLength = int64(len(res.Body))
   c.Request.Header.Set("Content-Length", strconv.Itoa(len(res.Body)))
   ```
   Build `counts := make(map[string]int, len(res.Counts))` from
   `res.Counts`, log one line at info with the call id and, in the fixed
   order `email`, `ssn`, `api_key` (never map iteration order, so log
   lines and tests are stable), a field per category actually present:
   `s.logger.Info("request filtered", zap.String("call_id", c.ID), zap.Int("email", n), ...)`.
   Return `pipeline.Allow().WithDetail(trace.StageDetail{Redactions: counts}), nil`.

`gateway.go`: `buildStages` appends `&filteringStage{engine, logger}`
after the policies block when `cfg.Pipeline.Stages.Filtering` is true,
building the engine with
`filtering.New(cfg.Filtering, cfg.Provider.Name, logger)`. On error,
whatever was already opened (sink, policy engine) is closed first,
matching the existing cleanup order. `buildStages`'s return signature
gains the engine: `([]pipeline.Stage, trace.CloseableSink, *policy.Engine, *filtering.Engine, error)`.
`Gateway` gains a `filterEngine *filtering.Engine` field; `Run` closes it
on shutdown the same way it closes `sink`:
```go
if g.filterEngine != nil {
	defer func() { _ = g.filterEngine.Close() }()
}
```

### `internal/pipeline` — modified

`pipeline.go`, in `Handler`: right after the body is read and accepted
(`bodyRead = true; c.Body = body; ...`), capture
`requestSize := int64(len(body))`. This is the size the *caller* sent,
before any stage can change `c.Body`.

`records.go`: `buildRequestRecord` takes a new parameter,
`requestSize int64`, and sets `rec.SizeBytes = &requestSize` when
`bodyRead` instead of re-measuring `len(c.Body)`. Its call site in
`pipeline.go` passes the captured value.

### `internal/trace` — modified

`record.go`: `StageDetail` gains a field, and its doc comment is
loosened to describe a shared, per-stage container rather than a
policy-specific one:

```go
// StageDetail is what a stage reports about its own decision. Each field
// is populated by the stage that uses it; a stage that doesn't use a
// field leaves it at its zero value (empty string, or nil, which encodes
// as JSON null).
type StageDetail struct {
	Revision   string         `json:"revision"`
	Rules      []string       `json:"rules"`
	Redactions map[string]int `json:"redactions"`
}
```

A policy denial/allow leaves `Redactions` nil (encodes `null`); a
filtering detail leaves `Revision` `""` and `Rules` nil, for the same
reason.

## Configuration

```yaml
pipeline:
  stages:
    filtering: true # redacts PII before forwarding; see filtering below

filtering:
  categories:
    email: true
    ssn: true # US, UK and France formats, one category
    api_key: true
  cache:
    backend: memory # the only backend this version implements
    capacity: 100000 # entries held before the oldest (by insertion) is evicted
    ttl: 5m # how long a detected value is kept
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.stages.filtering` | bool | `true` | `CARDEA_GATEWAY_PIPELINE_STAGES_FILTERING` |
| `filtering.categories.email` | bool | `true` | `CARDEA_GATEWAY_FILTERING_CATEGORIES_EMAIL` |
| `filtering.categories.ssn` | bool | `true` | `CARDEA_GATEWAY_FILTERING_CATEGORIES_SSN` |
| `filtering.categories.api_key` | bool | `true` | `CARDEA_GATEWAY_FILTERING_CATEGORIES_API_KEY` |
| `filtering.cache.backend` | string | `"memory"` | `CARDEA_GATEWAY_FILTERING_CACHE_BACKEND` |
| `filtering.cache.capacity` | int | `100000` | `CARDEA_GATEWAY_FILTERING_CACHE_CAPACITY` |
| `filtering.cache.ttl` | duration | `5m` | `CARDEA_GATEWAY_FILTERING_CACHE_TTL` |

```go
type FilteringConfig struct {
	Categories CategoriesConfig  `mapstructure:"categories"`
	Cache      FilterCacheConfig `mapstructure:"cache"`
}

type CategoriesConfig struct {
	Email  bool `mapstructure:"email"`
	SSN    bool `mapstructure:"ssn"`
	APIKey bool `mapstructure:"api_key"`
}

type FilterCacheConfig struct {
	// Backend must be "memory" — the only value accepted this version.
	// "redis" is reserved for when specs/0006 gains that implementation.
	Backend  string        `mapstructure:"backend"`
	Capacity int           `mapstructure:"capacity"`
	TTL      time.Duration `mapstructure:"ttl"`
}
```

`Config` gains `Filtering FilteringConfig`; `StagesConfig` gains
`Filtering bool`. `defaults()` sets `Filtering: true` under
`Stages`, and `Filtering: FilteringConfig{Categories: {Email: true, SSN:
true, APIKey: true}, Cache: {Backend: "memory", Capacity: 100000, TTL: 5
* time.Minute}}`.

`Load()` registers every key above via `v.SetDefault`, and unmarshals
`filtering` with `ErrorUnused` like `policies` and `pipeline.stages`
(a typo'd key fails at startup). After unmarshalling, `Load` rejects:
`filtering.cache.backend` not equal to `"memory"` (naming the value and
that only `"memory"` is implemented this version); `filtering.cache.capacity
<= 0`; `filtering.cache.ttl <= 0`.

`configs/cardea-gateway.example.yaml` gets the `filtering` block above and
`filtering: true` under `pipeline.stages`.

## Failure modes

| Condition | Result |
| --- | --- |
| Every category disabled, or the stage disabled | `Redact` is a no-op (or never called); body forwarded exactly as received |
| Body is not an Anthropic `messages`/`count_tokens` request (other provider, other path, not JSON) | Not a target: body forwarded unchanged, no error, no detail |
| Target body is not valid JSON, repeats an object key at any depth, or `messages` is not an array | `*FilterError{"input"}`; stage returns the error; 503 `governance_unavailable`; provider receives nothing |
| `cache.Set` fails (a future backend unreachable, or the in-memory cache closed) | `*FilterError{"cache_write"}`; same 503 handling; the body already built up to that point is discarded, never partially forwarded |
| A panic during redaction | Recovered inside `Engine.Redact` itself into `*FilterError{"internal"}`, before the pipeline's own generic stage-panic recovery (which logs the panic's `%v` and could otherwise leak content) ever sees it |
| `filtering.cache.backend` is anything but `"memory"` | `config.Load` fails at startup, naming the value |
| `filtering.cache.capacity` or `.ttl` is zero or negative | `config.Load` fails at startup |
| A redaction occurs | Body rewritten; `c.Request.ContentLength` and the `Content-Length` header updated to match; one info log line with per-category counts; `trace.StageDetail.Redactions` set on the request record's `filtering` stage entry |
| Nothing is detected | Body, `ContentLength` and headers untouched; no log line; `trace.StageDetail` omitted (nil) |

**Requests fail closed**, matching every other stage (specs/0001,
specs/0004): a detection or cache failure denies the call with the
standard "governance unavailable" 503; the provider never receives the
original, unredacted body either. Filtering has no deliberate denial of
its own — it only ever allows (optionally with a rewritten body) or fails
closed.

**Nothing logged carries PII.** `FilterError` carries a kind only, never
matched text (unlike `policy.EvalError`, there's no parser-error-with-
file-position case here worth quoting, since there's no operator-authored
file to blame). The per-request log line carries counts per category,
never a value or a placeholder's UUID. `Entry.Value` exists only in the
cache, written once and read by nothing in this version.

## Testing

`internal/filtering` (table tests, no network):

- **Detection and replacement.** One matched email, one each of US/UK/FR
  SSN, one of each vendor-prefixed API key — each lands as its category's
  placeholder; a category disabled in config lets its own pattern's text
  through unredacted while others in the same string are still redacted;
  the same email appearing three times in one string gets three distinct
  UUIDs and three distinct cache entries; two overlapping matches (e.g. a
  US-SSN-shaped substring inside a longer API-key-shaped token) keep only
  the earlier/longer one per the merge rule.
- **Negative API-key case.** A 40-character random hex string with no
  vendor prefix is not matched — the decision this version makes instead
  of a generic entropy heuristic (*Non-goals*).
- **Scope.** A `text` block's `text` redacted; a `tool_use` block's
  `input` redacted at depth 3 inside a nested object and inside an array
  element, with map keys never touched even when a key name looks like an
  email; a `tool_result` block's `content` redacted both as a plain
  string and as blocks; a `thinking` and a `redacted_thinking` block
  containing an obvious match passed through byte-identical; an `image`
  block's base64 `data` passed through byte-identical; an unrecognized
  future block `type` passed through byte-identical; an unrecognized key
  on a known block type (`cache_control`, `citations`) preserved
  byte-identical; every top-level key other than `system`/`messages`
  (`model`, `tools`, `parameters`, `metadata`) preserved byte-identical
  even when it contains PII-shaped text.
- **Non-target bodies.** Provider not `"anthropic"`; path not
  `/v1/messages` or `/v1/messages/count_tokens`; non-JSON body on an
  unrelated path — all return unchanged with no error.
- **Failure.** A duplicate top-level key; a duplicate key nested inside a
  `tool_use`'s `input`; `messages` present but not an array — each gives
  `*FilterError{"input"}`. A `Cache` test double whose `Set` always
  errors gives `*FilterError{"cache_write"}`, and the caller never
  receives a body — not even a partially redacted one (assert `Result{}`
  on error). A redaction helper made to panic (test hook) is recovered
  into `*FilterError{"internal"}` with no panic value in `Error()`.
- **Cache (`MemoryCache`).** `Set`/`Get` round trip; an entry past its TTL
  is a miss on `Get` even before the sweep runs; inserting past capacity
  evicts the oldest *inserted* entry first (FIFO, not LRU — a `Get` on
  the second-oldest does not protect it from later eviction); `Close`
  stops the sweep goroutine (`goleak` or an explicit counter) and a `Set`
  after `Close` errors.
- **No content in outputs.** A marker string placed in `system` text, a
  `tool_use` argument three levels deep, and `tool_result` text: the
  marker appears in no `FilterError.Error()`, no log line captured via
  `zaptest/observer`, and nowhere in `Result.Body` — only as the `Value`
  of a `Cache.Get` the test performs directly, standing in for a future
  restoration consumer.

`internal/gateway`: `filteringStage` against a stub engine — a redaction
attaches `Redactions` and logs one line with per-category fields in fixed
order; zero redactions attaches no detail and logs nothing; an engine
error becomes the 503 through the real pipeline, and the fake upstream
never receives a call. End-to-end with a real `Engine` and an `httptest`
upstream that echoes back the `Content-Length` header and body length it
received: after a redaction, both match the *rewritten* body's length,
not the original request's.

`internal/pipeline`: a fake stage that shrinks `c.Body` proves
`buildRequestRecord`'s `size_bytes` still reports the length the caller
originally sent, not the post-stage size.

`internal/config`: defaults and env overrides for every new field;
`filtering.cache.backend: redis` (and any value besides `memory`) fails
naming it; non-positive `capacity`/`ttl` fail; an unknown key under
`filtering` fails.

End to end (one test, mirroring 0004's): default config, a prompt
containing an email — provider receives `[email:UUID]` in its place, and
the response is relayed byte-identical (functional criterion 1, 11); no
PII anywhere — byte-identical forward (criterion 5); the stage disabled —
PII of every category reaches the provider (criterion 7); a forced
cache-write failure — 503, upstream never called (criterion 10); PII only
in a `tool_use` argument and a `tool_result`, none in the top-level
`messages` text — both still redacted (criterion 12); a PII-shaped string
inside the `tools` list's `description` — left untouched (criterion 13).

## Alternatives considered

- **A single regex pass over the raw body bytes, with no JSON
  structure-awareness** — rejected: would also match inside `model`,
  tool schemas and other out-of-scope fields, and risks corrupting
  base64 image data or breaking JSON string escaping mid-match.
- **Re-marshalling the whole body through a generic `map[string]any`
  round trip** — rejected: silently drops any field this package's block
  handling doesn't know about (`cache_control`, `citations`, a future
  block property) everywhere in the body, not just inside the blocks
  being redacted, for no benefit over touching only `system` and
  `messages` and leaving the rest as raw bytes.
- **Redacting `thinking`/`redacted_thinking` blocks like any other text**
  — rejected per the interview for this spec: breaks Anthropic's
  signature verification the next time that block is resent.
- **A generic high-entropy secret heuristic for `api_key`** — rejected
  for this version: no reliable way to tell a real secret from an
  ordinary long ID, hash or UUID without more signal than this stage has,
  and a wrong call here means the real value never even reaches the
  cache for a human to later decide was fine.
- **True LRU eviction** — rejected for this version: nothing reads an
  entry back yet, so there's no access pattern to make recency-aware
  eviction meaningful to build, test, or justify over FIFO.
- **A public, non-`internal/` package for `Cache`** — rejected for now:
  every other extension point in this codebase (`trace.Sink`) is
  `internal/`-only and extended by forking; moving just this one
  interface out would be inconsistent, and can be revisited if a real
  external consumer shows up.
- **A generic `Decision.WithBody([]byte)` mechanism in `pipeline.go`,
  mirroring `WithDetail`** — rejected: filtering is the only stage that
  needs to rewrite a request, and it already holds a `*pipeline.Call` it
  can mutate directly; building a generic hook for a single caller is
  unneeded machinery that specs/0001 never anticipated and nothing else
  would use.
- **Sharing `policy`'s `rejectDuplicateKeys`/`operationName` instead of
  duplicating them** — rejected, for the same reason specs/0004 gave for
  not sharing its Anthropic reader with tracing: the two stages
  incidentally read the same wire format for unrelated reasons and should
  stay free to diverge independently.

## Open questions

- **The Redis cache.** `filtering.cache.backend: redis` is reserved but
  rejected at startup until that implementation exists; its own spec
  decides encryption-at-rest and the key-management story the functional
  spec already flags as a requirement.
- **Generic/entropy-based API key detection.** Deferred; worth revisiting
  once there's a corpus of real traffic to measure false-positive rate
  against, per the functional spec's own open question.
- **FIFO vs. LRU.** Revisit once a restoration feature makes cache reads
  a real, hot-path access pattern.
- **Per-jurisdiction SSN toggle.** Same open question as the functional
  spec; implementing it later means splitting `CategoriesConfig.SSN`
  into three booleans without changing the placeholder category name
  (`ssn` stays one category in the placeholder even if its three
  patterns become independently toggleable).

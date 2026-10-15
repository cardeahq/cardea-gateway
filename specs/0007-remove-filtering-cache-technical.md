# 0007 — Remove filtering cache (technical)

**Date:** 2026-10-15
**Functional spec:** specs/0007-remove-filtering-cache-functional.md
**Amends:** specs/0006-filtering-technical.md (see *Relationship to 0006*)

## Context

specs/0006-technical built `internal/filtering` around a pluggable
`Cache` interface, one implementation (`MemoryCache`), and a write on
every detected occurrence before the redacted body is returned —
entirely so a future, unbuilt restoration feature would have something
to read. The functional half (specs/0007-functional) removes that: no
detected value is written anywhere once its placeholder has been
substituted.

This is a deletion, not a redesign. `internal/filtering`'s JSON walking
(`anthropic.go`), its category patterns (`patterns.go`), the engine
shape, and the `filteringStage` wiring in `internal/gateway` are
unaffected in their structure — they lose the cache-shaped parts of
their current form and nothing else.

## Goals

- Delete `Cache`, `MemoryCache`, `Entry`, `NewMemoryCache`, and every
  reference to a cache from `Engine`, `New`, and `redactText`.
- Delete `filtering.cache.*` from `config.FilteringConfig` and from
  `Load`'s validation, defaults, and `configs/cardea-gateway.example.yaml`.
- Delete the `"cache_write"` `FilterError.Kind` and the startup warning
  log line about cache volatility.
- Keep every other exported type, every detection pattern, the
  placeholder format, and the JSON-walking logic in `anthropic.go`
  unchanged.
- `make check` passes with no reference to a filtering cache anywhere in
  `internal/filtering`, `internal/config`, `internal/gateway`, or
  `configs/cardea-gateway.example.yaml`.

## Non-goals

- Everything specs/0007-functional excludes: restoration, a Redis
  backend, pattern or scope changes, per-jurisdiction SSN toggles.
- Changing `anthropic.go`'s JSON-walking functions
  (`redactSystem`/`redactMessages`/`redactBlock`/`redactTree`/
  `redactTextOrTextBlocks`) beyond the error-return simplification that
  falls out of `redactText` no longer being able to fail (see *Design*).
  Their JSON-shape validation (`errNotTargetShape`, `errMessagesNotList`,
  `rejectDuplicateKeys`) is untouched.
- Changing `gateway.buildStages`'s signature or its stage ordering.
  `*filtering.Engine` is still threaded through exactly where it is
  today; only what it holds changes.

## Design

```
request ─▶ pipeline.Handler ─▶ identification ─▶ tracing ─▶ policies ─▶ filtering ─▶ forward
                                                                          │
                                                       filtering.Engine.Redact
                                                                          │
                              r := filtering.Request{CallID, Path, Body: c.Body}
                              top, isTarget := buildTarget(r.Body)   (unchanged)
                                          │
                       not a target / no category enabled ─▶ Result{Body: r.Body} unchanged, no error
                                          │
                              top["system"]   ─▶ redactSystem   (unchanged shape)
                              top["messages"] ─▶ redactMessages (unchanged shape)
                              every other top-level key        ─▶ copied as raw bytes, untouched
                                          │
                              body := json.Marshal(top)   ─▶ Result{Body: body, Counts: {category: n, ...}}
```

The only change inside this flow is at the leaf: `redactText` no longer
writes to anything before returning the placeholder, and therefore
cannot fail. Everything upstream of it (`buildTarget`, the per-block
walking in `anthropic.go`, the pipeline wiring in `internal/gateway`) is
structurally the same; only the cache-specific branches are deleted.

**`redactText` can no longer error.** Its only failure source in 0006
was `cache.Set`. With that call gone, matching every enabled category's
patterns, building the placeholder, and incrementing the count cannot
fail — so `redactText` drops its `error` return and becomes
`func (s *scan) redactText(str string) string`. Every caller in
`anthropic.go` that currently does
`redacted, err := s.redactText(str); if err != nil { return nil, err }`
drops the `err` check and uses `redacted := s.redactText(str)` instead.
This is the one ripple into `anthropic.go`; nothing else there changes,
since the other error returns in those functions come from JSON
decoding (`errNotTargetShape`, `errMessagesNotList`,
`json.Unmarshal`/`json.Marshal` failures), not from `redactText`.

**`Engine` no longer holds a cache or a TTL.** `Engine{patterns,
providerName}` — `cache Cache` and `ttl time.Duration` are deleted
fields. `New` no longer builds a cache, so it cannot fail for a reason
related to one, and no longer logs the startup warning. `Redact`'s
control flow (target check, `redactSystem`/`redactMessages`, counting,
returning the original bytes when nothing matched) is otherwise
identical to 0006's description, minus the cache-write error path.

**`Engine.Close` is removed.** 0006 gave `Engine` a `Close` purely to
release the cache's sweep goroutine. With no cache, there is nothing
left for `Engine` to release, so `Close` and the `internal/gateway`
code that calls it on shutdown are deleted rather than kept as a no-op —
consistent with CLAUDE.md's "no interfaces with a single implementation
for later" and "no speculative abstractions." *(Assumption: this is an
inferred simplification, not a question needing a human decision — it
follows mechanically from there being no resource left to close.)*

**`FilterError.Kind` drops `"cache_write"`.** Only `"input"` and
`"internal"` remain possible.

## Packages and types

### `internal/filtering` — modified

Files deleted entirely: `cache.go`'s `Cache` interface and `Entry` type;
`memory_cache.go`; their test files `memory_cache_test.go` and whatever
in `cache_test.go`/`engine_test.go` exercises `Cache`/`MemoryCache`
directly or a cache-write failure. `cache.go` today also declares
`Category` and its three constants and the `Request`/`Result` types
alongside `Cache`/`Entry` — since the file disappears, these three
non-cache declarations need a new home: `Category` and its constants
move to `patterns.go` (which already keys `categoryPatterns` by
`Category` and is the natural place for it), and `Request`/`Result`
move to `engine.go` (the only remaining file that constructs or returns
them). No behavior changes; this is purely where each declaration
lives.

```go
// Engine detects and redacts PII in a request body. Safe for concurrent
// use: it holds no per-call state (see scan).
type Engine struct {
	patterns     map[Category][]*regexp.Regexp
	providerName string
}

// New builds an Engine from configuration. With every category
// disabled, the returned Engine's Redact is a no-op.
func New(cfg config.FilteringConfig, providerName string, logger *zap.Logger) (*Engine, error)

// Redact scans r.Body for every enabled category and returns the
// rewritten body plus how many occurrences of each category were found.
// Redact recovers its own panics into a *FilterError{Kind: "internal"}
// so a panic value that might carry request content never reaches the
// pipeline's generic stage-panic recovery (which logs the panic's %v).
func (e *Engine) Redact(ctx context.Context, r Request) (Result, error)

// FilterError classifies a Redact failure; it never carries request
// content. Kind is one of "input", "internal" — "cache_write" no
// longer exists.
type FilterError struct{ Kind string }

func (e *FilterError) Error() string
```

`Category` (and its three constants), `Request`, `Result`,
`categoryPatterns` and every regular expression are unchanged in shape
from specs/0006-technical; only `Category`'s and `Request`/`Result`'s
file (see below) moves, since the file they lived in (`cache.go`) is
deleted.

`logger *zap.Logger` stays a parameter of `New` for signature
compatibility with `gateway.buildStages`'s call site, even though this
version of `New` no longer logs a startup warning; a future log line
(for example, one naming which categories are active) can still use it.

**`engine.go`.** `New`:

1. Builds the enabled pattern set from `cfg.Categories` and
   `categoryPatterns`, exactly as 0006 described.
2. Returns `&Engine{patterns: patterns, providerName: providerName},
   nil`. `New` cannot fail in this version — it is kept returning
   `(*Engine, error)` rather than changing its signature, since
   `gateway.buildStages` already handles an error from it and a future
   validation (for example, an empty pattern set check) may need to
   return one again.

`Redact`'s steps 1–9 are unchanged from 0006-technical except that step
4's call to `sc.redactSystem` and step 5's call to `sc.redactMessages`
can still return an error (JSON-shape failures), and step 7's
`redactText` calls inside them can no longer contribute one.

**`cache.go` deleted; `Request`/`Result` move to `engine.go`:**

```go
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
```

**`Category` moves to `patterns.go`:**

```go
// Category names a built-in PII category. Fixed in this version —
// operators enable or disable one, they cannot define a new one.
type Category string

const (
	CategoryEmail  Category = "email"
	CategorySSN    Category = "ssn"
	CategoryAPIKey Category = "api_key"
)
```

placed above the existing `categoryPatterns` map, which already keys
off `Category`.

**`anthropic.go`.** Unchanged except every call site of `redactText`
drops its error check, per *Design*. `scan` keeps its `ctx` field —
still threaded through for consistency with every other method's
signature and in case a future check needs it — but nothing inside
`redactText` reads it any more now that there is no `cache.Set(ctx, ...)`
call. *(Assumption: keeping `ctx` unused rather than removing it from
`scan` avoids reshaping every method signature for a field this spec
isn't asked to touch; if `go vet`/`staticcheck` flags it as unused, the
implementer drops it from `scan` and its construction site instead —
either is acceptable, this spec doesn't mandate one.)*

### `internal/gateway` — modified

`gateway.go`:

- `Gateway.filterEngine` field, `buildStages`'s `*filtering.Engine`
  return value, and the `filtering.New(cfg.Filtering, ...)` call site
  are unchanged in shape.
- The deferred `g.filterEngine.Close()` block in `Run` is deleted, since
  `Engine.Close` no longer exists (see *Design*).

`filtering.go` (`filteringStage.Request`) is unchanged: it already only
reads `res.Body`/`res.Counts`/the error's `Kind`, none of which change
shape.

### `internal/config` — modified

`config.go`:

- `FilteringConfig` loses its `Cache FilterCacheConfig` field and its
  doc comment's mention of a cache:

  ```go
  // FilteringConfig configures the filtering stage (specs/0006, amended
  // by specs/0007): which PII categories are detected.
  type FilteringConfig struct {
  	Categories CategoriesConfig `mapstructure:"categories"`
  }
  ```

- `FilterCacheConfig` type is deleted entirely.
- `CategoriesConfig` is unchanged.
- `defaults()`'s `Filtering: FilteringConfig{...}` literal drops its
  `Cache: FilterCacheConfig{...}` field.
- `Load()` drops the three `v.SetDefault("filtering.cache.*", ...)`
  calls and the three `cfg.Filtering.Cache.*` validation checks
  (`backend != "memory"`, `capacity <= 0`, `ttl <= 0`).
- The `discardFiltering` `ErrorUnused` decode (used to catch a typo'd key
  under `filtering`) is unchanged in mechanism, but now rejects
  `filtering.cache` and anything under it as an unrecognized key, since
  `FilteringConfig` no longer declares a `Cache` field — this is what
  delivers specs/0007-functional acceptance criterion 3.

### `configs/cardea-gateway.example.yaml` — modified

```yaml
# PII filtering: before a request reaches the provider, email addresses,
# SSNs (US/UK/France) and vendor-prefixed API keys are replaced with a
# [category:UUID] placeholder. Responses are never touched. On by
# default, with zero configuration needed.
filtering:
  categories:
    email: true
    ssn: true # US, UK and France formats, one category
    api_key: true
```

The `cache:` block (`backend`, `capacity`, `ttl`) and its comment
mentioning "the real value is written to the cache below" are removed.

## Configuration

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.stages.filtering` | bool | `true` | `CARDEA_GATEWAY_PIPELINE_STAGES_FILTERING` |
| `filtering.categories.email` | bool | `true` | `CARDEA_GATEWAY_FILTERING_CATEGORIES_EMAIL` |
| `filtering.categories.ssn` | bool | `true` | `CARDEA_GATEWAY_FILTERING_CATEGORIES_SSN` |
| `filtering.categories.api_key` | bool | `true` | `CARDEA_GATEWAY_FILTERING_CATEGORIES_API_KEY` |

`filtering.cache.backend`, `filtering.cache.capacity` and
`filtering.cache.ttl` no longer exist; setting any of them is an
unrecognized key and fails Gateway startup (see *Failure modes*).

## Failure modes

| Condition | Result |
| --- | --- |
| Every category disabled, or the stage disabled | `Redact` is a no-op (or never called); body forwarded exactly as received |
| Body is not an Anthropic `messages`/`count_tokens` request | Not a target: body forwarded unchanged, no error, no detail |
| Target body is not valid JSON, repeats an object key at any depth, or `messages` is not an array | `*FilterError{"input"}`; 503 `governance_unavailable`; provider receives nothing |
| A panic during redaction | Recovered inside `Engine.Redact` into `*FilterError{"internal"}` |
| A key is set under `filtering.cache` (any key) | `config.Load` fails at startup, naming the unrecognized key — `filtering.cache.backend`/`capacity`/`ttl` no longer exist to validate |
| A redaction occurs | Body rewritten; `c.Request.ContentLength` and the `Content-Length` header updated; one info log line with per-category counts; `trace.StageDetail.Redactions` set |
| Nothing is detected | Body, `ContentLength` and headers untouched; no log line; `trace.StageDetail` omitted (nil) |

**Requests still fail closed** on a detection error or panic, matching
specs/0007-functional's *Failure behavior*. The row 0006 had for
`cache.Set` failing is gone — there is no cache to fail. No other row
changes.

**Nothing logged carries PII.** Unchanged from 0006: `FilterError`
carries a kind only; the per-request log line carries counts per
category, never a value.

## Testing

`internal/filtering`:

- Every 0006 test case that does not involve `Cache`/`MemoryCache`
  carries over unchanged: detection and replacement per category, the
  negative API-key case, scope (text/tool_use/tool_result/thinking/
  image/unrecognized block types/unrecognized keys/non-`system`-or-
  `messages` top-level fields), non-target bodies, the `*FilterError{"input"}`
  cases (duplicate keys at any depth, non-array `messages`), the panic
  recovery into `*FilterError{"internal"}`, and the "no content in
  outputs" check (minus its one assertion that used to read the real
  value back via `Cache.Get`, which has nothing left to assert against).
- Deleted: `MemoryCache`'s `Set`/`Get`/TTL/FIFO-eviction/`Close` tests;
  the `Cache` test double that forces a `"cache_write"` error; the
  write-before-forward test that asserted a cache hit from inside the
  upstream handler (0006's acceptance criterion 8, removed by
  specs/0007-functional); the startup-warning test (0006's acceptance
  criterion 9, also removed).
- New: a table or assertion that `*FilterError{"cache_write"}` cannot be
  constructed by any path reachable from `Redact` — equivalently, that
  `redactText` has no error return any more (a compile-time property,
  not a runtime test, but worth a comment at the call sites in
  `anthropic.go` noting why they no longer check an error).
- New (functional criterion 5): `filtering.New`, asserted via
  `zaptest/observer`, logs no line at any level naming a cache backend
  or mentioning volatility — the inverse of 0006's deleted
  startup-warning test.

`internal/gateway`: `filteringStage` tests against a stub engine are
unchanged in shape (they already stub `Engine.Redact`'s return value,
not a cache); drop any assertion, if one exists, about
`filterEngine.Close()` being called on shutdown.

`internal/config`: defaults and env overrides for
`filtering.categories.*` are unchanged; the existing tests asserting
`filtering.cache.backend: redis` (and non-`memory` values) fails, and
that non-positive `capacity`/`ttl` fail, are deleted since those fields
no longer exist; a new or updated test asserts that setting
`filtering.cache.backend` (or any key under `filtering.cache`) now fails
as an *unrecognized* key rather than a bad value for a recognized one.

End to end: the existing 0006 end-to-end test (default config, a prompt
containing an email, PII in `tool_use`/`tool_result`, the stage
disabled, a PII-shaped string inside `tools`' `description`) carries
over unchanged, minus its forced-cache-write-failure case, which is
deleted since that condition no longer exists.

## Alternatives considered

- **Keeping `Cache`/`MemoryCache` in place but making `Engine` never
  call them** — rejected: dead code that still has to be built, tested,
  and kept compiling for no reason; CLAUDE.md's "no speculative
  abstractions" applies directly, and the functional spec asks for the
  mechanism removed, not merely unused.
- **Keeping `filtering.cache.backend: memory` as an accepted but
  ignored config key, for forward compatibility with whatever
  restoration ends up needing** — rejected: an operator who sets it
  today would reasonably believe it does something; silently ignoring a
  value is worse than failing on an unrecognized key, and a future
  restoration spec can reintroduce whatever config surface it actually
  needs rather than inherit this version's guess at it.
- **Giving `Engine.Close` a no-op body instead of deleting it** —
  rejected: nothing calls it for a reason any more, and an empty method
  kept "in case something needs it later" is exactly the speculative
  surface CLAUDE.md asks against; `buildStages`/`Run` already don't need
  a cleanup hook for `Engine` once there's nothing to clean up.

## Open questions

- **Restoration's future storage.** specs/0007-functional defers the
  same open question 0006 had, but notes its own cache-miss-fallback
  guidance no longer has a cache to apply to. Whoever designs
  restoration decides storage from scratch.

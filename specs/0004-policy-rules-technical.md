# 0004 — Policy rules (technical)

**Date:** 2026-10-06
**Functional spec:** specs/0004-policy-rules-functional.md

## Context

The functional spec asks for a policies stage that decides, from a Rego
policy file the operator writes, whether each request is forwarded; that
loads the file named in configuration; and that reloads it from disk on a
signal or a file change without dropping calls. Responses are out of
scope: they stay observe-only (specs/0001, specs/0003).

Today the pipeline has two stages (identification, tracing) and three
gaps this feature needs filled:

- No stage can add anything to its own entry in the request record. The
  record carries `decision` and timings only, so a stage cannot report the
  policy revision and the rules that fired (functional criterion 27).
- Denials carry no "do not retry" signal (specs/0001, rules 7 and 9, as
  amended), so SDKs resend a refused request.
- Nothing in the process reacts to `SIGHUP`, and no component owns state
  that changes at runtime. Every stage today is immutable after
  construction.

This spec adds one package, `internal/policy`, which owns everything that
touches OPA, the policy files and the input document, plus a thin stage in
`internal/gateway` that adapts it to the pipeline, and small changes to
`pipeline`, `trace`, `config` and `cli`.

## Goals

- A `policies` stage, third in the fixed order (identification, tracing,
  policies), that evaluates one Rego query per request and denies when it
  returns any rule identifier.
- Policy rules run in-process, embedded OPA, with no network and no
  access to the environment, filesystem or clock beyond OPA's own time
  built-ins.
- One input document, documented below, that does not depend on the
  provider's wire format. It is the contract operators write rules
  against.
- Load and compile the policy once, at startup, and keep the prepared
  query in memory: `Evaluate` does no file access, parsing or compilation,
  so per-request cost is the input build plus the query.
- A `Reload` function that rebuilds the whole policy from disk and swaps it
  in, triggered by `SIGHUP` and, optionally, by file change; atomic, all or
  nothing, never interrupting an evaluation.
- Evaluation failures deny with 503; deliberate denials return 403. Both
  follow specs/0001's denial contract.
- No argument value, prompt text or result text ever reaches a log, a
  record or an error message.

## Non-goals

- Everything the functional spec excludes: response rules, rewriting a
  request, state across calls, remote policy sources, a dry-run mode, a
  rule-testing tool, free-text deny messages.
- Provider formats other than the Anthropic Messages API. With another
  provider, the input still has the generic groups (call, agent, session,
  provider, operation, headers) and none of the conversation groups; the
  next provider's spec adds its reader.
- Reloading anything but policy and data files.
- Building the input as native OPA values to skip a JSON round trip. Worth
  doing only if profiling asks for it; see *Open questions*.

## Design

```
request ─▶ pipeline.Handler ─▶ identification ─▶ tracing ─▶ policies ─▶ forward
                                                              │
                                              policy.Engine.Evaluate
                                                              │
                              snapshot := engine.current.Load()   (once)
                              input    := buildInput(request)
                              snapshot.query.Eval(ctx, input)
                                          │
                       data.cardea.policy.deny  ─▶ set of rule ids
                                          │
                              empty ─▶ allow     non-empty ─▶ deny
```

**Where it sits.** `buildStages` appends policies after tracing. Tracing
before policies gives rules the session (`Call.Trace.Session`) and costs
nothing on denial: the request record is still written, with policies as
`denied`. If an earlier stage denies, policies is `not_run`.

**Request only.** The stage implements `pipeline.Stage` and nothing else.
It implements no `ResponseObserver`: policies never see a response
(functional non-goal).

**The engine and its snapshot.** `policy.Engine` is the only
long-lived mutable object in the feature. It holds one atomic pointer to
an immutable `snapshot` (the prepared query plus the revision).
`Evaluate` loads the pointer once and uses that snapshot for the whole
evaluation, which gives functional rule 22 (an evaluation finishes under
the policy it started with) with no locks on the hot path. `Reload` builds
a complete new snapshot, and only on success stores it.

**Rules and entry point.** Operators write rules in the Rego package
`cardea.policy`, in one file. The entry point is the set `deny`:

```rego
package cardea.policy

deny contains "no-delete-tool" if {
    some t in input.tools
    t.name == "delete_database"
}

deny contains "beta-small-model-only" if {
    input.agent.id == "agent-beta"
    input.model != "claude-haiku-4-5"
}
```

The query is `data.cardea.policy.deny`. Results: undefined (no `deny`
rules, or none fire) or an empty set is allow; a non-empty set of strings
is the rule identifiers; anything else is a malformed decision (a
failure). The file may declare other packages as helpers.

**Failure of rules is not failure of evaluation.** OPA's non-strict
built-in errors (a regex on a missing field, a type mismatch) make the
expression undefined, so that rule does not fire. That is exactly
functional rule 12 and the "rules that simply do not match" bullet, and it
is why the engine does *not* enable strict built-in errors.

**Why an Anthropic reader lives in `policy`, not `trace`.** Tracing reads
only metadata (tool-result ids, sizes) and must never keep content. The
policy input needs content. Sharing one parser would put content-carrying
types next to code that promises not to hold any, so the policy package has
its own reader, and the two stay free to change independently.

## Packages and types

### `internal/policy` — new

Files: `engine.go`, `load.go`, `input.go`, `anthropic.go`, `watch.go`,
`errors.go`, plus tests.

```go
// Request is what the pipeline hands the engine for one call. It holds
// only what the policies stage can see; the engine builds the input
// document from it.
type Request struct {
	CallID    string
	Method    string
	Path      string
	Headers   http.Header // credentials are stripped by the engine, not the caller
	Body      []byte
	AgentID   string // "" when not identified
	SessionID string // "" when tracing found none (status "claimed" only)
}

// Result is one evaluation's decision. Rules is sorted and unique; empty
// means allow. Revision names the policy the evaluation ran under.
type Result struct {
	Revision string
	Rules    []string
}

// Engine evaluates requests and owns the policy lifecycle.
type Engine struct { /* unexported */ }

// New loads and checks the configured policy file and data files. With no
// policy path configured it returns an Engine that allows everything.
// A bad policy is an error naming the file and position.
func New(cfg config.PoliciesConfig, providerName string, logger *zap.Logger) (*Engine, error)

// Evaluate decides one request under the current snapshot. The returned
// error is always an *EvalError.
func (e *Engine) Evaluate(ctx context.Context, r Request) (Result, error)

// Reload rebuilds the policy from disk. trigger is "signal" or "watch"
// and only appears in the log. On error the previous snapshot stays.
func (e *Engine) Reload(trigger string) error

// Revision is the revision in force, "" when no policy is configured.
func (e *Engine) Revision() string

// Watch reloads when policy or data files change, until ctx ends. It
// returns immediately, nil, when cfg.Watch is false.
func (e *Engine) Watch(ctx context.Context) error

// EvalError never carries OPA's message: only a kind, so nothing derived
// from a request can reach a log or a record.
type EvalError struct{ Kind string } // "timeout", "overloaded", "internal", "malformed_decision", "input"

func (e *EvalError) Error() string
```

**Loading (`load.go`).** `loadSnapshot(cfg, logger)`:

1. Read the file at `cfg.Path`; a path that does not exist or is not a
   regular `.rego` file is an error naming it. Walk each path in `cfg.Data`
   (a file, or a directory recursively), keeping `.json`, `.yaml`, `.yml`
   files sorted by path.
2. Revision = first 12 hex characters of SHA-256 over the policy file's
   path and bytes, then each data file's path and bytes, each separated by
   a zero byte. It is a function of contents and names, not of
   modification time.
3. Parse the policy with `ast.ParseModule(path, string(bytes))` (Rego v1
   syntax). Parse each data file with `util.Unmarshal` (JSON or YAML) into
   a `map[string]any`; a top-level value that is not an object is an
   error. Data objects are merged at the root of `data`. Two files
   defining the same top-level key, or the key `cardea`, is an error: data
   must not shadow the rules' package.
4. Build capabilities from `ast.CapabilitiesForThisVersion()` minus the
   built-ins that reach outside the process: `http.send`,
   `net.lookup_ip_addr`, `opa.runtime`. `print` stays disabled (OPA's
   default: print statements are dropped unless enabled), since it would
   write input values to a log. A rule using a removed built-in fails
   compilation with OPA's own "unknown function" error naming file and
   position.
5. `rego.New(rego.Query("data.cardea.policy.deny"),
   rego.ParsedModule(module), rego.Store(inmem.NewFromObject(data)),
   rego.Capabilities(caps)).PrepareForEval(ctx)`.
6. Smoke-evaluate the prepared query once with an empty input and check
   the shape of the result (undefined, or a set of strings). This catches
   `deny` defined as the wrong type, and complete-rule conflicts that do
   not depend on the input, at load time and not on the first request.
7. Check that the module's package is `cardea.policy` and that it defines
   at least one rule whose head is `deny`. If not, loading fails with an
   error naming the file ("policy defines no `deny` in package
   cardea.policy; it could never deny"). The likeliest cause is a typo'd
   package name, which Rego would otherwise treat as an empty policy that
   allows everything. A file that only defines helpers is rejected too: with
   one policy file, it could never contribute a denial.

Parse and compile errors are returned as OPA reports them (file, row,
column, message). They quote the *policy's* text, never a request's, so
they may be logged.

```go
type snapshot struct {
	query    rego.PreparedEvalQuery
	revision string
	files    int // data files plus the policy file
}
```

**Evaluation (`engine.go`).** `Evaluate`:

1. `snap := e.current.Load()`; nil means no policy: return the zero
   `Result{}` immediately, without building the input.
2. `input, err := buildInput(e.provider, r)`; an error is an `*EvalError`
   of kind `"input"`.
3. If `e.abandoned.Load() >= maxAbandoned` (16, a constant), return
   `*EvalError{overloaded}` without starting an evaluation.
4. `ctx, cancel := context.WithTimeout(ctx, e.timeout)`; `defer cancel()`.
   Run the query in a goroutine and wait for the first of its result and
   the deadline:

   ```go
   type outcome struct {
   	rs  rego.ResultSet
   	err error
   }
   done := make(chan outcome, 1) // buffered: an abandoned goroutine can still send and exit
   go func() {
   	defer func() {
   		if recover() != nil { // never log the panic value: it can hold input
   			done <- outcome{err: errEvalPanic}
   		}
   	}()
   	rs, err := e.eval(ctx, snap.query, input) // snap.query.Eval, via a test seam
   	done <- outcome{rs, err}
   }()
   select {
   case o := <-done:
   	// continue with o.rs, o.err
   case <-ctx.Done():
   	e.abandoned.Add(1)
   	go func() { <-done; e.abandoned.Add(-1) }() // counts until the goroutine ends
   	// deadline: kind "timeout"; the caller's own cancellation: kind "internal"
   }
   ```

   `e.eval` is an unexported field defaulting to `PreparedEvalQuery.Eval`;
   tests replace it to inject a panic, a block or an error. A recovered
   panic is kind `"internal"` (never the panic value, which can contain
   request data), and the abandoned counter still falls back because the
   goroutine ends through the same path.

   `cancel` runs as soon as `Evaluate` returns, so OPA stops the abandoned
   evaluation at its next step; only a single built-in call that is already
   running holds the goroutine until it ends. A context deadline error
   from `Eval` itself is also kind `"timeout"`; any other error is
   `"internal"`.
5. Read the result: no result set is allow, with `Result{Revision:
   snap.revision}`. Otherwise `rs[0].Expressions[0].Value` must be `[]any`
   of `string`, else kind `"malformed_decision"`.
6. Normalize the rule ids: a string matching
   `^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$` is kept; any other is replaced by
   `invalid-rule-id` (the call is still denied, because the rule fired).
   This is a best-effort guard against free text, not a guarantee: a short
   id built from the input (`sprintf("no-%s", [input.x])` with a one-word
   value) still passes, and writing fixed ids is the operator's
   responsibility (functional spec, *What the operator sees*). Every fired
   id is kept, sorted and unique: there is no cap on how many are recorded
   (rule 10), since a policy of fixed ids can fire no more rules than it
   defines.

**Why a goroutine.** OPA cancels a running evaluation when its context
ends, but it checks between evaluation steps, not inside a single
built-in call, so a heavy built-in (for example a regex over a very large
string) would hold `Evaluate`, and with it the request, past the limit.
Waiting for the result or the deadline in the stage, rather than inside
OPA, makes the limit hold for the caller whatever the rule does. What is
left is CPU, not latency: an abandoned evaluation runs until its built-in
returns, and `maxAbandoned` bounds how many may do so at once. A new
evaluation past the bound fails immediately (`overloaded`) rather than
adding to it.

**Cached, not loaded per request.** `New` calls `loadSnapshot` once, and
the resulting `snapshot` (parsed modules, compiled and prepared queries,
data store) lives in the engine until `Reload` replaces it. `Evaluate` only
loads the snapshot pointer and calls the prepared queries; it never opens a
file, never parses Rego, and never compiles. Editing a file on disk changes
nothing until `Reload` runs.

**Reload.** `Reload` is the one function that rebuilds the policy, and the
only caller of `loadSnapshot` after startup. It takes a mutex (reloads are serialized, evaluations
are not blocked), calls `loadSnapshot`, and compares revisions. Unchanged
revision: log at debug as "no change", keep the snapshot. Different
revision: store the new snapshot, log at info with old and new revision,
trigger and file count. Error: log at error with trigger, the current
revision, and the error; keep the old snapshot; return the error. A path
that vanished is an error like any other (functional rule 24).

**Watching (`watch.go`).** `Watch` uses `fsnotify`, already in `go.mod` as
an indirect dependency of Viper, which becomes direct. It watches the
parent directory of each configured file and each configured directory and
its subdirectories (fsnotify is not recursive; new subdirectories are added
on `Create`). Watching the parent directory, not the file, is what keeps
editors that save by rename-and-replace from silently ending the watch. An
event counts only if the file's extension is one the loader keeps, or the
event is on a configured path itself. Counted events (re)arm a single
`time.AfterFunc(cfg.WatchSettle, …)`; when it fires, `Reload("watch")`
runs once. `Watch` returns when `ctx` is done, closing the watcher.

### `internal/policy` — the input document

`buildInput(provider string, r Request) (map[string]any, error)` returns
the document below, the contract the functional spec describes for rule
authors (rule 11, key by key). A key whose value does not apply or is
unknown is **omitted**, never `null` or `""`; a list the Gateway knows is
empty is **present and empty** (functional rule 12): for a conversation
operation, `messages`, `tools`, `tool_calls` and `tool_results` are always
`[]` at least, and `parameters` is always `{}` at least, while `system` is
omitted when there is no system prompt.

```jsonc
{
  "call":      { "id": "ak_7f3c2b1d9e8a4f60", "size_bytes": 48213,
                 "headers": { "x-team": ["blue"] } },   // lower-case names; lists
  "agent":     { "id": "acme-bot" },                    // omitted when not identified
  "session":   { "id": "8d0c7a3e-…" },                  // omitted unless claimed
  "provider":  "anthropic",
  "operation": { "name": "messages", "method": "POST", "path": "/v1/messages" },
  "model":     "claude-sonnet-5",
  "stream":    true,
  "parameters":{ "max_tokens": 4096, "temperature": 0.2 },
  "system":    "…",                                     // text, blocks joined with "\n"
  "messages": [
    { "role": "user", "content": [
        { "type": "text", "text": "…" },
        { "type": "tool_result", "tool_call_id": "toolu_01A9", "is_error": false,
          "size_bytes": 5120, "text": "…" } ] },
    { "role": "assistant", "content": [
        { "type": "tool_call", "id": "toolu_01B2", "name": "bash",
          "arguments": { "command": "ls" } } ] }
  ],
  "tools":        [ { "name": "bash", "description": "…", "parameters": { /* JSON schema */ } } ],
  "tool_calls":   [ { "id": "toolu_01B2", "name": "bash", "arguments": { "command": "ls" } } ],
  "tool_results": [ { "tool_call_id": "toolu_01A9", "tool_name": "bash", "is_error": false,
                      "size_bytes": 5120, "text": "…" } ]
}
```

Rules for building it:

- **`call.headers`**: lower-cased names, values as lists, with
  `authorization`, `proxy-authorization`, `x-api-key`, `api-key`, `cookie`
  and `set-cookie` removed (the list functional rule 11 names), plus
  `x-gateway-request-id`, which the pipeline already strips from
  inbound requests. The removal list is a constant in the package, with a
  test that fails if any of them can reach the input.
- **`operation.name`** from the path: `/v1/messages` is `messages`,
  `/v1/messages/count_tokens` is `count_tokens`, anything else `other`. The
  raw method and path are always there.
- **Conversation groups** (`model`, `stream`, `parameters`, `system`,
  `messages`, `tools`, `tool_calls`, `tool_results`) are built only when the
  provider is `anthropic` and the operation is `messages` or
  `count_tokens`. For those operations a body that is not valid JSON,
  that repeats a key in any object at any depth, or whose `messages` is
  not an array, is `EvalError{Kind: "input"}` (fail closed, functional
  failure list). For any other operation the groups are absent and the
  body is not parsed.
- **`parameters`** is every top-level field of the request other than
  `model`, `messages`, `system`, `tools` and `stream`, unchanged
  (`max_tokens`, `temperature`, `metadata`, `tool_choice`, `thinking`, …).
- **Content blocks** are normalized: a string `content` becomes one `text`
  block. `text` stays `text`. `tool_use` becomes `tool_call` with its
  `input` as `arguments`. `tool_result` becomes `tool_result`, its content
  reduced to `text` (text parts joined with `"\n"`) and `size_bytes` (the
  byte length of that text). `image` becomes `{"type":"image"}` with no
  data (a base64 image is megabytes of no use to a rule). Any other type
  (for example `thinking`) becomes `{"type": "<its type>"}` with no payload.
- **`tool_calls`** lists every `tool_call` block in the conversation, in
  order. **`tool_results`** lists every `tool_result` block, in order, with
  `tool_name` taken from the `tool_call` of the same `tool_call_id` when
  that call is in this request; omitted otherwise. Both are views of the
  same data as `messages`, offered because rules are easier to write
  against flat lists.
- **`tools`** maps each Anthropic tool to `name`, `description` (omitted if
  absent) and `parameters` (its `input_schema`). A server tool without a
  schema carries its `type` instead of `parameters`.

`anthropic.go` holds the unexported structs that read the request and the
functions that produce these shapes. Before decoding, `rejectDuplicateKeys(body
[]byte) error` walks the body once with `json.Decoder` tokens, keeping a
key set per open object, and returns an error at the first repeated key
(its message names the byte offset, never the key or any value). Go's
`json.Unmarshal` keeps the last of a repeated key and a provider may keep
the first, so a rule and the provider could read different values from
one body; rejecting the body removes the disagreement. The walk is linear
in the body size and runs only for the operations whose body the policy
reads. The body is then decoded with `json.Unmarshal` into
structs with `json.RawMessage` for the open parts, then builds
`map[string]any` trees. Numbers keep `json.Number` so large integers
survive the trip into OPA.

### `internal/gateway` — modified

New file `policies.go`:

```go
type policiesStage struct {
	engine *policy.Engine
}

func (s *policiesStage) Name() string { return "policies" }

func (s *policiesStage) Request(ctx context.Context, c *pipeline.Call) (pipeline.Decision, error)
```

`Request`:

1. Build a `policy.Request` from the call: `CallID` from `c.ID`; method,
   path, headers and body from `c.Request` and `c.Body`; `AgentID` from
   `c.AgentID`; `SessionID`
   from `c.Trace.Session` when its status is `claimed`, else empty
   (`c.Trace` may be nil when tracing is off).
2. `res, err := s.engine.Evaluate(ctx, req)`. On error, log at error level
   with the call id, the error kind and `s.engine.Revision()`, and return
   `pipeline.Decision{}, err`. The pipeline turns that into the 503
   `gateway_unavailable` denial (specs/0001).
3. `detail := trace.StageDetail{Revision: res.Revision, Rules: res.Rules}`
   (a nil `Rules` becomes `[]string{}` so it encodes as `[]`); when
   `res.Revision == ""` (no policy configured) the detail is omitted.
4. No rule fired: `return pipeline.Allow().WithDetail(detail), nil`.
5. Otherwise return
   `pipeline.Deny(http.StatusForbidden, "request denied by policy", reason).WithDetail(detail)`,
   where `reason` is `"rules=" + strings.Join(res.Rules, ",") + " revision=" +
   res.Revision`. The pipeline logs that `reason` at warn, as it does for
   every denial; it is built only from the normalized rule ids and the
   revision.

`gateway.go`: `buildStages` appends `&policiesStage{engine}` after tracing
when `cfg.Pipeline.Stages.Policies` is true. The engine is created there
with `policy.New(cfg.Policies, cfg.Provider.Name, logger)`; its error
fails `gateway.New`, so `serve` exits before listening. `buildStages` also
logs the warning for functional rule 15 when a policy is configured and
identification is off. `Gateway` keeps the engine, and gains:

```go
// ReloadPolicy reloads the policy from disk (the SIGHUP path). With no
// policy configured it logs that and returns nil.
func (g *Gateway) ReloadPolicy() error
```

`Run` starts `engine.Watch(ctx)` in a goroutine that ends with `ctx`.

`buildStages` now returns the engine too, so its signature becomes
`([]pipeline.Stage, *trace.LogSink, *policy.Engine, error)`.

### `internal/pipeline` — modified

`pipeline.go`:

```go
// WithDetail returns d with detail attached; it is carried to the stage's
// entry in the request record, for allows and denies alike.
func (d Decision) WithDetail(detail trace.StageDetail) Decision
```

`Decision` gains an unexported `detail *trace.StageDetail`. `stageResult`
gains `detail *trace.StageDetail`, set from the decision for `allowed` and
`denied` results. The existing "did a stage return a bare `Decision{}`"
protection is unchanged.

`records.go`: `buildRequestRecord` copies `r.detail` into
`RequestStage.Detail`.

`pipeline.go`: the `call denied` warning gains an `agent_id` field when
`c.AgentID != ""` (functional *What the operator sees*: the denial line
names the agent when identified). It never carries the credential, only the
registered id identification resolved.

`errors.go`: `writeDenial` sets `X-Should-Retry: false` when `unavailable`
is false, and no `X-Should-Retry` header when it is true. This covers every
deliberate denial, including identification's 401 and the pipeline's own
400 and 413 (specs/0001, as amended).

### `internal/trace` — modified

`record.go`: `RequestStage` gains `Detail *StageDetail` and the new
`StageDetail` type, as in the amended specs/0003.

### `internal/config` — modified

```go
type PoliciesConfig struct {
	// Path is the .rego policy file. Empty: no policy.
	Path string `mapstructure:"path"`
	// Data are JSON or YAML files, or directories of them, exposed to
	// rules as `data`.
	Data []string `mapstructure:"data"`
	// Watch turns on reload on file change.
	Watch bool `mapstructure:"watch"`
	// WatchSettle is how long to wait after the last change before
	// reloading.
	WatchSettle time.Duration `mapstructure:"watch_settle"`
	// EvalTimeout is the longest one evaluation may take.
	EvalTimeout time.Duration `mapstructure:"eval_timeout"`
}
```

`Config` gains `Policies PoliciesConfig`; `StagesConfig` gains
`Policies bool`.

### `internal/cli` — modified

`serve.go` registers `SIGHUP` with `signal.Notify` on a buffered channel
and starts a goroutine that calls `gw.ReloadPolicy()` for each signal
until the serve context ends. Registering is required even with no policy,
because SIGHUP's default action ends the process; with no policy the call
logs "no policy configured" and does nothing.

## Configuration

```yaml
pipeline:
  stages:
    policies: true          # in the path; allows everything until path is set

policies:
  path: ""                  # the .rego policy file; "" = no policy
  data: []                  # .json / .yaml files or directories
  watch: false
  watch_settle: 500ms
  eval_timeout: 100ms
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.stages.policies` | bool | `true` | `CARDEA_GATEWAY_PIPELINE_STAGES_POLICIES` |
| `policies.path` | string | `""` | `CARDEA_GATEWAY_POLICIES_PATH` |
| `policies.data` | list of string | `[]` | `CARDEA_GATEWAY_POLICIES_DATA` (comma-separated) |
| `policies.watch` | bool | `false` | `CARDEA_GATEWAY_POLICIES_WATCH` |
| `policies.watch_settle` | duration | `500ms` | `CARDEA_GATEWAY_POLICIES_WATCH_SETTLE` |
| `policies.eval_timeout` | duration | `100ms` | `CARDEA_GATEWAY_POLICIES_EVAL_TIMEOUT` |

Every key is registered in `defaults()` and with `SetDefault` in `Load()`
(an unregistered key would unmarshal to its zero value; for
`pipeline.stages.policies` that is `false`, silently pulling the stage out
of the path, the same trap specs/0001 records). `policies` is unmarshalled
with `ErrorUnused`, like `pipeline.stages` and `agents`, so a typo'd key
fails at startup. `Load` rejects `eval_timeout <= 0` and `watch_settle <=
0`. A `policies.path` that does not exist is rejected by
`policy.New` (not by `Load`), since loading needs the filesystem anyway.

The existing `rules.path` key is left as it is: still unused, still in the
example file. It belongs to the circuit-breaker and compaction rules its
comment describes; this feature does not touch it. `configs/cardea-gateway.example.yaml`
gets the `policies` block above and `policies: true` under
`pipeline.stages`, with a note that the stage allows everything until
`path` is set.

## Failure modes

| Condition | Result |
| --- | --- |
| No `policies.path` configured | The stage allows every call; no input is built; `detail` omitted |
| The policy file or a data file is missing, unreadable, does not parse or compile, uses a removed built-in, or defines `deny` with a non-set type | `policy.New` fails; `gateway.New` fails; `serve` exits before listening, with file and position |
| Two data files define the same top-level key, or a data key equals a package root | Same: startup fails naming both files |
| The policy does not declare `package cardea.policy`, or defines no `deny` | `policy.New` fails and `serve` exits before listening, naming the file; on reload, the old snapshot stays |
| Reload fails, for any reason above, or a watched path vanished | Old snapshot stays; error logged with trigger, revision and position; `Reload` returns the error |
| Evaluation exceeds `eval_timeout` | `*EvalError{timeout}` at the deadline, even if the evaluation cannot be interrupted at once; stage returns it; 503 `gateway_unavailable`, no `X-Should-Retry`; error log with kind and revision |
| 16 abandoned evaluations are still running | `*EvalError{overloaded}` immediately; same 503 handling; the log names the kind |
| OPA returns an error, or the result is not a set of strings | `*EvalError{internal}` or `{malformed_decision}`; same handling |
| `messages` or `count_tokens` body is not valid JSON, repeats a key in any object, or `messages` is not an array | `*EvalError{input}`; same handling |
| A rule id does not match the id pattern | Denied anyway; that id is replaced by `invalid-rule-id` |
| Evaluation panics | Recovered in the evaluation goroutine itself, into `*EvalError{internal}` and the same 503. The pipeline's own recover covers only the stage's goroutine, and an unrecovered panic in another goroutine ends the process |
| `SIGHUP` with no policy configured | Logged at info; nothing changes |
| `fsnotify` cannot watch a path | `Watch` returns an error; logged at error; the Gateway keeps serving, and signal reload still works |

**Requests fail closed, reloads fail safe**, as the functional spec
requires. The watcher failing to start is the one place the Gateway does
not stop: a broken watcher must not take enforcement offline, and the
operator has the signal.

**Nothing logged or recorded carries call content.** `EvalError` carries a
kind only, never OPA's text. OPA error text can include rule locations and,
for some built-in failures, values from the input; it is discarded after
classification. Rule ids are normalized before they leave the package. The
`reason` is built from ids and the revision. Parse and compile errors
reference policy text only. `print` is disabled, and `http.send`,
`opa.runtime` and `net.lookup_ip_addr` are removed.

## Testing

`internal/policy` (table tests, temp directories, no network):

- **Decisions.** One policy file per case, one `Request` per case: deny by
  model; by agent; by tool offered; by tool call name and argument value; by
  header; by operation; by time (a clock-dependent rule compares against a
  fixed `time.now_ns` stub through OPA's `rego.Time` option or a rule that
  reads the input only); data-driven rule; three rules firing at once (all
  ids returned, sorted); absent field does not match.
- **Input document.** Golden JSON per scenario: string content versus
  blocks; `tool_use` to `tool_call`; `tool_result` text joining and
  `size_bytes`; image without data; unknown block type; `tool_name` lookup
  hit and miss; headers lower-cased with credentials removed (an
  `Authorization`, `X-Api-Key` and `Cookie` header must be absent); omitted
  keys, never `null`; empty `tools`, `tool_calls`, `tool_results` and
  `parameters` present as `[]` / `{}`; no system prompt gives no `system`
  key; operation names; non-JSON body on `count_tokens`
  (kind `input`) and on an `other` path (no error); duplicate keys
  rejected at the top level (`model` twice), inside `tools`, inside a
  nested tool-call `arguments` object, and in an array element, while the
  same key in two different objects is accepted; the error text contains
  no key and no value.
- **Loading.** Missing policy file; a path that is not a `.rego` file;
  syntax error names file and row; removed built-in rejected (`http.send`,
  `opa.runtime`); `deny` defined as an object; data key collision, and the
  reserved key `cardea`; package-name typo and a file with no `deny` rejected, naming the file; data
  directory with `_test`-style or unrelated files ignored; revision stable
  across data directory order and changes with a one-byte edit.
- **Panic.** With `e.eval` replaced by a function that panics (with a
  marker string in the panic value): `Evaluate` returns
  `*EvalError{internal}`, the process survives, the marker appears in no
  error text or log, the next `Evaluate` succeeds, and the abandoned count
  is unchanged. Also a panic after the deadline (abandoned goroutine) does
  not crash the test binary and the count falls back.
- **Failure.** Timeout with a deliberately expensive rule and a 1 ms limit
  gives `timeout`; a rule that blocks inside a built-in (a test hook
  built-in, or a very large regex input) still returns `timeout` at about
  the limit, not when it finishes; with `maxAbandoned` blocked evaluations
  in flight the next `Evaluate` returns `overloaded` immediately, and the
  count falls back when they end (`go test -race`, `goleak` or an
  explicit counter to prove no goroutine outlives the test); `EvalError.Error()` contains no input value (assert on
  an input carrying a marker string); rule id with a newline or a marker is
  normalized.
- **Cached.** After `New`, delete or overwrite the policy and data files:
  `Evaluate` still decides under the loaded policy, with the same revision,
  until `Reload` is called (and then fails or swaps as usual). A counting
  wrapper around the file-reading function shows zero reads across 1000
  `Evaluate` calls.
- **Reload.** Valid edit swaps and changes revision; invalid edit keeps the
  old snapshot and returns the error; unchanged files are a no-op; deleted
  file fails and does not empty the policy; an evaluation started before a
  swap finishes under the old snapshot (an unexported test hook between
  loading the snapshot and evaluating blocks it on a channel); `go test -race` with concurrent `Evaluate` and `Reload`.
- **Watch.** Write a file, expect one reload after the settle time; write
  five files within the settle time, expect one reload; save by
  rename-and-replace, expect a reload; invalid change is rejected and the
  watch continues; `Watch` ends on context cancel.

`internal/gateway` also covers these end-to-end checks through the real
pipeline, with a fake provider and a `zaptest/observer` logger:

- **No policy (criterion 1).** With `policies.path` unset, a request is
  forwarded byte-identical and the response is unchanged.
- **Quiet allow (criterion 2).** An allowed call under a configured policy
  adds no log line at any level the pipeline writes by default.
- **No content in outputs (criteria 13, 23).** A marker string placed in a
  prompt and in a tool-call argument, and a rule that matches it and
  builds an id from it with `sprintf` (the operator-mistake case): the
  marker may appear only as part of the normalized id, and a second case
  with a *fixed* id asserts the marker appears in **no** log line, no
  trace record and no response body, including the 403 and the 503 cases.
- **Stateless detect-and-halt (criterion 12).** Two successive requests of
  one conversation, each carrying a forbidden tool call, are both denied;
  a third request of a different, clean conversation is allowed. Sending
  the second request to a freshly built Gateway gives the same denial,
  which shows no state was needed from the first.

`internal/gateway`: `policiesStage` against a stub engine and a real one:
allow sets `detail` with an empty `rules`; deny returns 403 with the
generic message and `reason` containing only ids and revision; an engine
error becomes the 503 failure through the real pipeline; `c.Trace == nil`
(tracing off) and an unclaimed session give no `session` key; `buildStages`
order with all three stages on, and with policies off; the warning when a
policy is configured and identification is off.

`internal/pipeline`: `writeDenial` sets `X-Should-Retry: false` for a
denial and for the pipeline's own 413, and none for a failure; a stage
`detail` reaches the request record on allow and on deny; a stage without
detail encodes `"detail": null`.

`internal/config`: defaults and env overrides for every new field;
`CARDEA_GATEWAY_POLICIES_PATH`; unknown key under `policies`
fails; non-positive durations fail.

`internal/cli`: `SIGHUP` reaches `ReloadPolicy` (with a fake) and does not
end the process.

End to end (one test, `httptest` provider): a policy denying a model;
allowed call forwarded; denied call gets 403, `X-Should-Retry: false`, the
generic body without the rule id, and the provider is never called; the
request record shows `policies` `denied` with revision and rules in
`detail`; the response of an allowed call is byte-identical and its
response record is unchanged (functional criterion 25).

## Alternatives considered

- **An OPA server or sidecar queried over HTTP** — rejected: a network hop
  on every request, a second process to run and keep in step, and a new way
  to fail that has to be classified. Embedding keeps the failure modes to
  the ones above.
- **Compiling Rego to Wasm** — rejected: a second runtime and a build
  step for a latency win nobody has measured. A prepared query evaluates
  in microseconds to low milliseconds for policies of this size.
- **A Go rule DSL instead of Rego** — rejected by the requirement, and it
  would give operators a language nobody else can test or review.
- **An ordered list of isolated scripts, first denial wins** — rejected
  for this version: it needs script names in every log and record, an
  order contract, isolation rules and a policy for a script that errors,
  for a need one file plus the data files already covers. `policies.path`
  can become a list later without changing the input document or the
  `deny` contract.
- **Strict built-in errors** — rejected: they turn "this request has no
  agent" into a failure for every rule that mentions the agent, and
  contradict the functional rule that absent fields do not match.
- **Reusing the tracing parser for the input** — rejected: tracing
  promises never to hold content (specs/0003), and its types should not
  grow content fields.
- **Polling file modification times instead of `fsnotify`** — rejected:
  it needs a poll interval to tune and misses a replace that keeps the
  mtime; `fsnotify` is already in the module graph through Viper.
- **Watching the policy file directly** — rejected: editors that save by
  rename drop the watch silently, leaving a policy that looks reloaded and
  is not. Watching the parent directory avoids it.
- **Reloading by checking the revision on every request** — rejected:
  hashing files on the hot path. Reload is event-driven.
- **Rule ids as free text** — rejected (functional spec): a rule could
  build the string from request content, and it would reach the logs.

## Open questions

- **Input conversion cost.** The body is decoded to Go values, rebuilt as a
  `map[string]any`, and then converted again by OPA. For a 10 MiB request
  that is real memory and time. If profiling shows it, build the input as
  `ast.Value` directly and pass it with `rego.EvalParsedInput`. Decided
  with a benchmark on a large conversation, not before.
- **OPA version and binary size.** The first OPA dependency adds
  noticeably to build time and binary size. The pinned version and the
  import path (`github.com/open-policy-agent/opa/v1/...`) are confirmed
  when the module is added; the APIs named here are those of OPA v1 and
  are checked against it then.
- **`rules.path`.** Left in place; whether to remove it once the
  circuit-breaker and compaction rules have their own specs is for those
  specs.

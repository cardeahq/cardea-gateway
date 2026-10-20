# 0008 — Authorization and identification (technical)

**Date:** 2026-10-20
**Functional spec:** specs/0008-authorization-and-identification-functional.md

**Supersedes:** specs/0002-agent-identification-technical.md — see that
file's superseded notice.

## Context

specs/0002 built one stage, `internal/identity`, that both authenticates a
bearer credential (API key or JWT) against a per-agent registry *and*
sets `pipeline.Call.AgentID` from the match. specs/0008 (functional) splits
this into two independent concerns that happen to share the same header:
**authorization** (a global shared-secret gate, no per-agent anything) and
**identification** (classifying the caller's agent *type* from request
headers, never denying). This spec replaces `internal/identity` with two
new, smaller packages and reworks the parts of `internal/config` and
`internal/gateway` that wired the old one in.

## Goals

- Replace `internal/identity`'s per-agent registry and JWT verification
  with a minimal shared-secret gate (`internal/authorization`).
- Replace it, for agent classification, with a directory-driven,
  header-matching classifier (`internal/identification`) that always
  produces an outcome — a matched type or `"unknown"` — and never denies.
- Remove `config.AgentConfig`, the `agents:` config block, and the
  `github.com/golang-jwt/jwt/v5` dependency entirely.
- Keep `pipeline.Call.AgentID`, `policy.Request.AgentID`, and
  `trace.RequestInfo`'s agent-keyed session lookup unchanged in shape —
  only what populates `AgentID` changes.

## Non-goals

- Everything specs/0008 (functional) already excludes: body-content-based
  identification, moving session-source config into agent-type files
  (deferred to a later revision), a credential/agent-type management API.
- Embedding Cardea's Claude Code / Open Code presets in the binary.
  Per the functional spec's operator controls, these ship as plain example
  files under `configs/`, the same way `configs/policies.example/` works
  today — an operator copies or symlinks them into their configured
  directory. `internal/identification` has no built-in knowledge of either
  agent.
- Static detection of overlapping agent-type match rules. First-match-wins
  by file order is the whole design (see *Alternatives considered*).

## Design

Two new stages sit where `identityStage` used to, in `cli.serve`'s existing
flow (`cli.serve → config.Config → zap.Logger → provider.Provider →
gateway.Gateway`): `authorizationStage` first, then `identificationStage`,
both ahead of tracing, policies, and filtering.

**`internal/authorization`** is the direct, shrunk descendant of
`internal/identity`: same bearer-token-shape detection
(`isBearerToken68`), same "exact match against a small set of known
values" matching, minus the registry (no per-entry `agent_id`, so no map
from credential to identity) and minus the entire JWT branch
(`jwt.go`, `looksLikeJWT`, the `golang-jwt` dependency) — none of it
applies to a single shared pool of secrets. `Gate.Authorize` returns
`Result{OK: true}` or a denial reason; it never produces an identity,
because authorization doesn't have one to give.

**`internal/identification`** is new. It has no credential-verification
logic at all — it reads a directory of YAML files at startup, each
describing one agent type as a name plus a list of header-matching rules,
and compiles them into an ordered list (file order = `os.ReadDir`'s sorted
filename order, which is also match precedence, per the functional spec's
rule 4). At request time, `Registry.Identify` walks that list and returns
the first matching type's ID, or the reserved constant `"unknown"`. There
is no error path here: an unmatched request isn't a failure, it's the
documented default outcome.

**Wiring `AgentID` is unchanged.** `identificationStage.Request` returns
`pipeline.AllowAs(registry.Identify(c.Request))` — exactly the same
`Decision` constructor `identityStage` used, so `pipeline.Handler`'s
existing "first non-empty `agentID` wins, nothing later overwrites it"
rule (specs/0002 technical, unchanged by this spec) needs no change.
`authorizationStage.Request` never calls `AllowAs`; it only ever returns
`pipeline.Allow()` or `pipeline.Deny(...)`, the same as `filteringStage`
or `policiesStage` do today.

**Downstream consumers of `AgentID` are unchanged code, changed meaning.**
`internal/policy`'s `buildInput` (`input.go:33`), `internal/trace`'s
`Tracer.Request`/`sourcesFor` (`tracer.go:80,117`), and
`internal/pipeline/records.go`'s `agent_id` trace-record field all keep
their exact current signatures and logic. What changes is only what value
flows through them: with identification on by default, `AgentID` is
non-empty on almost every call (a matched type or `"unknown"`) instead of
being populated only for operators who had opted into the old credential
registry. A Rego rule written as `input.agent.id == "..."` or a
`tracing.sessions[].agents` entry can now legitimately target `"unknown"`
to apply default/stricter handling to unclassified callers — this is a
feature of the split, not a side effect to work around.

**The one real cross-package consequence:** `config.validateTracing`
today cross-checks `tracing.sessions[].agents` against `cfg.Agents`
(`config.go:364,391-399`), both available purely from the parsed config
file. That source of truth disappears with `AgentConfig`. Re-deriving it
would mean `config.Load` re-implementing `identification.NewRegistry`'s
directory scan, which `internal/config` has no business doing (it doesn't
import `internal/identification`, any more than `internal/identity`
imported `internal/config` the other way in specs/0002). So this one
check moves to `gateway.buildStages`, which already constructs the
registry: after building it, `buildStages` validates every name in
`cfg.Tracing.Sessions[].Agents` against `registry.IDs()` plus the reserved
`"unknown"`, and fails the same way `config.Load` used to. When
identification is disabled, `buildStages` skips this check entirely —
`Call.AgentID` is always `""` in that case, so a stray name in
`tracing.sessions[].agents` is simply unreachable config, not an error,
matching how specs/0002 already let a dependent stage decide how it
degrades when identification is off.

## Packages and types

### `internal/identity` — deleted

`identity.go`, `jwt.go`, `credential.go`, and their tests are removed in
full, along with the `github.com/golang-jwt/jwt/v5` dependency from
`go.mod`.

### `internal/authorization` — new

`authorization.go`:

```go
// DenialReason is the audit-only category for a failed authorization
// attempt. Never sent to the caller.
type DenialReason string

const (
	ReasonNoCredential      DenialReason = "no_credential"
	ReasonUnrecognizedShape DenialReason = "unrecognized_shape"
	ReasonNoMatch           DenialReason = "no_match"
)

// Result is Authorize's outcome. Reason is audit-only and must never
// reach the caller.
type Result struct {
	OK     bool
	Reason DenialReason // set on any denial
}

// Gate holds the set of valid shared secrets for the life of the
// process.
type Gate struct {
	secrets map[string]bool
}

// NewGate reads each named env var and registers its value as a valid
// shared secret. It fails if any named var is unset or empty. A Gate
// built from a zero-length apiKeyEnvs is valid — Authorize then denies
// every call, matching specs/0002's "zero agents" precedent.
func NewGate(apiKeyEnvs []string) (*Gate, error)

// Authorize validates one raw Authorization header value (as returned by
// http.Header.Get; "" if the header is absent) against the Gate's
// secrets.
func (g *Gate) Authorize(authorizationHeader string) Result
```

`credential.go` — carried over from `internal/identity` unchanged:
`isBearerToken68(s string) bool` and `isB64TokenChar(c byte) bool`. JWT
shape detection (`looksLikeJWT`) is not carried over — authorization never
looks for a JWT.

### `internal/identification` — new

`identification.go`:

```go
// Unknown is the agent type attached to a call that matches no
// configured agent type, has no configured agent types to match
// against, or is otherwise unclassified. It is reserved: no agent-type
// file may use it as its own agent_id.
const Unknown = "unknown"

// MatchRule is one way to recognize an agent type from a single request
// header. Exactly one of Value, Prefix, Pattern is set.
type MatchRule struct {
	Header  string
	Value   string // exact, case-sensitive match against the header's value
	Prefix  string // the header's value starts with this
	Pattern string // RE2; the header's value matches this
}

// agentType is one loaded, compiled agent-type definition.
type agentType struct {
	id    string
	rules []compiledRule // one per MatchRule
}

// Registry holds every loaded agent type, in file-load order — also
// match precedence (functional spec rule 4): the first type any of whose
// rules match wins.
type Registry struct {
	types []agentType
}

// NewRegistry scans dir for *.yaml/*.yml files, each one agent-type
// definition, sorted by filename (Go's os.ReadDir order). dir == ""
// returns an empty, valid Registry — every call resolves to Unknown — it
// is not an error to run identification with no agent-type directory
// configured. It fails if dir is set but does not exist, if two files
// share an agent_id, if a file's agent_id is Unknown, or if any match
// rule does not set exactly one of Value/Prefix/Pattern, or sets a
// Pattern that does not compile as RE2.
func NewRegistry(dir string) (*Registry, error)

// IDs returns every loaded agent type's ID, in no particular order. Used
// by gateway.buildStages to cross-validate tracing.sessions[].agents.
func (r *Registry) IDs() []string

// Identify returns the first matching agent type's ID, or Unknown.
func (r *Registry) Identify(req *http.Request) string
```

`load.go` — the directory scan and per-file YAML decode into `MatchRule`,
using `gopkg.in/yaml.v3` promoted from an indirect to a direct dependency
in `go.mod` (Viper currently pulls in `go.yaml.in/yaml/v3` transitively,
a different import path; this needs its own `go get`, not a free reuse),
kept separate from matching logic the same way `internal/trace`'s
`compileSource` is kept separate from `sourcesFor`.

### `internal/pipeline`, `internal/policy`, `internal/trace` — unchanged

No type or signature in any of these three packages changes. See *Design*
for why.

### `internal/gateway` — modified

`identity.go` is deleted. `authorization.go` (new):

```go
const authorizationDenialMessage = "not authorized"

type authorizationStage struct {
	gate *authorization.Gate
}

func (s *authorizationStage) Name() string { return "authorization" }

func (s *authorizationStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	result := s.gate.Authorize(c.Request.Header.Get("Authorization"))
	if !result.OK {
		return pipeline.Deny(http.StatusUnauthorized, authorizationDenialMessage, string(result.Reason)), nil
	}
	return pipeline.Allow(), nil
}
```

`identification.go` (new):

```go
const identificationStageName = "identification"

type identificationStage struct {
	registry *identification.Registry
}

func (s *identificationStage) Name() string { return identificationStageName }

func (s *identificationStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	return pipeline.AllowAs(s.registry.Identify(c.Request)), nil
}
```

Unlike the old `identityStage`, `authorizationStage` carries no claimed
identity to sanitize and log — there is nothing to claim — so
`sanitizeClaimed` is not carried over.

`gateway.go`'s `buildStages`:

```go
func buildStages(cfg config.Config, logger *zap.Logger) ([]pipeline.Stage, trace.CloseableSink, *policy.Engine, *filtering.Engine, error) {
	var stages []pipeline.Stage
	var sink trace.CloseableSink

	if cfg.Pipeline.Stages.Authorization {
		gate, err := authorization.NewGate(cfg.Authorization.APIKeyEnvs)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("building authorization gate: %w", err)
		}
		stages = append(stages, &authorizationStage{gate: gate})
	}

	var identRegistry *identification.Registry
	if cfg.Pipeline.Stages.Identification {
		var err error
		identRegistry, err = identification.NewRegistry(cfg.Identification.AgentTypesDir)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("building identification registry: %w", err)
		}
		if err := validateTracingAgentTypes(cfg.Tracing.Sessions, identRegistry.IDs()); err != nil {
			return nil, nil, nil, nil, err
		}
		stages = append(stages, &identificationStage{registry: identRegistry})
	}

	// ... tracing / policies / filtering stages, unchanged ...
}

// validateTracingAgentTypes checks every name in sessions[].agents
// against known, identified agent types (plus the reserved "unknown"),
// replacing config.validateTracing's old cfg.Agents-based check — see
// Design.
func validateTracingAgentTypes(sessions []config.SessionSourceList, knownIDs []string) error
```

`buildStages`'s existing warning ("a policy is configured but
identification is off: rules cannot tell agents apart",
`gateway.go:148-150`) is unchanged — it already keys off
`cfg.Pipeline.Stages.Identification`, which still means the same thing:
no stage after identification will ever see a non-empty `AgentID`.

### `internal/config` — modified

```go
type Config struct {
	Server         ServerConfig         `mapstructure:"server"`
	Logging        LoggingConfig        `mapstructure:"logging"`
	Rules          RulesConfig          `mapstructure:"rules"`
	Provider       ProviderConfig       `mapstructure:"provider"`
	Pipeline       PipelineConfig       `mapstructure:"pipeline"`
	Authorization  AuthorizationConfig  `mapstructure:"authorization"`  // new, replaces Agents
	Identification IdentificationConfig `mapstructure:"identification"` // new
	Tracing        TracingConfig        `mapstructure:"tracing"`
	Policies       PoliciesConfig       `mapstructure:"policies"`
	Filtering      FilteringConfig      `mapstructure:"filtering"`
}

type StagesConfig struct {
	Authorization  bool `mapstructure:"authorization"`  // new, defaults false
	Identification bool `mapstructure:"identification"` // defaults true (was false)
	Tracing        bool `mapstructure:"tracing"`
	Policies       bool `mapstructure:"policies"`
	Filtering      bool `mapstructure:"filtering"`
}

// AuthorizationConfig configures the shared-secret gate. Secret values
// are never written here — only the names of the environment variables
// holding them, matching ProviderConfig.APIKeyEnv.
type AuthorizationConfig struct {
	APIKeyEnvs []string `mapstructure:"api_key_envs"`
}

// IdentificationConfig configures agent-type recognition.
type IdentificationConfig struct {
	// AgentTypesDir holds one YAML file per recognized agent type. ""
	// means none configured — every call resolves to
	// identification.Unknown.
	AgentTypesDir string `mapstructure:"agent_types_dir"`
}
```

`AgentConfig` and the `Agents []AgentConfig` field are deleted.
`config.go`'s per-agent validation loop (`config.go:350-361`) is deleted
with it — `AuthorizationConfig`/`IdentificationConfig` need no equivalent,
since neither has a list of entries to check for duplicates at this
layer (identification's duplicate-`agent_id` check needs the directory's
file contents, so it lives in `identification.NewRegistry` instead, per
*Design*).

`validateTracing` drops the `agents map[string]bool` parameter entirely
— it still enforces session-list shape (exactly one of
header/body_field/baggage, at most one `default: true` list, no name
listed twice) but no longer cross-checks names against anything; that
check moves to `gateway.validateTracingAgentTypes` (see *Packages and
types* above).

## Configuration

```yaml
pipeline:
  stages:
    authorization: false  # off by default — see note below
    identification: true  # on by default
    tracing: true
    policies: true
    filtering: true

# The shared-secret gate. Needed only when pipeline.stages.authorization
# is true. Each entry names an environment variable holding one valid
# secret; list two while rotating. Secret values are never written here.
authorization:
  api_key_envs: []
  # - "GATEWAY_API_KEY"
  # - "GATEWAY_API_KEY_NEXT" # during rotation

# Agent-type recognition. agent_types_dir holds one YAML file per
# recognized type; see configs/identification.example/ for Cardea's
# built-in Claude Code and Open Code definitions to copy in.
identification:
  agent_types_dir: "" # "" = none configured; every call is "unknown"
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.stages.authorization` | bool | `false` | `CARDEA_GATEWAY_PIPELINE_STAGES_AUTHORIZATION` |
| `pipeline.stages.identification` | bool | `true` | `CARDEA_GATEWAY_PIPELINE_STAGES_IDENTIFICATION` |
| `authorization.api_key_envs` | `[]string` | `[]` | not settable via env (list) |
| `identification.agent_types_dir` | string | `""` | `CARDEA_GATEWAY_IDENTIFICATION_AGENT_TYPES_DIR` |

`pipeline.stages.authorization` defaults to `false` for the same reason
`identification` did in specs/0002: "enabled with nothing configured"
means "deny everything," which must be a deliberate operator act, not an
upgrade surprise. `pipeline.stages.identification` defaults to `true`,
diverging from specs/0002: "enabled with nothing configured" now means
"every call is `unknown`," which is the staging pipeline's ordinary
"allow everything, nothing to enforce" shape (specs/0001 rule 4), so
identification no longer needs the exception specs/0002 carved out for
it. Both booleans still need explicit entries in `defaults()`/`Load()`
rather than relying on Go's zero value, to document that each default is
a deliberate, specific choice.

One agent-type file, e.g. `configs/identification.example/claude_code.yaml`:

```yaml
agent_id: "claude-code"
match:
  - header: "X-Claude-Code-Session-Id"
    pattern: "."
  - header: "user-agent"
    prefix: "claude-cli/"
```

and `configs/identification.example/opencode.yaml`:

```yaml
agent_id: "opencode"
match:
  - header: "x-opencode-session"
    pattern: "."
```

These ship as plain example files, not embedded in the binary (see
*Non-goals*); an operator adopts one by copying it into their configured
`agent_types_dir`.

Both new config fields need entries in `defaults()`/`Load()`, and
`configs/cardea-gateway.example.yaml` needs the `agents:` block replaced
by the `authorization:`/`identification:` blocks above.

## Failure modes

| Condition | Result |
| --- | --- |
| Authorization enabled, no `Authorization` header | 401, generic denial; audit reason `no_credential` |
| Authorization enabled, header not `Bearer <token68>` | 401, generic denial; audit reason `unrecognized_shape` |
| Authorization enabled, bearer value matches no configured secret | 401, generic denial; audit reason `no_match` |
| Authorization enabled, zero `api_key_envs` configured | Every call denied — documented expected behavior, not a startup failure |
| An `api_key_envs` entry names an unset/empty env var | `authorization.NewGate` fails (surfaced through `gateway.New`); process exits before listening |
| Authorization disabled | Stage absent; every call proceeds regardless of `Authorization` header |
| Identification enabled, `agent_types_dir` unset/empty | `Registry` has zero types; every call's `AgentID` is `"unknown"` |
| Identification enabled, `agent_types_dir` set but does not exist | `identification.NewRegistry` fails; process exits before listening |
| A file in `agent_types_dir` fails to parse, has no `agent_id`, sets `agent_id: "unknown"`, or a match rule does not set exactly one of `value`/`prefix`/`pattern` | `identification.NewRegistry` fails, naming the file; process exits before listening |
| Two files in `agent_types_dir` share an `agent_id` | `identification.NewRegistry` fails; process exits before listening |
| `tracing.sessions[].agents` names an ID that is neither a loaded agent type nor `"unknown"`, while identification is enabled | `gateway.buildStages` fails via `validateTracingAgentTypes`; process exits before listening |
| Identification disabled | Stage absent; `Call.AgentID` always `""`; a stray `tracing.sessions[].agents` entry is inert, not validated |
| `authorizationStage.Request` / `identificationStage.Request` panics (e.g. a wiring bug leaves a nil gate/registry) | Recovered by the pipeline's existing panic handling (specs/0001); 503 |

Requests fail **closed** for authorization and **never fail** for
identification, exactly as specs/0008 (functional) states. Neither stage
introduces a failure mode beyond what specs/0001 already defines for a
stage's error vs. its denial.

## Testing

`internal/authorization` (new; table-driven, mirrors
`internal/identity`'s old API-key-branch tests minus everything JWT):

- `NewGate` rejects an unset/empty named env var; accepts a zero-length
  `apiKeyEnvs`; accepts one and two configured secrets.
- `Authorize`: missing header → `no_credential`; wrong scheme, empty
  Bearer value, or a disallowed byte → `unrecognized_shape`; a
  syntactically fine bearer value matching none of the secrets →
  `no_match`; a value matching either of two configured secrets → `OK`.
- A `Gate` built from a zero-length `apiKeyEnvs`: `Authorize` denies every
  case above — missing header, any syntactically valid bearer value, and
  the empty string — all with `no_credential`/`unrecognized_shape`/
  `no_match` as appropriate, never `OK` (functional spec acceptance
  criterion 5).
- No field of `Result` ever contains a substring of the raw input
  credential.

`internal/identification` (new; table-driven, using `t.TempDir()` fixture
directories):

- `NewRegistry("")` succeeds with zero types; `Identify` always returns
  `Unknown`.
- A directory with one well-formed file: a request matching its rule by
  `value`, by `prefix`, and by `pattern` each resolves to that type; a
  request matching none resolves to `Unknown`.
- Two files whose rules could both match the same request: `Identify`
  returns the one from the alphabetically-first filename, proven with
  both file-name orderings to show it's the name, not registration order,
  that decides it.
- Loading the actual shipped `configs/identification.example/claude_code.yaml`
  and `opencode.yaml` (not synthetic fixtures) into a `Registry` and
  asserting `Identify` resolves to `"claude-code"` for a request carrying
  `X-Claude-Code-Session-Id` or a `user-agent` prefixed `claude-cli/`, and
  to `"opencode"` for one carrying `x-opencode-session` — this is what
  functional spec acceptance criterion 8 actually requires; the synthetic
  fixture cases above exercise the matching mechanism, not whether the
  shipped presets work.
- Rejected at `NewRegistry`: a nonexistent configured directory; two files
  sharing an `agent_id`; a file whose `agent_id` is `"unknown"`; a match
  rule with zero or more than one of `value`/`prefix`/`pattern` set; an
  invalid RE2 `pattern`.
- `IDs()` returns every loaded type's ID and nothing else.

`internal/gateway`:

- `authorization_test.go` (replacing `identity_test.go`'s authorization
  half): `authorizationStage.Name() == "authorization"`; each denial
  category produces the same status/message but a distinct
  `Denial.Reason`; `buildStages` prepends it ahead of identification when
  both are enabled, omits it when disabled, and `buildStages`/`gateway.New`
  return an error when `authorization.NewGate` fails.
- `identification_test.go` (new): `identificationStage.Name() ==
  "identification"`; a matching request's `Call.AgentID` is the matched
  type; a non-matching one is `"unknown"`; identification disabled means
  `Call.AgentID` stays `""` through the full pipeline.
- `buildStages` rejects a `tracing.sessions[].agents` entry naming an
  unrecognized ID when identification is enabled, and does not validate
  it at all when identification is disabled.
- Full-pipeline integration: authorization enabled and failing denies the
  call before identification runs (`Call.AgentID` stays `""`); both
  enabled and authorization passing lets identification run and attach
  its result.
- `policies_test.go`'s existing
  `TestBuildStages_PolicyConfiguredWithoutIdentificationWarns` keeps
  passing unmodified — it already only depends on
  `cfg.Pipeline.Stages.Identification`, not on `cfg.Agents`.
- `tracing_test.go`'s agent-keyed session tests are re-pointed at
  `identificationStage` fixtures (a small temp-dir agent-type registry)
  instead of `identity.Registry`, but assert the same outcomes: tracing
  runs after identification, and both request/response records carry
  whatever `AgentID` identification produced.

`internal/config`:

- `defaults()` and an empty config file both produce
  `authorization == false`, `identification == true`, empty
  `Authorization.APIKeyEnvs`, and empty `Identification.AgentTypesDir`.
- `CARDEA_GATEWAY_PIPELINE_STAGES_AUTHORIZATION=true` and
  `CARDEA_GATEWAY_PIPELINE_STAGES_IDENTIFICATION=false` both override
  their defaults.
- `validateTracing`'s structural checks (exactly one of
  header/body_field/baggage, one `default: true` list, no agent name
  twice) still pass without needing any `agents:` config at all.
- No test asserts a cross-check between `tracing.sessions[].agents` and
  anything in `config.Config` — that behavior moved to
  `internal/gateway` (see its own test list above).

## Alternatives considered

- **Embed Cardea's Claude Code / Open Code presets in the binary
  (`go:embed`), auto-loaded unless overridden** — rejected per the
  functional spec's operator-controls decision: shipping them as plain
  files under `configs/identification.example/` matches the existing
  `configs/policies.example/` convention, needs no new embedding
  machinery, and keeps `internal/identification` with exactly one way to
  load agent types (a directory) instead of two (embedded defaults plus
  an override directory) to reason about.
- **Static overlap detection**: fail startup if two agent-type files'
  rules could both match the same request — rejected per the functional
  spec's decision: proving two arbitrary regex/prefix/value rules can
  never simultaneously match is not generally decidable for the `pattern`
  case, and first-match-by-filename is simple, deterministic, and fully
  operator-visible without it.
- **Rename the `agent_id` field to `agent_type` everywhere it flows**
  (policy input, trace records, `tracing.sessions[].agents`) to reflect
  that it's no longer a verified identity — rejected: it would touch
  `internal/policy`, `internal/trace`, and every existing Rego rule and
  tracing config an operator has already written, for a purely naming
  gain. The field has always meant "whichever handle later stages use to
  tell callers apart"; that remains true even though what establishes it
  changed from credential verification to header matching.
- **Keep the `tracing.sessions[].agents` cross-check in `config.Load`** by
  having it scan `identification.agent_types_dir` itself — rejected:
  duplicates `identification.NewRegistry`'s directory-scanning and
  validation logic in a package that otherwise does zero filesystem
  reads beyond the one config file, and creates an import-path problem
  (`internal/config` would need to import `internal/identification`,
  which doesn't yet exist when `config.Load` runs during `cli.serve`'s
  own config-loading step, before `internal/gateway` is even reached).
- **A single shared-secret *and* a JWT-with-no-`sub` mode for
  authorization**, keeping some trace of specs/0002's JWT support —
  rejected: the functional spec is explicit that authorization is a
  shared secret only (non-goal); a token format whose only job is "prove
  you know a secret" doesn't need a key management and validity-window
  mechanism built for proving *which* agent you are, since this stage no
  longer distinguishes agents at all.

## Open questions

- Should `MatchRule.Value`/`Prefix` be case-sensitive (as specced) or
  case-insensitive? Header *names* are already case-insensitive via
  `http.Header.Get`; this question is only about header *values*, which
  are producer-controlled strings (e.g. a user-agent prefix) that a given
  SDK version should emit consistently. Case-sensitive until a real
  preset needs otherwise.
- `MatchRule` reads only the first value of a repeated header
  (`http.Header.Get`'s behavior), matching `internal/trace`'s existing
  `SessionSource` convention. Revisit if an agent SDK is found that sends
  a distinguishing header more than once.
- Whether `authorization.NewGate` should warn (not fail) when two
  `api_key_envs` entries resolve to the identical secret — harmless
  (rotation with the old and new key temporarily equal is a normal state
  to pass through), but possibly worth a log line so an operator notices
  a copy-paste mistake. Not designed here; cheap to add later without a
  signature change.
- specs/0002's constant-time-comparison open question (`crypto/subtle`
  for the shared-secret match) carries over unchanged and is still
  undecided.

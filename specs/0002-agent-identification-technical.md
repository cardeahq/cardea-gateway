# 0002 — Agent identification (technical)

**Date:** 2026-09-21
**Functional spec:** specs/0002-agent-identification-functional.md

## Context

The staging pipeline (0001) gives every governance stage a fixed slot and
a uniform allow/deny mechanism, but no stage currently knows who is
calling — every request is anonymous. Later stages (policies, monitoring,
filtering, rate-limiting, audit) all need a per-caller `agent_id` that
nothing produces today. `pipeline.Call` has no field for it,
`gateway.buildStages` has no entry for it, and no credential, registry, or
JWT-handling code exists anywhere in the repository. This spec adds all
three: a new `internal/identity` package that authenticates a bearer
credential against an operator-configured registry, a small extension to
`internal/pipeline` so a stage can attach the identity it establishes, and
the wiring in `internal/gateway`/`internal/config` that turns it into the
pipeline's first stage.

## Goals

- Authenticate every proxied call, per specs/0002 (functional), to an
  operator-registered `agent_id`, accepting either an opaque API key or an
  HS256 JWT as the credential.
- Attach the resulting `agent_id` to `pipeline.Call` without weakening the
  "only the pipeline mutates `Call`" invariant specs/0001 established.
- Deny every failure mode identically to the caller, while keeping the
  four cases distinguishable in the audit-only reason.
- Fail closed: enabled with zero (or zero matching) agents denies
  everything; misconfiguration fails at startup, not at request time.

## Non-goals

- Everything specs/0002 (functional) already excludes: deciding what an
  identified agent may do, per-agent quotas, audit log format/storage, a
  credential management API, fetching JWT keys from an external IdP or
  JWKS endpoint, mTLS, and holding more than one live credential per
  agent.
- Asymmetric JWT signing (RS256/ES256). This version supports HS256 only;
  see *Alternatives considered*.
- Constant-time credential comparison. Left as an *Open question* rather
  than designed here.

## Design

Credential verification and the agent registry live in a new
`internal/identity` package, kept separate from `internal/gateway` because
this stage — unlike `tokenStage` — carries real surface: registry
construction and validation, credential parsing, and a new third-party
dependency (`github.com/golang-jwt/jwt/v5`). `internal/gateway` gets a
thin `pipeline.Stage` wrapper, `identityStage`, that holds a
`*identity.Registry` and delegates every decision to it — the same shape
`tokenStage` uses for `tokenCounter`, just with a package boundary in
between.

`buildStages` prepends `identityStage` ahead of `tokenStage`, so
identification is the first stage the pipeline runs, exactly as specs/0001
requires for anything the rest of the pipeline should be able to trust.

**Credential-shape detection.** `Registry.Authenticate` receives the raw
`Authorization` header value and resolves it in three tiers, each
producing at most one of the four denial reasons:

1. **Header framing.** Missing or empty header → `no_credential`. Present
   but not `Bearer <token68>` — wrong scheme, empty value after `Bearer `,
   or a byte outside RFC 6750 §2.1's `b64token` grammar → `unrecognized_shape`.
   This is the only tier that can produce `unrecognized_shape`; it never
   touches the registry.
2. **Structural sub-shape**, only once tier 1 passes. Exactly three
   non-empty, base64url-valid, dot-separated segments selects the JWT
   branch; anything else selects the API-key branch. This is a syntactic
   choice of code path, not a validity judgment — a 3-segment string with
   garbage inside still enters the JWT branch and is resolved there.
3. **Matching.**
   - *API-key branch*: exact match against the registry's key map, or
     `no_match`.
   - *JWT branch*: parsed with `jwt.WithValidMethods([]string{"HS256"})`
     and `jwt.WithExpirationRequired()` — a JWT with no `exp` claim has no
     validity window to be inside, so it is rejected rather than treated
     as perpetually valid — plus a `Keyfunc` that reads the token's (not
     yet verified) `sub` claim to select which agent's secret to check
     the signature against. An unreadable/missing `sub`, a `sub` naming
     no registered agent or an agent with no JWT material, a non-HS256
     token, or a signature that doesn't verify → `no_match`. A verified
     signature with a missing `exp`, or outside `exp`/`nbf` →
     `outside_validity_window`. A verified signature inside its window →
     success.

Collapsing every JWT-branch failure into just `no_match` or
`outside_validity_window` (never `unrecognized_shape`) keeps
`unrecognized_shape` meaning exactly one thing — not even syntactically a
bearer credential — and avoids inventing a fifth caller-adjacent category
for "claims to be a JWT but isn't a good one."

**Attaching `agent_id` to `Call`.** `pipeline.Decision` gains an
unexported `agentID` field and a new constructor, `AllowAs(agentID
string)`. `pipeline.Call` gains an exported `AgentID string` field. The
pipeline's `Handler` — the only place `Call` is mutated, per its own
invariant — copies `decision.agentID` onto `c.AgentID` immediately after
its existing `decision.allow` check, but only if `c.AgentID` is still
empty, so identification (which runs first) establishes the identity and
no later stage's `Decision` can overwrite it. `identityStage.Request`
itself never touches `*Call`; it only returns `pipeline.Deny(...)` or
`pipeline.AllowAs(...)`, keeping the existing "stages read, pipeline
writes" contract intact.

`identityStage.Request` has no infra-failure path — matching is local
string comparison and HMAC verification, with no I/O at request time — so
it never returns a Go `error`. Registry construction, which does touch the
environment, happens once at startup; a wiring bug that reaches
`Request` with a nil registry would panic, which specs/0001's existing
panic recovery already turns into a 503 without any extra code here.

## Packages and types

### `internal/identity` — new

`identity.go` — the registry and its public entry point:

```go
// DenialReason is the audit-only category for a failed identification
// attempt. Never sent to the caller.
type DenialReason string

const (
	ReasonNoCredential      DenialReason = "no_credential"
	ReasonUnrecognizedShape DenialReason = "unrecognized_shape"
	ReasonNoMatch           DenialReason = "no_match"
	ReasonOutsideValidity   DenialReason = "outside_validity_window"
)

// Result is Authenticate's outcome. Reason and ClaimedAgentID are
// audit-only and must never reach the caller.
type Result struct {
	OK      bool
	AgentID string // set on success

	Reason         DenialReason // set on any denial
	ClaimedAgentID string       // unverified "sub" a JWT claimed, when
	                            // readable, even on denial; always "" for
	                            // the API-key branch
}

type Registry struct {
	byAPIKey map[string]string      // api key -> agent_id
	byAgent  map[string]*agentEntry // agent_id -> entry
}

// agentEntry is one registered agent's verification material. Either
// field may be nil/"" — NewRegistry requires at least one to be set, but
// not both.
type agentEntry struct {
	apiKey    string // "" if this agent has no API-key credential
	jwtSecret []byte // nil if this agent has no JWT credential
}

// NewRegistry builds a Registry from config, reading each entry's
// api_key_env / jwt_secret_env from the process environment — never from
// YAML, matching config.ProviderConfig.APIKeyEnv's existing pattern. It
// fails if an entry names an env var that is unset or empty, a
// jwt_secret_env resolves to fewer than 32 bytes (HS256's RFC 7518 §3.2
// floor), or two entries register the identical literal API key.
// config.Load has already rejected a duplicate agent_id or an entry with
// neither field set by the time this runs.
func NewRegistry(agents []config.AgentConfig) (*Registry, error)

// Authenticate validates one raw Authorization header value (as returned
// by http.Header.Get; "" if the header is absent) and returns exactly one
// Result. All identification logic lives here — internal/gateway's stage
// wrapper contains none of its own.
func (r *Registry) Authenticate(authorizationHeader string) Result
```

`credential.go` — tier 1/tier 2 parsing: `isBearerToken68(s string) bool`
(RFC 6750 `b64token` charset check) and `looksLikeJWT(credential string)
bool` (three non-empty, base64url-decodable, dot-separated segments).

`jwt.go` — tier 3's JWT branch, wrapping `github.com/golang-jwt/jwt/v5`:
the `Keyfunc` that reads `sub` from the token's claims and returns the
matching agent's secret, and error classification —
`errors.Is(err, jwt.ErrTokenExpired)`,
`errors.Is(err, jwt.ErrTokenNotValidYet)`, or
`errors.Is(err, jwt.ErrTokenRequiredClaimMissing)` (returned for a
missing `exp`, since tokens are parsed with `jwt.WithExpirationRequired()`)
map to `ReasonOutsideValidity`, everything else to `ReasonNoMatch`.

### `internal/pipeline` — modified

`pipeline.go`:

```go
type Call struct {
	ID       string
	Received time.Time
	Request  *http.Request
	Body     []byte

	// AgentID is set by the pipeline from the identification stage's
	// Decision on a successful match (AllowAs). Empty when identification
	// is disabled, hasn't run yet, or denied the call. Once set, no later
	// stage's Decision can overwrite it — see Handler.
	AgentID string

	Status   int
	Duration time.Duration
	Denial   *Denial
	ResponseBody      []byte
	ResponseTruncated bool
	ResponseErr       error
}

type Decision struct {
	allow   bool
	status  int
	message string
	reason  string
	agentID string
}

func Allow() Decision
func Deny(status int, message, reason string) Decision

// AllowAs allows the call and attaches agentID as its identity. A later
// stage's plain Allow() never clears or overwrites an identity a prior
// AllowAs already set — see Handler.
func AllowAs(agentID string) Decision
```

`Handler`'s stage loop, immediately after the existing
`if !decision.allow { ... break }` block:

```go
if decision.agentID != "" && c.AgentID == "" {
	c.AgentID = decision.agentID
}
```

This is purely additive: `Allow()`/`Deny()` keep their exact signatures,
`Call` gains one new zero-valued field, and every existing stage,
including `tokenStage`, is unaffected.

### `internal/gateway` — modified

`identity.go` (new):

```go
const identityDenialMessage = "could not identify caller"

type identityStage struct {
	registry *identity.Registry
}

func (s *identityStage) Name() string { return "identification" }

func (s *identityStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	result := s.registry.Authenticate(c.Request.Header.Get("Authorization"))
	if !result.OK {
		reason := string(result.Reason)
		if result.ClaimedAgentID != "" {
			reason += ": claimed agent_id=" + sanitizeClaimed(result.ClaimedAgentID)
		}
		return pipeline.Deny(http.StatusUnauthorized, identityDenialMessage, reason), nil
	}
	return pipeline.AllowAs(result.AgentID), nil
}
```

`identityDenialMessage` is one fixed string for every denial category,
which is what makes the four categories identical to the caller while
`Denial.Reason` (audit-only, per specs/0001) still carries the specific
reason and, when known, the claimed identity. `sanitizeClaimed` truncates
and strips control characters from the unverified `sub` before it reaches
a log line, since it's attacker-influenced input even though it isn't the
raw credential.

`gateway.go`: `buildStages` and `New` both gain an error return, since
registry construction can now fail:

```go
func buildStages(cfg config.Config, logger *zap.Logger) ([]pipeline.Stage, error) {
	var stages []pipeline.Stage
	if cfg.Pipeline.Stages.Identification {
		registry, err := identity.NewRegistry(cfg.Agents)
		if err != nil {
			return nil, fmt.Errorf("building agent registry: %w", err)
		}
		stages = append(stages, &identityStage{registry: registry})
	}
	if cfg.Pipeline.Stages.TokenCounting {
		stages = append(stages, &tokenStage{logger: logger, counter: &tokenCounter{}})
	}
	return stages, nil
}

func New(cfg config.Config, logger *zap.Logger, prov provider.Provider) (*Gateway, error) {
	// unchanged except: stages, err := buildStages(cfg, logger); if err != nil { return nil, err }
}
```

Identification is appended first simply by being the first `if` in
`buildStages`.

### `internal/config` — modified

```go
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Logging  LoggingConfig  `mapstructure:"logging"`
	Rules    RulesConfig    `mapstructure:"rules"`
	Provider ProviderConfig `mapstructure:"provider"`
	Pipeline PipelineConfig `mapstructure:"pipeline"`
	Agents   []AgentConfig  `mapstructure:"agents"` // new
}

type StagesConfig struct {
	TokenCounting  bool `mapstructure:"token_counting"`
	Identification bool `mapstructure:"identification"` // new, defaults false
}

// AgentConfig is one operator-registered agent. At least one of
// APIKeyEnv/JWTSecretEnv must be set; both may be, e.g. while migrating
// an agent from an API key to a JWT. Credential values are never written
// here — only the name of the environment variable holding them,
// matching ProviderConfig.APIKeyEnv.
type AgentConfig struct {
	AgentID      string `mapstructure:"agent_id"`
	APIKeyEnv    string `mapstructure:"api_key_env"`
	JWTSecretEnv string `mapstructure:"jwt_secret_env"`
}
```

`Load` gains, alongside its existing `MaxBodyBytes` check: rejecting a
duplicate `AgentID` across `Agents`, and rejecting any entry with both
`APIKeyEnv` and `JWTSecretEnv` empty. Both checks are pure config-shape
validation — no environment access — matching the existing style of
`Load`'s inline checks. `Load`'s `agents` entries are also scoped into the
same `ErrorUnused`-via-`UnmarshalKey` pattern already used for
`pipeline.stages`, so a typo'd field (e.g. `jwt_secert_env`) fails at
startup instead of silently vanishing.

Environment lookups (does the named env var actually exist, is a JWT
secret long enough, do two agents share a literal API key) happen later,
in `identity.NewRegistry` — not in `config.Load` — mirroring how
`provider.NewAnthropicProvider` reads `APIKeyEnv` from the environment
rather than `config.Load` doing it. This also avoids an import cycle:
`internal/identity` imports `internal/config` for `AgentConfig`, so
`internal/config` cannot import `internal/identity` back.

### `internal/cli` — modified

`serve.go`: `gateway.New` now returns an error, handled the same way
`provider.NewAnthropicProvider`'s error already is:

```go
gw, err := gateway.New(cfg, logger, prov)
if err != nil {
	return err
}
return gw.Run(ctx)
```

## Configuration

```yaml
pipeline:
  max_body_bytes: 10485760
  stages:
    token_counting: true
    identification: false # off by default — see note below

# Agents the Gateway recognizes once identification is enabled. Each
# entry needs api_key_env, jwt_secret_env, or both. Credential values are
# never written here — only the name of the environment variable holding
# them, matching provider.api_key_env.
agents: []
  # - agent_id: "acme-bot"
  #   api_key_env: "AGENT_ACME_BOT_API_KEY"
  # - agent_id: "beta-bot"
  #   jwt_secret_env: "AGENT_BETA_BOT_JWT_SECRET"
```

| Field | Type | Default | Env var |
| --- | --- | --- | --- |
| `pipeline.stages.identification` | bool | `false` | `CARDEA_GATEWAY_PIPELINE_STAGES_IDENTIFICATION` |
| `agents` | `[]AgentConfig` | `[]` | not settable via env (list) |
| `agents[].agent_id` | string | — | — |
| `agents[].api_key_env` | string | `""` | — |
| `agents[].jwt_secret_env` | string | `""` | — |

`pipeline.stages.identification` defaults to `false`, diverging from
specs/0001's "every stage boolean defaults `true`" rule. The reason isn't
stylistic: for every other stage, "enabled with nothing configured to
enforce" means "allow everything" (specs/0001, rule 4) — but for
identification, "enabled with nothing configured" means "no caller can
ever be identified," which is indistinguishable from denying all traffic
outright. An operator upgrading the Gateway must not discover this as an
outage. Both `defaults()` and `Load()` must set this boolean to `false`
explicitly — leaving it unregistered would coincidentally also unmarshal
to `false` (Go's zero value), but registering it explicitly documents
that the divergence from every other stage is deliberate.

Both new fields need entries in `defaults()`/`Load()`, and this section's
YAML needs to be reflected in `configs/cardea-gateway.example.yaml`.

## Failure modes

| Condition | Result |
| --- | --- |
| No `Authorization` header, identification enabled | 401, identical denial body; audit reason `no_credential` |
| Header not `Bearer <token68>` (wrong scheme, empty, or invalid charset) | 401, identical denial; audit reason `unrecognized_shape` |
| API-key-shaped credential matches no registered agent | 401, identical denial; audit reason `no_match` |
| JWT-shaped credential: unreadable claims, unknown `sub`, `sub` names an agent with no JWT material, non-HS256, or bad signature | 401, identical denial; audit reason `no_match` (plus claimed `agent_id` if `sub` was readable) |
| JWT-shaped credential: valid signature, resolved agent, but outside `exp`/`nbf`, or missing `exp` entirely | 401, identical denial; audit reason `outside_validity_window` (plus claimed `agent_id`) |
| Valid API key, or valid/in-window JWT | Allowed; `Call.AgentID` set; no new log line |
| Zero agents registered, identification enabled | Every call denied — documented expected behavior (functional spec rule 8), not a startup failure |
| Two `agents` entries share an `agent_id`, or one sets neither `api_key_env` nor `jwt_secret_env` | `config.Load` fails; process exits before listening |
| `api_key_env`/`jwt_secret_env` names an unset or empty environment variable | `identity.NewRegistry` fails (surfaced through `gateway.New`); process exits before listening |
| `jwt_secret_env` value shorter than 32 bytes | `identity.NewRegistry` fails; process exits before listening |
| Two agents register the identical literal API key | `identity.NewRegistry` fails (ambiguous match); process exits before listening |
| `identityStage.Request` panics (e.g. a wiring bug leaves the registry nil) | Recovered by the pipeline's existing panic handling (specs/0001); 503 `gateway_unavailable` |
| Identification disabled | Stage absent from `buildStages`; no `Authorization` header required; `Call.AgentID` always `""` |

Requests fail **closed**, per specs/0002 (functional) § *Failure
behavior*: every identification failure denies the call, and this stage
introduces no failure mode beyond what specs/0001 already defines for a
stage's error vs. its denial.

## Testing

`internal/identity` (new; table-driven):

- Registry construction rejects an unset/empty named env var, a
  `jwt_secret_env` under 32 bytes, and two agents sharing one literal API
  key; accepts a mixed registry (API-key-only, JWT-only, and
  both-shapes agents).
- Shape detection: missing header → `no_credential`; empty Bearer value,
  non-Bearer scheme, or a value with a disallowed byte → `unrecognized_shape`;
  a 3-segment string with a non-base64url segment still falls to the
  API-key branch, not JWT.
- API-key matching is an exact, case-sensitive match; an empty registry
  denies with `no_match`.
- JWT matching: valid and in-window succeeds with the right `agent_id`;
  wrong secret, unregistered `sub`, a `sub` naming an API-key-only agent,
  and malformed claims all → `no_match`; expired, not-yet-valid, and
  missing `exp` entirely all → `outside_validity_window`; a token signed
  with anything other than HS256 (e.g. `none`, RS256) is rejected without
  consulting any agent's secret.
- Two agents registered, each with its own API key or JWT: authenticating
  each agent's credential resolves to that agent's own `agent_id` and
  never the other's (functional spec acceptance criterion 8) — run both
  directions in the same test so a swapped lookup would fail it.
- `ClaimedAgentID` is populated only where a `sub` was actually readable
  (bad-signature `no_match` and `outside_validity_window`) and always
  empty on the API-key branch.
- No field of any returned `Result` ever contains a substring of the raw
  input credential, asserted against both API-key and JWT inputs.

`internal/gateway` (`identity_test.go`, mirroring `tokens_test.go`):

- `identityStage.Name() == "identification"`.
- A valid credential allows the call and `Call.AgentID` matches the
  registered agent.
- Each of the four denial categories produces the same status and message
  but a distinct `Denial.Reason`, driven through a small fixed two-agent
  registry.
- `buildStages` prepends `identityStage` ahead of `tokenStage` when both
  are enabled, omits it when disabled, and `buildStages`/`gateway.New`
  return an error — not a partially built `*Gateway` — when
  `identity.NewRegistry` fails.
- Full-pipeline integration: identification enabled, no `Authorization`
  header, the request is denied before the stub provider is ever invoked.
- Full-pipeline integration: identification enabled with two agents, each
  registered under a different shape (one API key, one JWT); a request
  carrying each credential reaches the stub provider and the recorded
  `Call.AgentID` matches only the credential's own agent (criterion 8,
  exercised end to end rather than just at the `Registry` unit level).
- Zero agents registered, identification enabled: a proxied (non-healthz)
  request is denied the same way as the no-`Authorization`-header case,
  both with and without a credential presented (criterion 7).
- `/healthz` still bypasses the pipeline with identification enabled and
  zero agents registered.

`internal/config`:

- `defaults()` and an empty config file both produce
  `identification == false` and empty `Agents`, with an explicit
  assertion this differs from `token_counting`'s default.
- A well-formed `agents` list with distinct `agent_id`s and valid field
  combinations unmarshals cleanly.
- Duplicate `agent_id`, empty `agent_id`, and an entry with neither env
  field set are all rejected by `Load`.
- `CARDEA_GATEWAY_PIPELINE_STAGES_IDENTIFICATION=true` overrides the default.
- An unknown key inside an `agents` entry is rejected the same way
  `pipeline.stages` already rejects unknown keys.

## Alternatives considered

- **`agent_id` via `context.Context`** — rejected: specs/0001 already
  rejected passing `Call` itself through context for the same reason
  (trades compile-time checking for a fallible type assertion every stage
  would repeat). Narrowing that to just `agent_id` doesn't avoid the
  argument — a stage would still need a magic context key when
  `Decision`/`Call` are already the typed channel specs/0001 built for
  this.
- **A stage writing `Call.AgentID` directly, bypassing `Decision`** —
  rejected: violates the load-bearing invariant in `Call`'s own doc
  comment ("only the pipeline mutates it"), which exists so every
  mutation to shared per-call state funnels through one reviewable point.
  Carving out an exception here means every later stage spec would have
  to separately re-justify not doing the same.
- **Try an API-key lookup, then fall back to a JWT parse, with no
  structural detection** — rejected: two independent match attempts per
  credential leaves the denial-reason derivation ambiguous when both
  fail, and wastes a map lookup on every JWT. Structural detection (three
  dot-separated base64url segments) picks the code path deterministically
  up front, so exactly one reason-producing path runs per request.
- **RS256/ES256 support now, instead of HMAC-only** — rejected:
  asymmetric verification needs key management the functional spec
  explicitly put out of scope this version (no JWKS fetch, no external
  IdP). HS256 with a shared secret configured locally alongside the
  registry entry matches the functional spec's stated model exactly, and
  adding asymmetric support later is additive — a new `SigningMethod` and
  a new `AgentConfig` field — not a breaking change to
  `Registry`/`Authenticate`.
- **Fold identification into `internal/gateway`, like `tokenStage`, with
  no new package** — rejected: identification carries meaningfully more
  surface than the one-file, no-dependency `tokenStage` — registry
  construction and validation, credential parsing, and a new third-party
  JWT dependency. A separate `internal/identity` package gives
  `Authenticate` a gateway-independent unit boundary and keeps
  `golang-jwt` contained to one package instead of leaking into
  `internal/gateway`'s import graph.

## Open questions

- What a stage that needs `agent_id` (policies, rate-limiting, audit)
  should do when identification is disabled — deferred to each of those
  specs, per specs/0001's pattern of letting the dependent stage decide.
- Credential rotation and revocation without a restart — out of scope
  this version (see *Non-goals*), deferred until registries grow past a
  handful of agents.
- Whether the agent developer should ever see their own `agent_id` echoed
  back — deferred to the audit stage's spec.
- Should `Registry.Authenticate`'s API-key comparison use
  `crypto/subtle` for constant time, closing a narrow timing side
  channel a plain map lookup leaves open? No other part of the Gateway
  does constant-time comparison today, and network jitter likely
  dominates; worth a decision during the security-review pass before this
  ships.
- This version requires every JWT to carry an `exp` claim (see *Design*
  and *Packages and types*) rather than accepting a non-expiring token as
  perpetually valid — a signed credential with no validity window would
  otherwise defeat the point of choosing JWT over a plain API key.
  Revisit if an operator scenario needs a genuinely long-lived JWT; until
  then this isn't open, it's decided.
- Is `identity.Result`'s shape (`OK`/`AgentID`/`Reason`/`ClaimedAgentID`)
  right-sized for a first version, given only `internal/gateway` calls it
  today? Worth revisiting once the policies stage's spec shows whether it
  needs anything else off a `Result`.
- The 32-byte JWT-secret minimum is a judgment call tied to HS256's
  RFC 7518 recommendation, not something the functional spec pins down —
  confirm it's workable for operators generating and storing one secret
  per agent via the env-var pattern before registries scale up.

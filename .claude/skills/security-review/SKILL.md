---
name: security-review
description: This skill should be used when the user asks for a "security review", to "review security", "check for vulnerabilities", or before merging changes that touch config loading, logging, the gateway's HTTP surface, or provider/upstream handling in the Gateway.
---

# the Gateway security review

The Gateway's whole purpose is to sit in front of paid, credentialed LLM
APIs and mediate access to them, so the failure modes that matter most here
are: leaking upstream provider credentials, leaking prompt/response
content (which is often sensitive/PII), and being tricked into forwarding
requests somewhere the operator didn't intend. A generic Go security pass
misses these; this checklist is scoped to them.

## Scope

Review the pending diff on the current branch (`git diff main...HEAD` or
equivalent) unless the user names a different target (a specific commit,
PR, or the whole repo).

## Checklist

**Credential handling**
- Provider API keys/secrets must come from config/env, never be
  hardcoded or committed (check `configs/*.yaml` and any new example
  configs for real-looking keys).
- Credentials must never be written to logs (`internal/logging`, zap
  fields anywhere) or reflected back in HTTP error responses to callers.
- If a provider injects a secret on the caller's behalf (see the
  `add-provider` skill), confirm any caller-supplied `Authorization`
  header is overridden, not merged/appended — a client should not be able
  to smuggle its own creds through to a provider that expects the
  gateway's.

**Upstream routing / SSRF**
- The gateway proxies to exactly one operator-configured provider
  (`config.Config.Provider`) — there is deliberately no way for a caller
  to select or influence the upstream host (no `/v1/{provider}/` in the
  URL, no header-driven selection). Flag any change that reintroduces a
  request-time value (client input, a header, a rule) influencing which
  host gets contacted — that's an SSRF vector.
- New reverse-proxy or custom `Provider` implementations should validate
  `base_url` is absolute (see `NewHTTPProxy`'s checks) and reject
  scheme/host surprises rather than trusting config blindly if that
  config could ever come from a less-trusted source than the operator.

**Logging / observability**
- Request and response bodies can contain prompts, completions, and PII.
  Flag any log statement (`zap.String`, `zap.Any`, etc.) that could dump
  a full request/response body at `info` level or below — that data
  should be redacted, truncated, or gated behind an explicit debug flag.

**Gateway HTTP surface**
- New endpoints on the gateway's `http.ServeMux` (`internal/gateway`)
  should be checked for: authentication (the Gateway centralizes access
  to paid APIs — is an unauthenticated caller able to spend the
  operator's provider budget?), and whether internal error details
  (stack traces, wrapped errors) leak to the HTTP response instead of
  just the log.

**Config loading**
- Viper-based config (`internal/config`) reads from file paths and
  `CARDEA_GATEWAY_`-prefixed env vars. Flag anything that turns a
  request-time or client-supplied value into a config file path (path
  traversal) or that widens what env vars are trusted.

**Rules engine (once implemented)**
- If a rules file format exists yet (`internal/config.RulesConfig.Path`
  and its consumer), check that rules are data, not code — no
  `eval`-like execution of rule expressions from a file an operator
  didn't fully trust, and that a rule can't be used to disable other
  security controls (e.g. a circuit breaker rule that also strips auth).

**Dependencies**
- New dependencies in `go.mod`/`go.sum`: sanity-check they're
  reasonably maintained/reputable before pulling them into a gateway
  that sees API keys and prompt content.

**TLS**
- Any HTTP client construction should not disable TLS verification
  (`InsecureSkipVerify: true`) against upstream providers.

## Reporting

Use the `ReportFindings` tool, ranked most-severe first. For each finding,
give a concrete failure scenario (what input/config leads to what leak or
bypass), not just "this could be a problem." If nothing in scope has an
issue, report an empty findings list rather than manufacturing minor
nitpicks.

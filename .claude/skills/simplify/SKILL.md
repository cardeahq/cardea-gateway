---
name: simplify
description: This skill should be used when the user asks to "simplify this", "clean this up", "reduce duplication", or wants reuse/efficiency/altitude cleanups applied to the Gateway changes — not for hunting correctness bugs (use the code-review skill for that).
---

# the Gateway simplify

Review changed code for reuse, simplification, efficiency, and "altitude"
cleanups (comments/names that don't earn their place), then apply the
fixes directly. This is a quality pass, not a bug hunt.

## Conventions to enforce

- Business logic stays under `internal/`; `cmd/gateway/main.go` stays
  a one-line delegation to `cli.Execute()` — flag anything added there
  beyond wiring.
- Every package keeps a package-level doc comment stating its
  responsibility (see the top of `gateway.go`, `provider.go`, etc.) —
  update it if a change shifts what the package is for, rather than
  letting it go stale.
- Commands build their own dependencies inside `RunE` (see `serve.go`
  constructing `logging.New` → `provider.NewHTTPProxy` → `gateway.New` in
  sequence) rather than introducing package-level singletons — collapse
  any reintroduced global back into local construction.
- Errors are wrapped with `%w` and short context (`fmt.Errorf("parsing
  base URL %q for provider %q: %w", ...)`), not re-stated generic
  messages — check new error sites match this style instead of adding a
  parallel error-handling convention.
- Table-driven tests for anything with more than ~2 input variations (see
  `TestNewHTTPProxy_RejectsInvalidInput`, `httpproxy_test.go`) rather than
  repeated near-identical test functions.

## Things to look for and collapse

- **Duplicated provider/config wiring**: if a second place in the
  codebase starts reading `config.Config.Provider` and constructing a
  `provider.Provider` by hand instead of going through the pattern in
  `serve.go`, consolidate.
- **Config default drift**: `defaults()` and the `v.SetDefault(...)`
  calls in `config.Load()` currently say the same thing twice by
  necessity (Viper needs both) — don't "simplify" by dropping one, but do
  flag if a third, different-valued default shows up anywhere else.
- **Reintroducing caller-driven provider selection**: the Gateway
  deliberately forwards every path to one operator-configured provider
  (no `/v1/{provider}/`, no per-request lookup). Don't "simplify" a
  future rules-engine change back into that shape — provider selection
  must stay config/rule-driven, never derived from the request.
- **Unnecessary abstraction**: this is an early-stage codebase — don't
  introduce interfaces, options structs, or generic helpers for a single
  call site "for future flexibility." Three similar lines beat a
  premature abstraction (matches the parent CLAUDE.md's stance).
- **Comments that restate the code**: prefer letting names carry meaning;
  keep a comment only where it explains a non-obvious *why* (e.g. why
  `httpProxyProvider` exists separately from a custom `Provider` — see
  its doc comment — is worth keeping; restating what
  `NewSingleHostReverseProxy` does is not).

## Workflow

1. Identify the changed files (diff on current branch, or whatever scope
   the user names).
2. Apply simplifications directly — this skill fixes, it doesn't just
   report.
3. Re-run `go build ./... && go vet ./... && golangci-lint run ./... &&
   go test ./...` after changes to confirm nothing broke.

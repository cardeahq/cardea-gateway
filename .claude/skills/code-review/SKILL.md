---
name: code-review
description: This skill should be used when the user asks to "review this code", "code review", "review the diff", or wants correctness bugs found in the Gateway changes touching cli, config, gateway, logging, or provider packages.
---

# the Gateway code review

Review the pending diff for correctness bugs, scoped to patterns specific
to this codebase's stack (Cobra, Viper, zap, `net/http`/`httputil`). Don't
re-litigate generic Go style already enforced by `golangci-lint run
./...` — run that first and treat its output as ground truth for
formatting/vet-level issues; focus review time on logic bugs it can't
catch.

## Where bugs tend to hide in this codebase

**`internal/cli`**
- Each subcommand's `RunE` must build its own logger/gateway/provider
  rather than reaching for package-level state (see CLAUDE.md
  conventions) — flag any new global/singleton.
- Errors from `RunE` should propagate via `return err`, not be logged and
  swallowed — `Execute()` is the single place that prints to stderr and
  sets the exit code.
- Context handling: long-running commands should derive from
  `cmd.Context()` (as `serve.go` does via `signal.NotifyContext`), not
  `context.Background()`, or they won't respect cancellation.

**`internal/config`**
- New fields need all three: a struct field with `mapstructure` tag, a
  default in `defaults()` *and* a matching `v.SetDefault(...)` call in
  `Load()` (these two are easy to let drift apart), and documentation in
  `configs/cardea-gateway.example.yaml`.
- `v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))` means nested keys
  with underscores in their own name can collide with the dot-replacement
  scheme — check new field names for this.

**`internal/gateway`**
- Route handlers registered on the `http.ServeMux` must be added inside
  `New()` — check that a new handler isn't just added ad hoc and forgotten
  from the mux registration list.
- `Run()`'s shutdown path (`ctx.Done()` → `Shutdown`) vs. the serve-error
  path (`errCh`) — a new code path added to `Run` should preserve that
  `http.ErrServerClosed` from a graceful shutdown-triggered `Serve()`
  return isn't treated as a real error.
- The `/` catch-all forwards everything not matched by a more specific
  pattern (currently just `/healthz`) straight to the configured
  provider. Any new specific route added here narrows what reaches the
  provider unmodified — check it's intentional, since the Gateway's
  contract is a transparent single entrypoint.

**`internal/provider`**
- `NewHTTPProxy`/any new `Provider` constructor should reject invalid
  input (empty name, non-absolute URL) rather than constructing a
  Provider that fails at request time — check new constructors follow
  this validate-early pattern.
- Exactly one `Provider` is active per gateway instance (from
  `config.Config.Provider`); if a change reintroduces selecting between
  multiple providers, check where the selection logic lives — it must be
  config/rule-driven, never derived from the inbound request (see the
  `security-review` skill's SSRF section).

**`internal/logging`**
- Logger construction errors (bad level/format string) should be
  returned, not defaulted-and-ignored — silently falling back to a
  default level/format would hide operator misconfiguration.

## Reporting

Use the `ReportFindings` tool, ranked most-severe first, each with a
concrete failure scenario (specific input/config → wrong behavior), not
generic advice. Empty list if nothing survives scrutiny.

---
name: add-provider
description: This skill should be used when the user asks to "add a provider", "add an LLM provider", "wire up OpenAI/Anthropic/Gemini/etc.", "support a new upstream", or otherwise wants the Gateway to route requests to a new LLM backend.
---

# Add an LLM provider to the Gateway

The Gateway forwards every request to exactly one upstream LLM provider,
configured by the operator — never chosen by the caller or exposed in the
URL (`/healthz` aside, every path forwards as-is to that one provider's
`Handler()`; see `internal/gateway/gateway.go`). `config.Config.Provider`
holds which one; Anthropic is the only one implemented today. Switching
providers at runtime based on rules is a future concern, not something a
caller can trigger.

## The `Provider` interface

Defined in `internal/provider/provider.go`:

```go
type Provider interface {
    Name() string
    Handler() http.Handler
    ResponseFormat() trace.ResponseFormat
}
```

`ResponseFormat` is how tracing reads the provider's responses (response
ID, model, stop reason, token usage, tool calls). It returns nil when the
Gateway does not know the upstream's wire format; responses are then
relayed and traced without any of those fields. A format implements
`trace.ResponseFormat` (`ReadBody` for a non-streamed JSON body,
`NewStreamReader` for one SSE stream) and reports what it reads through
the `*trace.ResponseSink` it is given. Everything format-neutral
(encodings, SSE framing, read limits, disconnects, timing, fingerprints)
stays in the tap. See `anthropic_format.go` and `mistral_format.go`.

## Two ways to add a provider

### 1. Plain reverse proxy (most providers)

If the upstream can be reached by forwarding the request as-is (method,
path, body, most headers unchanged), no new code is needed — just point
config at it:

```yaml
provider:
  name: openai
  base_url: "https://api.openai.com"
```

This is built via `provider.NewHTTPProxy` (see
`internal/provider/httpproxy.go`), constructed directly in
`internal/cli/serve.go` from `cfg.Provider`. Its `ResponseFormat` is nil,
so tracing records no usage or tool calls for it.

### 2. Custom provider (auth injection, request rewriting, streaming quirks)

If the upstream needs something a plain reverse proxy can't do — e.g.
injecting an API key header from a secret rather than a client-supplied
one, rewriting the request path/body, or special SSE handling — implement
`Provider` directly:

1. Add a new file `internal/provider/<name>.go` with a type implementing
   `Provider`. Model it on
   `httpProxyProvider` in `httpproxy.go` — typically an
   `httputil.ReverseProxy` with a custom `Director` or
   `ModifyResponse` for the parts that differ.
2. Give it a constructor (`New<Name>Provider(cfg ...) (Provider, error)`)
   that validates its inputs and returns an error rather than panicking,
   matching `NewHTTPProxy`'s validation style (reject empty names,
   non-absolute URLs, etc.).
3. If the upstream's wire format is new, add
   `internal/provider/<name>_format.go` implementing `trace.ResponseFormat`
   and return it from `ResponseFormat()`; a provider speaking an existing
   format returns that one. Test it through a real tap, as
   `mistral_format_test.go` does: a body, a stream (with a tool call), and
   a response missing usage.
4. Wire it into `internal/cli/serve.go` where the provider is
   constructed — e.g. dispatch on the configured `Name` to pick
   `NewHTTPProxy` vs. the new constructor.
5. Add a test file `internal/provider/<name>_test.go` following the
   pattern in `httpproxy_test.go`: spin up an `httptest.Server` standing
   in for the upstream, assert the request the provider actually sends
   (headers, path, body) and that the response comes back untouched.

## Checklist before calling it done

- [ ] `go build ./... && go vet ./... && golangci-lint run ./...` clean.
- [ ] New provider has a table-driven or per-case test asserting what it
      sends upstream (not just that *a* request goes out).
- [ ] Config docs updated in `configs/cardea-gateway.example.yaml`.
- [ ] Secrets (API keys) come from config/env, never hardcoded, and are
      never logged — see the `security-review` skill's credential-handling
      checklist, which applies to any new provider.
- [ ] If the provider needs a secret the gateway injects on the caller's
      behalf, confirm the caller-supplied `Authorization` header (if any)
      is stripped/overridden rather than merely appended to.

# agentkeeper

agentkeeper is an LLM gateway that shields and contains AI usage with
security, privacy, observability, and easy LLM provider switching. It sits
between applications and LLM providers, enforcing user-defined rules —
custom circuit breakers, token compaction, and more.

## Requirements

- Go 1.26+

## Build

```sh
make build
```

This produces `bin/agentkeeper`.

## Run

```sh
make run
# or
go run ./cmd/agentkeeper serve
```

## Configuration

Configuration is loaded (in increasing priority) from built-in defaults, a
YAML config file, and `AGENTKEEPER_`-prefixed environment variables. See
[`configs/agentkeeper.example.yaml`](configs/agentkeeper.example.yaml) for
all available keys. Point at a specific file with `--config`:

```sh
agentkeeper serve --config ./agentkeeper.yaml
```

## Development

```sh
make test   # run tests
make lint   # run golangci-lint
make fmt    # gofmt the tree
```

## Usecases

### Use agentkeeper with opencode

[opencode](https://opencode.ai) talks to Anthropic like any other client, so
pointing it at agentkeeper instead is just a matter of redirecting its
Anthropic provider's `baseURL`.

1. Run agentkeeper with a real Anthropic API key in its own environment —
   this key never needs to be shared with opencode or any other caller:

   ```sh
   export ANTHROPIC_API_KEY=sk-ant-...   # in agentkeeper's environment only
   agentkeeper serve --config ./agentkeeper.yaml
   ```

   Listens on `:8080` by default.

2. Point opencode's Anthropic provider at agentkeeper. Add this to your
   project's `opencode.json` (or the global `~/.config/opencode/opencode.json`):

   ```json
   {
     "$schema": "https://opencode.ai/config.json",
     "provider": {
       "anthropic": {
         "options": {
           "baseURL": "http://localhost:8080",
           "apiKey": "not-needed"
         }
       }
     }
   }
   ```

   The `apiKey` value is a placeholder — agentkeeper strips whatever
   credential the caller sends and injects the real `ANTHROPIC_API_KEY`
   from its own environment (see `internal/provider/anthropic.go`), so
   opencode never needs a valid Anthropic key of its own.

3. Use opencode normally. Every request it sends now flows through
   agentkeeper — check the gateway's logs for the `token usage` lines to
   see sent/received token counts per request.

### Use agentkeeper with Claude Code

Claude Code reads `ANTHROPIC_BASE_URL` at startup and sends every request
there instead of `api.anthropic.com` — this is the same mechanism it uses
for any organization-run LLM gateway.

1. Run agentkeeper with a real Anthropic API key in its own environment —
   this key never needs to be shared with Claude Code or any other caller:

   ```sh
   export ANTHROPIC_API_KEY=sk-ant-...   # in agentkeeper's environment only
   agentkeeper serve --config ./agentkeeper.yaml
   ```

   Listens on `:8080` by default.

2. Point Claude Code at agentkeeper by setting `ANTHROPIC_BASE_URL` and a
   credential variable in the shell you launch it from:

   ```sh
   export ANTHROPIC_BASE_URL=http://localhost:8080
   export ANTHROPIC_AUTH_TOKEN=not-needed
   claude
   ```

   `ANTHROPIC_AUTH_TOKEN` is a placeholder — agentkeeper strips whatever
   credential the caller sends and injects the real `ANTHROPIC_API_KEY`
   from its own environment (see `internal/provider/anthropic.go`), so
   Claude Code never needs a valid Anthropic key of its own.

   To make this persist across terminals instead of exporting it each
   time, put both variables in the `env` block of `~/.claude/settings.json`
   (not a project's `.claude/settings.json`, which gets committed):

   ```json
   {
     "env": {
       "ANTHROPIC_BASE_URL": "http://localhost:8080",
       "ANTHROPIC_AUTH_TOKEN": "not-needed"
     }
   }
   ```

3. Confirm it's routing through agentkeeper: send Claude Code a message,
   then run `/status` — the `Anthropic base URL` line should show
   `http://localhost:8080`. Check agentkeeper's logs for the `token usage`
   lines to see sent/received token counts per request.


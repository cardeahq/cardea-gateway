# Provider Agent Identification Configurations

This directory contains **agent identification configuration files** for Cardea Gateway. These files define how the gateway recognizes different AI agent types (Mistral Vibe, OpenCode, Claude Code) by matching specific request patterns.

## What Are These Files?

Each YAML file in this directory defines an **agent type** that Cardea Gateway can identify. When enabled, the gateway uses these configurations to:

- **Detect** which AI agent is making requests based on HTTP headers, body fields, or other request characteristics
- **Extract** session identifiers from recognized agents for tracing and analytics
- **Categorize** traffic by agent type in logs and monitoring

### File Structure

Each provider file contains:

```yaml
agent_id: "unique-identifier"      # Internal name for this agent type
match:                             # Rules to identify this agent
  - header: "header-name"          # Header to check
    pattern: "regex-pattern"      # Regex pattern to match
    prefix: "prefix-string"        # Alternative: simple prefix match
session_sources:                   # Where to extract session IDs
  - header: "header-name"          # From a header
  - body_field: "json.path"        # From a JSON body field
    pattern: "regex-pattern"      # Optional: extract with regex
```

## Available Provider Configurations

| File                | Agent        | Identification Method                                                         | Session Source                                                            |
| ------------------- | ------------ | ----------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| `mistral_vibe.yaml` | Mistral Vibe | User-Agent header contains `Mistral-Vibe/`                                    | `metadata.session_id` in request body                                     |
| `opencode.yaml`     | OpenCode     | `x-opencode-session` header present                                           | `x-opencode-session` header                                               |
| `claude_code.yaml`  | Claude Code  | `X-Claude-Code-Session-Id` header **or** User-Agent starts with `claude-cli/` | `X-Claude-Code-Session-Id` header, fallback to `metadata.user_id` in body |

## How to Use

### 1. Enable Agent Identification

In your `cardea-gateway.yaml` configuration:

```yaml
pipeline:
  stages:
    identification: true  # Enable the identification stage

identification:
  agent_types_dir: "configs/providers"  # Path to this directory
```

### 2. Configuration Options

#### Basic Setup
```yaml
identification:
  agent_types_dir: "configs/providers"
```

This loads all `.yaml` files from the specified directory as agent type definitions.

#### Custom Directory
You can place these files in any directory and point to it:
```yaml
identification:
  agent_types_dir: "/path/to/your/agent/configs"
```

#### Disable Identification
```yaml
pipeline:
  stages:
    identification: false
```

### 3. Default Session Sources

If an agent is recognized but has no session sources defined (or they don't match), the gateway falls back to `default_session_sources`:

```yaml
tracing:
  default_session_sources:
    - header: X-Session-Id
    - baggage: session.id
```

## Customizing Provider Configurations

### Add a New Agent Type

1. Create a new `.yaml` file in this directory (e.g., `my_agent.yaml`)
2. Define the identification rules:

```yaml
agent_id: "my-agent"
match:
  - header: "user-agent"
    pattern: "My-Agent/"
  - header: "x-my-agent-version"
    pattern: ".*"
session_sources:
  - header: "x-my-agent-session"
  - body_field: "metadata.session_id"
```

### Modify Existing Configurations

Edit the respective file to:
- Add additional match rules
- Change session source extraction
- Update patterns for new agent versions

**Note:** Changes take effect on gateway restart or when the identification stage reloads (if watching is enabled).

## How Identification Works

1. **Request arrives** at Cardea Gateway
2. **Identification stage** checks the request against all loaded agent type definitions
3. **First matching** agent type is assigned to the request
4. **Session ID** is extracted using the agent's `session_sources` (in order)
5. **Agent type and session** are attached to the request metadata for:
   - Tracing and analytics
   - Policy decisions
   - Logging and monitoring

## Version Control

These files contain **shared defaults** and should be committed to version control. They define the standard agent identification rules for your Cardea Gateway deployment.

For **local overrides** or environment-specific configurations, use the `identification.local/` directory (which should be git-ignored) or environment-specific configuration files.

## Related Configuration

- `configs/identification.local/` - Local overrides (not version controlled)
- `configs/identification.example/` - Example configurations
- `cardea-gateway.yaml` - Main gateway configuration

## References

- [Cardea Gateway Documentation](https://github.com/cardeahq/cardea-gateway)
- [Agent Identification Spec](specs/0009-agent-identification.md) (if available)
- [Tracing Configuration](../cardea-gateway.example.yaml) - See the `tracing` section
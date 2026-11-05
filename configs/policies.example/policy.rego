package cardea.policy

import rego.v1

# Stop a conversation once a destructive bash call is in its history
deny contains "destructive-bash-in-history" if {
	some c in input.tool_calls
	c.name == "bash"
	regex.match(`rm\s+-rf\s+(/|~)|mkfs\.|dd\s+if=.*of=/dev/`, c.arguments.command)
}

# Agent and model pairs that are never allowed, e.g. keep a coding agent off
# a provider. Patterns use glob syntax: "mistral-*" matches every Mistral model.
# deny contains "agent-model-forbidden" if {
# 	some pattern in data.forbidden_agent_models[input.agent.id]
# 	glob.match(pattern, null, input.model)
# }

# Cost guardrails: data.limits, overridable per agent in data.agents
limit(name) := object.get(data.agents, [object.get(input, ["agent", "id"], ""), name], data.limits[name])

deny contains "max-tokens-too-high" if input.parameters.max_tokens > limit("max_tokens")

deny contains "request-too-large" if input.call.size_bytes > limit("max_request_bytes")

# Weekend freeze for everyone except agent-alpha
deny contains "weekend-freeze" if {
	input.agent.id != "agent-alpha"
	time.weekday(time.now_ns()) in {"Saturday", "Sunday"}
}

# Instructions smuggled into the system prompt: clients load repo files
# such as AGENTS.md into it on every call. A demo-grade heuristic: a reworded
# attack gets through.
deny contains "prompt-injection-in-system-prompt" if {
	some p in data.injection_patterns
	regex.match(p, lower(input.system))
}

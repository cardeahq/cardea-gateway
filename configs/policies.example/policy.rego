package cardea.policy

import rego.v1

# 1. Per-agent model allow-list (data-driven, see data.json)
deny contains "model-not-allowed" if {
	allowed := data.agents[input.agent.id].models
	not input.model in allowed
}

# 2. Never offer dangerous tools
deny contains "forbidden-tool-offered" if {
	some t in input.tools
	t.name in data.forbidden_tools
}

# 3. Credentials in the prompt or tool results
secret_patterns := {
	`AKIA[0-9A-Z]{16}`,
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`,
	`sk-ant-[A-Za-z0-9_-]{20,}`,
	`ghp_[A-Za-z0-9]{36}`,
}

deny contains "secret-in-tool-result" if {
	some r in input.tool_results
	some p in secret_patterns
	regex.match(p, r.text)
}

deny contains "secret-in-message" if {
	some m in input.messages
	some part in m.content
	part.type == "text"
	some p in secret_patterns
	regex.match(p, part.text)
}

# 4. Stop a conversation once a destructive bash call is in its history
deny contains "destructive-bash-in-history" if {
	some c in input.tool_calls
	c.name == "bash"
	regex.match(`rm\s+-rf\s+(/|~)|mkfs\.|dd\s+if=.*of=/dev/`, c.arguments.command)
}

# 5. Sensitive paths sent to the LLM
deny contains "secret-path-in-tool-call" if {
	some c in input.tool_calls
	some arg in c.arguments
	is_string(arg)
	some prefix in data.blocked_path_prefixes
	startswith(arg, prefix)
}

# 6. Cost guardrails: data.limits, overridable per agent in data.agents
limit(name) := object.get(data.agents, [object.get(input, ["agent", "id"], ""), name], data.limits[name])

deny contains "max-tokens-too-high" if input.parameters.max_tokens > limit("max_tokens")

deny contains "request-too-large" if input.call.size_bytes > limit("max_request_bytes")

# 7. Weekend freeze for everyone except agent-alpha
deny contains "weekend-freeze" if {
	input.agent.id != "agent-alpha"
	time.weekday(time.now_ns()) in {"Saturday", "Sunday"}
}

# 8. Default-deny for unknown agents. Needs identification: true, otherwise
# input.agent.id is undefined and every call is denied.
# default allow := false
# allow if data.agents[input.agent.id]
# deny contains "unknown-agent" if not allow

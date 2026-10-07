package cardea.policy_test

import rego.v1

import data.cardea.policy

base := {"agent": {"id": "agent-beta"}, "model": "claude-haiku-4-5-20251001", "tools": [], "tool_calls": [], "tool_results": [], "messages": [], "parameters": {"max_tokens": 1024}, "call": {"size_bytes": 100}}

# Weekend freeze depends on the clock, so it is not exercised here.
test_clean_request_allowed if {
	not "model-not-allowed" in policy.deny with input as base
}

test_wrong_model_denied if {
	"model-not-allowed" in policy.deny with input as object.union(base, {"model": "claude-opus-5-5"})
}

test_forbidden_tool_denied if {
	"forbidden-tool-offered" in policy.deny with input as object.union(base, {"tools": [{"name": "delete_database"}]})
}

test_secret_in_tool_result_denied if {
	"secret-in-tool-result" in policy.deny with input as object.union(base, {"tool_results": [{"text": "key AKIAABCDEFGHIJKLMNOP"}]})
}

test_secret_in_message_denied if {
	"secret-in-message" in policy.deny with input as object.union(base, {"messages": [{"role": "user", "content": [{"type": "text", "text": "-----BEGIN RSA PRIVATE KEY-----"}]}]})
}

test_destructive_bash_denied if {
	"destructive-bash-in-history" in policy.deny with input as object.union(base, {"tool_calls": [{"name": "bash", "arguments": {"command": "rm -rf /"}}]})
}

test_blocked_path_denied if {
	"secret-path-in-tool-call" in policy.deny with input as object.union(base, {"tool_calls": [{"name": "read", "arguments": {"path": "/secrets/key"}}]})
}

test_max_tokens_denied if {
	"max-tokens-too-high" in policy.deny with input as object.union(base, {"parameters": {"max_tokens": 100000}})
}

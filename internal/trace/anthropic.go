package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// messagesRequest is the part of an Anthropic Messages request tracing
// reads.
type messagesRequest struct {
	Model    *string           `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
}

func parseMessagesRequest(body []byte) (messagesRequest, bool) {
	var req messagesRequest
	if json.Unmarshal(body, &req) != nil {
		return messagesRequest{}, false
	}
	return req, true
}

type toolResultBlock struct {
	id      string
	isError bool
	content json.RawMessage
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// newToolResults reads the tool_result blocks of the last message, which
// are the ones answering the previous response. Earlier messages resend
// results already recorded with the call that first carried them.
// Trailing system messages are skipped: agents such as Claude Code append
// one after the message carrying the tool results.
func newToolResults(messages []json.RawMessage) ([]toolResultBlock, error) {
	var last message
	for i := len(messages) - 1; i >= 0; i-- {
		if json.Unmarshal(messages[i], &last) != nil {
			return nil, nil
		}
		if last.Role != "system" {
			break
		}
	}
	if last.Role != "user" {
		return nil, nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(last.Content), []byte("[")) {
		return nil, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(last.Content, &blocks) != nil {
		return nil, nil
	}

	var out []toolResultBlock
	for i, raw := range blocks {
		var b struct {
			Type      string          `json:"type"`
			ToolUseID json.RawMessage `json:"tool_use_id"`
			IsError   *bool           `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			var typed struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(raw, &typed) == nil && typed.Type != "tool_result" {
				continue
			}
			return nil, fmt.Errorf("last message block %d: tool_result cannot be read", i)
		}
		if b.Type != "tool_result" {
			continue
		}
		var id string
		if json.Unmarshal(b.ToolUseID, &id) != nil || id == "" {
			return nil, fmt.Errorf("last message block %d: tool_result has no tool_use_id string", i)
		}
		res := toolResultBlock{id: id, content: b.Content}
		if b.IsError != nil {
			res.isError = *b.IsError
		}
		out = append(out, res)
	}
	return out, nil
}

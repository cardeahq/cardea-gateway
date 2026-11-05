package policy

import (
	"encoding/json"
	"strings"
)

var (
	openAIChatExcluded       = map[string]bool{"model": true, "messages": true, "tools": true, "stream": true}
	openAICompletionExcluded = map[string]bool{"model": true, "prompt": true, "suffix": true, "stream": true}
	openAIEmbeddingsExcluded = map[string]bool{"model": true, "input": true}
)

// addOpenAIChat adds the conversation groups of a chat completions request.
func addOpenAIChat(input map[string]any, r Request) error {
	top, err := decodeTop(r.Body)
	if err != nil {
		return err
	}
	if err := addModelAndStream(input, top); err != nil {
		return err
	}
	if input["parameters"], err = parametersOf(top, openAIChatExcluded); err != nil {
		return err
	}

	var rawMessages []json.RawMessage
	if err := json.Unmarshal(top["messages"], &rawMessages); err != nil || rawMessages == nil {
		return errBadMessages
	}
	messages := make([]any, 0, len(rawMessages))
	var calls, results []map[string]any
	var system []string
	hasSystem := false
	for _, raw := range rawMessages {
		msg, c, res, text, err := normalizeOpenAIMessage(raw)
		if err != nil {
			return err
		}
		if role, _ := msg["role"].(string); role == "system" || role == "developer" {
			hasSystem = true
			system = append(system, text)
		}
		messages = append(messages, msg)
		calls = append(calls, c...)
		results = append(results, res...)
	}
	if hasSystem {
		input["system"] = strings.Join(system, "\n")
	}

	tools, err := normalizeOpenAITools(top["tools"])
	if err != nil {
		return err
	}
	setConversation(input, messages, calls, results, tools)
	return nil
}

type openAIPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// normalizeOpenAIMessage returns the normalized message, its tool calls and
// tool results, and its text (for system and developer messages). A
// tool-role message becomes a user message carrying one tool_result.
func normalizeOpenAIMessage(raw json.RawMessage) (msg map[string]any, calls, results []map[string]any, text string, err error) {
	var m struct {
		Role       json.RawMessage `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, nil, "", errBadBody
	}
	var role string
	if len(m.Role) > 0 {
		if err := json.Unmarshal(m.Role, &role); err != nil {
			return nil, nil, nil, "", errBadBody
		}
	}
	msg = map[string]any{}
	if role != "" {
		msg["role"] = role
	}

	if role == "tool" {
		text, err := resultText(m.Content)
		if err != nil {
			return nil, nil, nil, "", err
		}
		result := map[string]any{"tool_call_id": m.ToolCallID, "is_error": false, "size_bytes": len(text), "text": text}
		msg["role"] = "user"
		msg["content"] = []any{map[string]any{
			"type": "tool_result", "tool_call_id": m.ToolCallID, "is_error": false, "size_bytes": len(text), "text": text,
		}}
		return msg, nil, []map[string]any{result}, "", nil
	}

	content := []any{}
	var texts []string
	switch {
	case len(m.Content) == 0 || string(m.Content) == "null":
	case json.Unmarshal(m.Content, &text) == nil:
		content = append(content, map[string]any{"type": "text", "text": text})
		texts = append(texts, text)
	default:
		var parts []openAIPart
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			return nil, nil, nil, "", errBadBody
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": p.Text})
				texts = append(texts, p.Text)
			case "image_url":
				content = append(content, map[string]any{"type": "image"})
			case "input_audio":
				content = append(content, map[string]any{"type": "audio"})
			default:
				content = append(content, map[string]any{"type": p.Type})
			}
		}
	}

	for _, tc := range m.ToolCalls {
		args, err := toolArguments(tc.Function.Arguments)
		if err != nil {
			return nil, nil, nil, "", err
		}
		content = append(content, map[string]any{"type": "tool_call", "id": tc.ID, "name": tc.Function.Name, "arguments": args})
		calls = append(calls, map[string]any{"id": tc.ID, "name": tc.Function.Name, "arguments": args})
	}
	msg["content"] = content
	return msg, calls, nil, strings.Join(texts, "\n"), nil
}

// toolArguments decodes the JSON held in an arguments string. A string
// that is not JSON stays a string.
func toolArguments(s string) (any, error) {
	if !json.Valid([]byte(s)) {
		return s, nil
	}
	if err := rejectDuplicateKeys([]byte(s)); err != nil {
		return nil, err
	}
	return decodeValue(json.RawMessage(s))
}

func normalizeOpenAITools(raw json.RawMessage) ([]any, error) {
	tools := []any{}
	if len(raw) == 0 || string(raw) == "null" {
		return tools, nil
	}
	var in []struct {
		Type     string `json:"type"`
		Function *struct {
			Name        string          `json:"name"`
			Description *string         `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errBadBody
	}
	for _, t := range in {
		if t.Function == nil {
			tools = append(tools, map[string]any{"type": t.Type})
			continue
		}
		tool := map[string]any{"name": t.Function.Name}
		if t.Function.Description != nil {
			tool["description"] = *t.Function.Description
		}
		if len(t.Function.Parameters) > 0 {
			schema, err := decodeValue(t.Function.Parameters)
			if err != nil {
				return nil, errBadBody
			}
			tool["parameters"] = schema
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// addOpenAICompletion exposes a FIM request's prompt and suffix as one
// user message.
func addOpenAICompletion(input map[string]any, r Request) error {
	top, err := decodeTop(r.Body)
	if err != nil {
		return err
	}
	if err := addModelAndStream(input, top); err != nil {
		return err
	}
	if input["parameters"], err = parametersOf(top, openAICompletionExcluded); err != nil {
		return err
	}
	var texts []string
	for _, k := range []string{"prompt", "suffix"} {
		raw, ok := top[k]
		if !ok || string(raw) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return errBadBody
		}
		texts = append(texts, s)
	}
	setConversation(input, userTextMessage(texts), nil, nil, nil)
	return nil
}

// addOpenAIEmbeddings exposes an embeddings request's string inputs as one
// user message; token-ID inputs carry no text and are skipped.
func addOpenAIEmbeddings(input map[string]any, r Request) error {
	top, err := decodeTop(r.Body)
	if err != nil {
		return err
	}
	if err := addModelAndStream(input, top); err != nil {
		return err
	}
	if input["parameters"], err = parametersOf(top, openAIEmbeddingsExcluded); err != nil {
		return err
	}
	var texts []string
	if raw, ok := top["input"]; ok && string(raw) != "null" {
		var one string
		var many []json.RawMessage
		switch {
		case json.Unmarshal(raw, &one) == nil:
			texts = append(texts, one)
		case json.Unmarshal(raw, &many) == nil:
			for _, item := range many {
				var s string
				if json.Unmarshal(item, &s) == nil {
					texts = append(texts, s)
				}
			}
		default:
			return errBadBody
		}
	}
	setConversation(input, userTextMessage(texts), nil, nil, nil)
	return nil
}

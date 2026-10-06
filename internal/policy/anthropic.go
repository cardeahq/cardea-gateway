package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	errBadBody     = errors.New("request body is not a valid conversation request")
	errBadMessages = errors.New("messages is not an array")
)

// conversationExcluded are the top-level fields that are not parameters.
var conversationExcluded = map[string]bool{"model": true, "messages": true, "system": true, "tools": true, "stream": true}

// addAnthropicConversation adds the conversation groups of a Messages or
// count_tokens request to input. Errors never carry body content.
func addAnthropicConversation(input map[string]any, body []byte) error {
	if err := rejectDuplicateKeys(body); err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return errBadBody
	}

	if raw, ok := top["model"]; ok {
		model, err := decodeValue(raw)
		if err != nil {
			return errBadBody
		}
		input["model"] = model
	}
	stream := false
	if raw, ok := top["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	input["stream"] = stream

	parameters := map[string]any{}
	for k, raw := range top {
		if conversationExcluded[k] {
			continue
		}
		v, err := decodeValue(raw)
		if err != nil {
			return errBadBody
		}
		parameters[k] = v
	}
	input["parameters"] = parameters

	if raw, ok := top["system"]; ok {
		system, err := systemText(raw)
		if err != nil {
			return err
		}
		if system != nil {
			input["system"] = *system
		}
	}

	var rawMessages []json.RawMessage
	if err := json.Unmarshal(top["messages"], &rawMessages); err != nil || rawMessages == nil {
		return errBadMessages
	}
	messages := make([]any, 0, len(rawMessages))
	toolCalls := []any{}
	var results []map[string]any
	callNames := map[string]string{}
	for _, raw := range rawMessages {
		msg, calls, res, err := normalizeMessage(raw)
		if err != nil {
			return err
		}
		messages = append(messages, msg)
		for _, c := range calls {
			toolCalls = append(toolCalls, c)
			if id, _ := c["id"].(string); id != "" {
				callNames[id], _ = c["name"].(string)
			}
		}
		results = append(results, res...)
	}
	toolResults := make([]any, 0, len(results))
	for _, r := range results {
		if name, ok := callNames[r["tool_call_id"].(string)]; ok {
			r["tool_name"] = name
		}
		toolResults = append(toolResults, r)
	}
	input["messages"] = messages
	input["tool_calls"] = toolCalls
	input["tool_results"] = toolResults

	tools, err := normalizeTools(top["tools"])
	if err != nil {
		return err
	}
	input["tools"] = tools
	return nil
}

// decodeValue decodes JSON keeping numbers as json.Number, so large
// integers survive.
func decodeValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// systemText is nil when there is no system prompt.
func systemText(raw json.RawMessage) (*string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return &s, nil
	}
	var parts []textPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		if string(bytes.TrimSpace(raw)) == "null" {
			return nil, nil
		}
		return nil, errBadBody
	}
	s = joinText(parts)
	return &s, nil
}

func joinText(parts []textPart) string {
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// normalizeMessage returns the message with normalized content blocks, plus
// its tool_call and tool_result blocks.
func normalizeMessage(raw json.RawMessage) (msg map[string]any, calls []map[string]any, results []map[string]any, err error) {
	var m struct {
		Role    json.RawMessage `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, nil, errBadBody
	}
	role, err := decodeValue(m.Role)
	if err != nil {
		role = nil
	}
	msg = map[string]any{}
	if role != nil {
		msg["role"] = role
	}

	content := []any{}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		content = append(content, map[string]any{"type": "text", "text": text})
		msg["content"] = content
		return msg, nil, nil, nil
	}
	var blocks []block
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, nil, nil, errBadBody
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.Text})
		case "tool_use":
			args := any(map[string]any{})
			if len(b.Input) > 0 {
				if args, err = decodeValue(b.Input); err != nil {
					return nil, nil, nil, errBadBody
				}
			}
			content = append(content, map[string]any{"type": "tool_call", "id": b.ID, "name": b.Name, "arguments": args})
			calls = append(calls, map[string]any{"id": b.ID, "name": b.Name, "arguments": args})
		case "tool_result":
			text, err := resultText(b.Content)
			if err != nil {
				return nil, nil, nil, err
			}
			content = append(content, map[string]any{
				"type": "tool_result", "tool_call_id": b.ToolUseID, "is_error": b.IsError,
				"size_bytes": len(text), "text": text,
			})
			results = append(results, map[string]any{
				"tool_call_id": b.ToolUseID, "is_error": b.IsError, "size_bytes": len(text), "text": text,
			})
		default:
			content = append(content, map[string]any{"type": b.Type})
		}
	}
	msg["content"] = content
	return msg, calls, results, nil
}

func resultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []textPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errBadBody
	}
	return joinText(parts), nil
}

func normalizeTools(raw json.RawMessage) ([]any, error) {
	tools := []any{}
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return tools, nil
	}
	var in []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description *string         `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, errBadBody
	}
	for _, t := range in {
		tool := map[string]any{"name": t.Name}
		if t.Description != nil {
			tool["description"] = *t.Description
		}
		switch {
		case len(t.InputSchema) > 0:
			schema, err := decodeValue(t.InputSchema)
			if err != nil {
				return nil, errBadBody
			}
			tool["parameters"] = schema
		case t.Type != "":
			tool["type"] = t.Type
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// rejectDuplicateKeys fails on the first repeated key in any object, at any
// depth. Go keeps the last of a repeated key and a provider may keep the
// first, so a rule and the provider could otherwise read different values.
// The error names a byte offset, never a key or a value.
func rejectDuplicateKeys(body []byte) error {
	type frame struct {
		keys      map[string]struct{} // nil for an array
		expectKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var stack []frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].keys != nil {
			stack[n-1].expectKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errBadBody
		}
		if n := len(stack); n > 0 && stack[n-1].keys != nil && stack[n-1].expectKey {
			if d, isDelim := tok.(json.Delim); !isDelim {
				key, _ := tok.(string)
				if _, dup := stack[n-1].keys[key]; dup {
					return fmt.Errorf("duplicate object key at byte offset %d", dec.InputOffset())
				}
				stack[n-1].keys[key] = struct{}{}
				stack[n-1].expectKey = false
				continue
			} else if d == '}' {
				stack = stack[:n-1]
				valueDone()
				continue
			}
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, frame{keys: map[string]struct{}{}, expectKey: true})
			case '[':
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		valueDone()
	}
}

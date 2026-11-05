package filtering

import (
	"encoding/json"
)

// redactChat rewrites an OpenAI-style chat body: every message's content,
// and the arguments of every tool call. Every other key is copied as raw
// bytes.
func (s *scan) redactChat(top map[string]json.RawMessage) error {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(top["messages"], &messages); err != nil || messages == nil {
		return errMessagesNotList
	}
	for _, m := range messages {
		if m == nil {
			return errNotTargetShape
		}
		if raw, ok := m["content"]; ok {
			redacted, err := s.redactChatContent(raw)
			if err != nil {
				return err
			}
			m["content"] = redacted
		}
		if raw, ok := m["tool_calls"]; ok && !isJSONNull(raw) {
			redacted, err := s.redactToolCalls(raw)
			if err != nil {
				return err
			}
			m["tool_calls"] = redacted
		}
	}
	enc, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	top["messages"] = enc
	return nil
}

// redactChatContent redacts a string whole, or the text of "text" parts;
// every other part is copied as raw bytes.
func (s *scan) redactChatContent(raw json.RawMessage) (json.RawMessage, error) {
	if isJSONNull(raw) {
		return raw, nil
	}
	if str, ok := decodeString(raw); ok {
		return json.Marshal(s.redactText(str))
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errNotTargetShape
	}
	for i, p := range parts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(p, &part); err != nil || part == nil {
			return nil, errNotTargetShape
		}
		if rawString(part["type"]) != "text" {
			continue
		}
		text, ok := decodeString(part["text"])
		if !ok {
			continue
		}
		enc, err := json.Marshal(s.redactText(text))
		if err != nil {
			return nil, err
		}
		part["text"] = enc
		if parts[i], err = json.Marshal(part); err != nil {
			return nil, err
		}
	}
	return json.Marshal(parts)
}

func (s *scan) redactToolCalls(raw json.RawMessage) (json.RawMessage, error) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, errNotTargetShape
	}
	for _, c := range calls {
		if c == nil {
			return nil, errNotTargetShape
		}
		rawFn, ok := c["function"]
		if !ok || isJSONNull(rawFn) {
			continue
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(rawFn, &fn); err != nil {
			return nil, errNotTargetShape
		}
		args, ok := fn["arguments"]
		if !ok || isJSONNull(args) {
			continue
		}
		redacted, err := s.redactArguments(args)
		if err != nil {
			return nil, err
		}
		fn["arguments"] = redacted
		if c["function"], err = json.Marshal(fn); err != nil {
			return nil, err
		}
	}
	return json.Marshal(calls)
}

// redactArguments redacts an OpenAI-style tool call's arguments string:
// as a JSON tree re-encoded to a string when it parses, else as text.
func (s *scan) redactArguments(raw json.RawMessage) (json.RawMessage, error) {
	str, ok := decodeString(raw)
	if !ok {
		return nil, errNotTargetShape
	}
	if !json.Valid([]byte(str)) {
		return json.Marshal(s.redactText(str))
	}
	tree, err := decodeJSONTree(json.RawMessage(str))
	if err != nil {
		return nil, errNotTargetShape
	}
	redacted, err := s.redactTree(tree)
	if err != nil {
		return nil, err
	}
	enc, err := json.Marshal(redacted)
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(enc))
}

// redactFIM redacts a fill-in-the-middle request's prompt and suffix.
func (s *scan) redactFIM(top map[string]json.RawMessage) error {
	for _, key := range []string{"prompt", "suffix"} {
		raw, ok := top[key]
		if !ok || isJSONNull(raw) {
			continue
		}
		str, ok := decodeString(raw)
		if !ok {
			return errNotTargetShape
		}
		enc, err := json.Marshal(s.redactText(str))
		if err != nil {
			return err
		}
		top[key] = enc
	}
	return nil
}

// redactEmbeddings redacts an embeddings input: a string, or each string
// element of an array. Token-ID elements carry no text and are copied as
// raw bytes.
func (s *scan) redactEmbeddings(top map[string]json.RawMessage) error {
	raw, ok := top["input"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	if str, ok := decodeString(raw); ok {
		enc, err := json.Marshal(s.redactText(str))
		if err != nil {
			return err
		}
		top["input"] = enc
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return errNotTargetShape
	}
	for i, item := range items {
		str, ok := decodeString(item)
		if !ok {
			continue
		}
		enc, err := json.Marshal(s.redactText(str))
		if err != nil {
			return err
		}
		items[i] = enc
	}
	enc, err := json.Marshal(items)
	if err != nil {
		return err
	}
	top["input"] = enc
	return nil
}

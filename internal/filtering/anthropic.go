package filtering

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	errNotTargetShape  = errors.New("filtering: body is not a valid conversation request")
	errMessagesNotList = errors.New("filtering: messages is not an array")
)

// buildTarget reports whether body is an Anthropic messages or
// count_tokens request (provider == "anthropic" and path is one of the
// two conversation routes). Non-target bodies return isTarget == false,
// err == nil — not a failure, nothing to scan. A target body that is not
// valid JSON, repeats a key at any depth, or whose top level is not an
// object, or whose messages is not an array, is a *FilterError{"input"}.
func buildTarget(providerName, path string, body []byte) (top map[string]json.RawMessage, isTarget bool, err error) {
	if providerName != "anthropic" {
		return nil, false, nil
	}
	op := operationName(path)
	if op != "messages" && op != "count_tokens" {
		return nil, false, nil
	}

	if err := rejectDuplicateKeys(body); err != nil {
		return nil, false, &FilterError{Kind: "input"}
	}
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, false, &FilterError{Kind: "input"}
	}
	if raw, ok := top["messages"]; ok && !isJSONNull(raw) {
		var messages []json.RawMessage
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, false, &FilterError{Kind: "input"}
		}
	}
	return top, true, nil
}

// scan carries the state of one Redact call. Engine itself holds no
// per-call data, so this is what every helper method hangs off — see
// *Design*, "No per-call state on Engine."
type scan struct {
	ctx    context.Context
	engine *Engine
	counts map[Category]int
}

// redactSystem rewrites a system prompt — a JSON string, or an array of
// content blocks — redacting only "text"-typed blocks' text (or the
// whole string, if it's the plain-string form). Any other block type is
// returned byte-identical. nil when raw is absent.
func (s *scan) redactSystem(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return s.redactTextOrTextBlocks(raw)
}

// redactTextOrTextBlocks redacts raw when it is a JSON string (the whole
// string), or an array of blocks (only "text"-typed ones), per the
// "system" and "tool_result" content scope. raw is returned unchanged
// when it is JSON null.
func (s *scan) redactTextOrTextBlocks(raw json.RawMessage) (json.RawMessage, error) {
	if isJSONNull(raw) {
		return raw, nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		// redactText cannot fail (specs/0007-technical, Design); no error
		// to check here any more.
		redacted := s.redactText(str)
		return json.Marshal(redacted)
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errNotTargetShape
	}
	for i, b := range blocks {
		if rawString(b["type"]) != "text" {
			continue
		}
		text, ok := decodeString(b["text"])
		if !ok {
			continue
		}
		redacted := s.redactText(text)
		enc, err := json.Marshal(redacted)
		if err != nil {
			return nil, err
		}
		blocks[i]["text"] = enc
	}
	return json.Marshal(blocks)
}

// redactMessages rewrites the messages array: each message's content —
// string or blocks — redacted via redactBlock. Every key on a message
// other than "content" is copied as raw bytes.
func (s *scan) redactMessages(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return raw, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, errMessagesNotList
	}
	for i, m := range messages {
		content, ok := m["content"]
		if !ok {
			continue
		}
		redacted, err := s.redactMessageContent(content)
		if err != nil {
			return nil, err
		}
		messages[i]["content"] = redacted
	}
	return json.Marshal(messages)
}

// redactMessageContent redacts a message's "content": a plain string
// (redacted whole), or an array of blocks, each redacted via redactBlock.
func (s *scan) redactMessageContent(raw json.RawMessage) (json.RawMessage, error) {
	if isJSONNull(raw) {
		return raw, nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		redacted := s.redactText(str)
		return json.Marshal(redacted)
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errNotTargetShape
	}
	for i, b := range blocks {
		out, err := s.redactBlock(b)
		if err != nil {
			return nil, err
		}
		blocks[i] = out
	}
	return json.Marshal(blocks)
}

// redactBlock redacts one content block by its "type", per the table in
// *Design*. Every key other than the one its type redacts ("text",
// "input" or "content") is copied as raw bytes, known or not.
func (s *scan) redactBlock(raw json.RawMessage) (json.RawMessage, error) {
	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, errNotTargetShape
	}
	switch rawString(block["type"]) {
	case "text":
		text, ok := decodeString(block["text"])
		if !ok {
			return raw, nil
		}
		redacted := s.redactText(text)
		enc, err := json.Marshal(redacted)
		if err != nil {
			return nil, err
		}
		block["text"] = enc
	case "tool_use":
		input, ok := block["input"]
		if !ok || isJSONNull(input) {
			break
		}
		tree, err := decodeJSONTree(input)
		if err != nil {
			return nil, errNotTargetShape
		}
		redactedTree, err := s.redactTree(tree)
		if err != nil {
			return nil, err
		}
		enc, err := json.Marshal(redactedTree)
		if err != nil {
			return nil, err
		}
		block["input"] = enc
	case "tool_result":
		content, ok := block["content"]
		if !ok {
			break
		}
		redacted, err := s.redactTextOrTextBlocks(content)
		if err != nil {
			return nil, err
		}
		block["content"] = redacted
	default:
		// thinking, redacted_thinking, image, server_tool_use and any
		// future block type this version does not recognize — passed
		// through byte-identical (specs/0006, Non-goals).
		return raw, nil
	}
	return json.Marshal(block)
}

// redactTree walks v — the result of decoding a tool_use block's "input"
// with json.Number so integers round-trip — and redacts every string
// leaf. Map keys and non-string leaves (numbers, bools, null) are never
// touched.
func (s *scan) redactTree(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return s.redactText(x), nil
	case map[string]any:
		for k, val := range x {
			redacted, err := s.redactTree(val)
			if err != nil {
				return nil, err
			}
			x[k] = redacted
		}
		return x, nil
	case []any:
		for i, val := range x {
			redacted, err := s.redactTree(val)
			if err != nil {
				return nil, err
			}
			x[i] = redacted
		}
		return x, nil
	default:
		return v, nil
	}
}

// decodeJSONTree decodes raw keeping numbers as json.Number, so large
// integers survive a redactTree round trip.
func decodeJSONTree(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// rawString decodes raw as a JSON string, returning "" for anything else
// (absent, non-string, malformed) — used only to read a block's "type",
// where an unrecognized shape should fall through to the default,
// byte-identical case rather than fail the whole call.
func rawString(raw json.RawMessage) string {
	s, _ := decodeString(raw)
	return s
}

func decodeString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// rejectDuplicateKeys fails on the first repeated key in any object, at
// any depth. Duplicated from internal/policy's version rather than
// shared: the two packages read the same wire format for unrelated
// reasons and should stay free to diverge (specs/0006, Alternatives
// considered).
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
			return errNotTargetShape
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

// operationName duplicates internal/policy's own version, for the same
// reason rejectDuplicateKeys does.
func operationName(path string) string {
	switch path {
	case "/v1/messages":
		return "messages"
	case "/v1/messages/count_tokens":
		return "count_tokens"
	}
	return "other"
}

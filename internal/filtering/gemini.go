package filtering

import (
	"encoding/json"
)

// field returns the member named camel or snake, the key it was found
// under, and an error when both spellings are present: the Gateway and
// Gemini could otherwise read different values.
func field(obj map[string]json.RawMessage, camel, snake string) (raw json.RawMessage, key string, err error) {
	c, hasCamel := obj[camel]
	sn, hasSnake := obj[snake]
	switch {
	case hasCamel && hasSnake:
		return nil, "", errNotTargetShape
	case hasCamel:
		return c, camel, nil
	case hasSnake:
		return sn, snake, nil
	}
	return nil, "", nil
}

// redactGenerate rewrites a generateContent or countTokens body: the
// system instruction, the contents, and a wrapped generateContentRequest.
func (s *scan) redactGenerate(top map[string]json.RawMessage) error {
	raw, key, err := field(top, "systemInstruction", "system_instruction")
	if err != nil {
		return err
	}
	if key != "" {
		if top[key], err = s.redactContent(raw); err != nil {
			return err
		}
	}

	if raw, ok := top["contents"]; ok && !isJSONNull(raw) {
		var contents []json.RawMessage
		if err := json.Unmarshal(raw, &contents); err != nil {
			return errNotTargetShape
		}
		for i, c := range contents {
			if contents[i], err = s.redactContent(c); err != nil {
				return err
			}
		}
		if top["contents"], err = json.Marshal(contents); err != nil {
			return err
		}
	}

	raw, key, err = field(top, "generateContentRequest", "generate_content_request")
	if err != nil {
		return err
	}
	if key != "" && !isJSONNull(raw) {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err != nil || inner == nil {
			return errNotTargetShape
		}
		if err := s.redactGenerate(inner); err != nil {
			return err
		}
		if top[key], err = json.Marshal(inner); err != nil {
			return err
		}
	}
	return nil
}

// redactEmbed rewrites an embedContent body's content, or each of a
// batchEmbedContents body's requests[].content.
func (s *scan) redactEmbed(top map[string]json.RawMessage) error {
	var err error
	if raw, ok := top["content"]; ok {
		if top["content"], err = s.redactContent(raw); err != nil {
			return err
		}
	}
	raw, ok := top["requests"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var requests []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &requests); err != nil {
		return errNotTargetShape
	}
	for _, req := range requests {
		if req == nil {
			return errNotTargetShape
		}
		if c, ok := req["content"]; ok {
			if req["content"], err = s.redactContent(c); err != nil {
				return err
			}
		}
	}
	top["requests"], err = json.Marshal(requests)
	return err
}

// redactContent redacts a Gemini Content object's parts: text, function
// call arguments and function responses. Every other part and field is
// copied as raw bytes.
func (s *scan) redactContent(raw json.RawMessage) (json.RawMessage, error) {
	if isJSONNull(raw) {
		return raw, nil
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil {
		return nil, errNotTargetShape
	}
	rawParts, ok := content["parts"]
	if !ok || isJSONNull(rawParts) {
		return raw, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawParts, &parts); err != nil {
		return nil, errNotTargetShape
	}
	for i, p := range parts {
		redacted, err := s.redactPart(p)
		if err != nil {
			return nil, err
		}
		parts[i] = redacted
	}
	var err error
	if content["parts"], err = json.Marshal(parts); err != nil {
		return nil, err
	}
	return json.Marshal(content)
}

func (s *scan) redactPart(raw json.RawMessage) (json.RawMessage, error) {
	var part map[string]json.RawMessage
	if err := json.Unmarshal(raw, &part); err != nil || part == nil {
		return nil, errNotTargetShape
	}
	call, callKey, err := field(part, "functionCall", "function_call")
	if err != nil {
		return nil, err
	}
	resp, respKey, err := field(part, "functionResponse", "function_response")
	if err != nil {
		return nil, err
	}
	if string(part["thought"]) == "true" {
		return raw, nil
	}

	switch {
	case part["text"] != nil:
		text, ok := decodeString(part["text"])
		if !ok {
			return nil, errNotTargetShape
		}
		if part["text"], err = json.Marshal(s.redactText(text)); err != nil {
			return nil, err
		}
	case callKey != "":
		if part[callKey], err = s.redactMember(call, "args"); err != nil {
			return nil, err
		}
	case respKey != "":
		if part[respKey], err = s.redactMember(resp, "response"); err != nil {
			return nil, err
		}
	default:
		return raw, nil
	}
	return json.Marshal(part)
}

// redactMember walks member key of the object raw with redactTree.
func (s *scan) redactMember(raw json.RawMessage, key string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errNotTargetShape
	}
	value, ok := obj[key]
	if !ok || isJSONNull(value) {
		return raw, nil
	}
	tree, err := decodeJSONTree(value)
	if err != nil {
		return nil, errNotTargetShape
	}
	redacted, err := s.redactTree(tree)
	if err != nil {
		return nil, err
	}
	if obj[key], err = json.Marshal(redacted); err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

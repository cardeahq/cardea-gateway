package policy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/cardeahq/cardea-gateway/internal/routing"
)

var (
	geminiGenerateExcluded = map[string]bool{
		"contents": true, "systemInstruction": true, "system_instruction": true,
		"tools": true, "generateContentRequest": true, "generate_content_request": true,
	}
	geminiEmbeddingsExcluded = map[string]bool{"content": true, "requests": true}
)

// snakeCase turns a camelCase field name into its snake_case spelling.
func snakeCase(camel string) string {
	var b strings.Builder
	for _, r := range camel {
		if unicode.IsUpper(r) {
			b.WriteByte('_')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// member returns obj's field in either spelling Gemini accepts. Both
// spellings at once is an error: the Gateway and Gemini could read
// different values.
func member(obj map[string]json.RawMessage, camel string) (json.RawMessage, bool, error) {
	c, hasCamel := obj[camel]
	snake := snakeCase(camel)
	if snake == camel {
		return c, hasCamel, nil
	}
	s, hasSnake := obj[snake]
	switch {
	case hasCamel && hasSnake:
		return nil, false, errBadBody
	case hasSnake:
		return s, true, nil
	}
	return c, hasCamel, nil
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errBadBody
	}
	return obj, nil
}

func decodeArray(raw json.RawMessage) ([]json.RawMessage, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errBadBody
	}
	return list, nil
}

// addGeminiModelAndStream reads model and stream from the URL: a Gemini
// body carries neither.
func addGeminiModelAndStream(input map[string]any, path string) {
	r := &http.Request{URL: &url.URL{Path: path}}
	if model, found, _ := routing.ExtractModel(routing.FormatGemini, r, nil); found {
		input["model"] = model
	}
	input["stream"] = routing.GeminiStreams(path)
}

func addGeminiGenerate(input map[string]any, r Request) error {
	top, err := decodeTop(r.Body)
	if err != nil {
		return err
	}
	addGeminiModelAndStream(input, r.Path)
	if input["parameters"], err = parametersOf(top, geminiGenerateExcluded); err != nil {
		return err
	}
	return addGeminiConversation(input, top)
}

// addGeminiCountTokens reads the conversation from generateContentRequest
// when present. Gemini accepts contents or generateContentRequest, not
// both; both is an error so neither can go unread.
func addGeminiCountTokens(input map[string]any, r Request) error {
	top, err := decodeTop(r.Body)
	if err != nil {
		return err
	}
	addGeminiModelAndStream(input, r.Path)
	if input["parameters"], err = parametersOf(top, geminiGenerateExcluded); err != nil {
		return err
	}
	raw, ok, err := member(top, "generateContentRequest")
	if err != nil {
		return err
	}
	if !ok {
		return addGeminiConversation(input, top)
	}
	if _, hasContents, err := member(top, "contents"); err != nil || hasContents {
		return errBadBody
	}
	inner, err := decodeObject(raw)
	if err != nil {
		return err
	}
	return addGeminiConversation(input, inner)
}

func addGeminiConversation(input map[string]any, src map[string]json.RawMessage) error {
	if raw, ok, err := member(src, "systemInstruction"); err != nil {
		return err
	} else if ok {
		texts, err := geminiTexts(raw)
		if err != nil {
			return err
		}
		input["system"] = strings.Join(texts, "\n")
	}

	var messages []any
	var calls, results []map[string]any
	if raw, ok, err := member(src, "contents"); err != nil {
		return err
	} else if ok {
		contents, err := decodeArray(raw)
		if err != nil {
			return err
		}
		for _, c := range contents {
			msg, cs, rs, err := normalizeGeminiContent(c)
			if err != nil {
				return err
			}
			messages = append(messages, msg)
			calls = append(calls, cs...)
			results = append(results, rs...)
		}
	}

	tools, err := normalizeGeminiTools(src)
	if err != nil {
		return err
	}
	setConversation(input, messages, calls, results, tools)
	return nil
}

// geminiTexts returns the text parts of a Content object.
func geminiTexts(raw json.RawMessage) ([]string, error) {
	content, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	rawParts, ok, err := member(content, "parts")
	if err != nil || !ok {
		return nil, err
	}
	parts, err := decodeArray(rawParts)
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, p := range parts {
		part, err := decodeObject(p)
		if err != nil {
			return nil, err
		}
		if rawText, ok := part["text"]; ok {
			var s string
			if err := json.Unmarshal(rawText, &s); err != nil {
				return nil, errBadBody
			}
			texts = append(texts, s)
		}
	}
	return texts, nil
}

// normalizeGeminiContent maps one contents entry to a message: role model
// becomes assistant, and an entry without a role is the user's, as Gemini
// reads it.
func normalizeGeminiContent(raw json.RawMessage) (msg map[string]any, calls, results []map[string]any, err error) {
	entry, err := decodeObject(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	role := "user"
	if rawRole, ok := entry["role"]; ok {
		if err := json.Unmarshal(rawRole, &role); err != nil {
			return nil, nil, nil, errBadBody
		}
	}
	switch role {
	case "model":
		role = "assistant"
	case "":
		role = "user"
	}

	content := []any{}
	if rawParts, ok, err := member(entry, "parts"); err != nil {
		return nil, nil, nil, err
	} else if ok {
		parts, err := decodeArray(rawParts)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, p := range parts {
			part, call, result, err := normalizeGeminiPart(p)
			if err != nil {
				return nil, nil, nil, err
			}
			content = append(content, part)
			if call != nil {
				calls = append(calls, call)
			}
			if result != nil {
				results = append(results, result)
			}
		}
	}
	return map[string]any{"role": role, "content": content}, calls, results, nil
}

func normalizeGeminiPart(raw json.RawMessage) (part, call, result map[string]any, err error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	fc, hasCall, err := member(obj, "functionCall")
	if err != nil {
		return nil, nil, nil, err
	}
	fr, hasResult, err := member(obj, "functionResponse")
	if err != nil {
		return nil, nil, nil, err
	}
	_, hasInline, err := member(obj, "inlineData")
	if err != nil {
		return nil, nil, nil, err
	}
	_, hasFile, err := member(obj, "fileData")
	if err != nil {
		return nil, nil, nil, err
	}

	switch {
	case obj["text"] != nil:
		var text string
		if err := json.Unmarshal(obj["text"], &text); err != nil {
			return nil, nil, nil, errBadBody
		}
		return map[string]any{"type": "text", "text": text}, nil, nil, nil
	case hasCall:
		f, err := decodeObject(fc)
		if err != nil {
			return nil, nil, nil, err
		}
		id, name, err := idAndName(f)
		if err != nil {
			return nil, nil, nil, err
		}
		args := any(map[string]any{})
		if rawArgs, ok := f["args"]; ok {
			if args, err = decodeValue(rawArgs); err != nil {
				return nil, nil, nil, errBadBody
			}
		}
		call = map[string]any{"name": name, "arguments": args}
		part = map[string]any{"type": "tool_call", "name": name, "arguments": args}
		if id != "" {
			call["id"], part["id"] = id, id
		}
		return part, call, nil, nil
	case hasResult:
		f, err := decodeObject(fr)
		if err != nil {
			return nil, nil, nil, err
		}
		id, name, err := idAndName(f)
		if err != nil {
			return nil, nil, nil, err
		}
		text := ""
		if rawResp, ok := f["response"]; ok {
			v, err := decodeValue(rawResp)
			if err != nil {
				return nil, nil, nil, errBadBody
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil, nil, nil, errBadBody
			}
			text = string(b)
		}
		part = map[string]any{"type": "tool_result", "is_error": false, "size_bytes": len(text), "text": text}
		result = map[string]any{"is_error": false, "size_bytes": len(text), "text": text}
		if id != "" {
			part["tool_call_id"], result["tool_call_id"] = id, id
		}
		if name != "" {
			result["tool_name"] = name
		}
		return part, nil, result, nil
	case hasInline:
		return map[string]any{"type": "image"}, nil, nil, nil
	case hasFile:
		return map[string]any{"type": "file"}, nil, nil, nil
	}
	return map[string]any{"type": "other"}, nil, nil, nil
}

func idAndName(f map[string]json.RawMessage) (id, name string, err error) {
	for key, dst := range map[string]*string{"id": &id, "name": &name} {
		if raw, ok := f[key]; ok {
			if json.Unmarshal(raw, dst) != nil {
				return "", "", errBadBody
			}
		}
	}
	return id, name, nil
}

func normalizeGeminiTools(src map[string]json.RawMessage) ([]any, error) {
	tools := []any{}
	raw, ok, err := member(src, "tools")
	if err != nil || !ok {
		return tools, err
	}
	list, err := decodeArray(raw)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		tool, err := decodeObject(t)
		if err != nil {
			return nil, err
		}
		rawDecls, ok, err := member(tool, "functionDeclarations")
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var decls []struct {
			Name        string          `json:"name"`
			Description *string         `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(rawDecls, &decls); err != nil {
			return nil, errBadBody
		}
		for _, d := range decls {
			out := map[string]any{"name": d.Name}
			if d.Description != nil {
				out["description"] = *d.Description
			}
			if len(d.Parameters) > 0 {
				schema, err := decodeValue(d.Parameters)
				if err != nil {
					return nil, errBadBody
				}
				out["parameters"] = schema
			}
			tools = append(tools, out)
		}
	}
	return tools, nil
}

// addGeminiEmbeddings exposes the text parts of content, or of each
// requests[].content, as one user message.
func addGeminiEmbeddings(input map[string]any, r Request) error {
	top, err := decodeTop(r.Body)
	if err != nil {
		return err
	}
	addGeminiModelAndStream(input, r.Path)
	if input["parameters"], err = parametersOf(top, geminiEmbeddingsExcluded); err != nil {
		return err
	}
	var texts []string
	if raw, ok := top["content"]; ok {
		if texts, err = geminiTexts(raw); err != nil {
			return err
		}
	}
	if raw, ok := top["requests"]; ok {
		requests, err := decodeArray(raw)
		if err != nil {
			return err
		}
		for _, req := range requests {
			obj, err := decodeObject(req)
			if err != nil {
				return err
			}
			if c, ok := obj["content"]; ok {
				more, err := geminiTexts(c)
				if err != nil {
					return err
				}
				texts = append(texts, more...)
			}
		}
	}
	setConversation(input, userTextMessage(texts), nil, nil, nil)
	return nil
}

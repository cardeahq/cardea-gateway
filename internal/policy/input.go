package policy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// credentialHeaders never reach the input document. x-gateway-request-id
// is the pipeline's own, stripped from inbound requests.
var credentialHeaders = map[string]bool{
	"authorization":        true,
	"proxy-authorization":  true,
	"x-api-key":            true,
	"x-goog-api-key":       true,
	"api-key":              true,
	"cookie":               true,
	"set-cookie":           true,
	"x-gateway-request-id": true,
}

var errNoReader = errors.New("no input reader for this format and operation")

// reader adds the conversation groups of one (format, operation) to input.
type reader func(input map[string]any, r Request) error

// readers lists every (format, operation) pair; a nil reader carries no
// conversation and its body is not parsed.
var readers = map[string]map[string]reader{
	"anthropic": {"messages": addAnthropicConversation, "count_tokens": addAnthropicConversation, "models": nil},
	"openai":    {"messages": addOpenAIChat, "completion": addOpenAICompletion, "embeddings": addOpenAIEmbeddings, "models": nil},
	"gemini":    {"messages": addGeminiGenerate, "count_tokens": addGeminiCountTokens, "embeddings": addGeminiEmbeddings, "models": nil},
}

// buildInput returns the document rules are written against. A key that
// does not apply or is unknown is omitted, never null. A pair with no
// reader returns errNoReader.
func buildInput(r Request) (map[string]any, error) {
	read, ok := readers[r.Format][r.Operation]
	if !ok {
		return nil, errNoReader
	}
	input := map[string]any{
		"call": map[string]any{
			"id":         r.CallID,
			"size_bytes": len(r.Body),
			"headers":    inputHeaders(r.Headers),
		},
		"provider":  r.Provider,
		"operation": map[string]any{"name": r.Operation, "method": r.Method, "path": r.Path},
	}
	if r.AgentID != "" {
		input["agent"] = map[string]any{"id": r.AgentID}
	}
	if r.SessionID != "" {
		input["session"] = map[string]any{"id": r.SessionID}
	}
	if read == nil {
		return input, nil
	}
	if err := rejectDuplicateKeys(r.Body); err != nil {
		return nil, err
	}
	if err := read(input, r); err != nil {
		return nil, err
	}
	return input, nil
}

// decodeTop decodes a body that must be a JSON object.
func decodeTop(body []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, errBadBody
	}
	return top, nil
}

// parametersOf decodes every top-level member not in excluded.
func parametersOf(top map[string]json.RawMessage, excluded map[string]bool) (map[string]any, error) {
	parameters := map[string]any{}
	for k, raw := range top {
		if excluded[k] {
			continue
		}
		v, err := decodeValue(raw)
		if err != nil {
			return nil, errBadBody
		}
		parameters[k] = v
	}
	return parameters, nil
}

// addModelAndStream sets model and stream from the body's own members.
func addModelAndStream(input map[string]any, top map[string]json.RawMessage) error {
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
	return nil
}

// setConversation sets the message and tool groups. A result without a
// tool_name takes the name of the call it answers, when one is known.
func setConversation(input map[string]any, messages []any, calls []map[string]any, results []map[string]any, tools []any) {
	callNames := map[string]string{}
	toolCalls := make([]any, 0, len(calls))
	for _, c := range calls {
		toolCalls = append(toolCalls, c)
		if id, _ := c["id"].(string); id != "" {
			callNames[id], _ = c["name"].(string)
		}
	}
	toolResults := make([]any, 0, len(results))
	for _, r := range results {
		if _, named := r["tool_name"]; !named {
			id, _ := r["tool_call_id"].(string)
			if name, ok := callNames[id]; ok {
				r["tool_name"] = name
			}
		}
		toolResults = append(toolResults, r)
	}
	if messages == nil {
		messages = []any{}
	}
	if tools == nil {
		tools = []any{}
	}
	input["messages"] = messages
	input["tool_calls"] = toolCalls
	input["tool_results"] = toolResults
	input["tools"] = tools
}

// userTextMessage is the one user message completion and embeddings
// requests expose their texts as.
func userTextMessage(texts []string) []any {
	content := make([]any, 0, len(texts))
	for _, t := range texts {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	return []any{map[string]any{"role": "user", "content": content}}
}

func inputHeaders(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for name, values := range h {
		name = strings.ToLower(name)
		if credentialHeaders[name] {
			continue
		}
		list, _ := out[name].([]any)
		for _, v := range values {
			list = append(list, v)
		}
		out[name] = list
	}
	return out
}

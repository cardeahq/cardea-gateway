package policy

import (
	"net/http"
	"strings"
)

// credentialHeaders never reach the input document. x-gateway-request-id
// is the pipeline's own, stripped from inbound requests.
var credentialHeaders = map[string]bool{
	"authorization":        true,
	"proxy-authorization":  true,
	"x-api-key":            true,
	"api-key":              true,
	"cookie":               true,
	"set-cookie":           true,
	"x-gateway-request-id": true,
}

// buildInput returns the document rules are written against. A key that
// does not apply or is unknown is omitted, never null.
func buildInput(provider string, r Request) (map[string]any, error) {
	op := operationName(r.Path)
	input := map[string]any{
		"call": map[string]any{
			"id":         r.CallID,
			"size_bytes": len(r.Body),
			"headers":    inputHeaders(r.Headers),
		},
		"provider":  provider,
		"operation": map[string]any{"name": op, "method": r.Method, "path": r.Path},
	}
	if r.AgentID != "" {
		input["agent"] = map[string]any{"id": r.AgentID}
	}
	if r.SessionID != "" {
		input["session"] = map[string]any{"id": r.SessionID}
	}
	if provider == "anthropic" && op != "other" {
		if err := addAnthropicConversation(input, r.Body); err != nil {
			return nil, err
		}
	}
	return input, nil
}

func operationName(path string) string {
	switch path {
	case "/v1/messages":
		return "messages"
	case "/v1/messages/count_tokens":
		return "count_tokens"
	}
	return "other"
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

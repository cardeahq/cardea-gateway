// Package routing chooses, per call, which configured provider serves it:
// detecting the request's API format, extracting the model it names, and
// resolving both against the operator's routing table. It imports nothing
// from internal/, so specs/0011 (Vertex) can reuse it unchanged.
package routing

import (
	"net/http"
	"strings"
)

// Format is the API shape the Gateway recognized in a request.
type Format string

const (
	FormatUnknown   Format = ""
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
	FormatGemini    Format = "gemini"
)

var geminiActions = map[string]bool{
	"generateContent":       true,
	"streamGenerateContent": true,
	"countTokens":           true,
	"embedContent":          true,
	"batchEmbedContents":    true,
}

// DetectFormat applies the rules in specs/0009-technical, Design -> Format
// detection, in order: the anthropic-version header, then the Gemini path
// shapes, then the Anthropic and OpenAI-style URLs, else unknown.
func DetectFormat(r *http.Request) Format {
	if r.Header.Get("anthropic-version") != "" {
		return FormatAnthropic
	}
	if isGeminiPath(r.URL.Path) {
		return FormatGemini
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/messages/count_tokens":
		return FormatAnthropic
	case "/v1/chat/completions", "/v1/fim/completions", "/v1/embeddings", "/v1/models":
		return FormatOpenAI
	}
	if strings.HasPrefix(r.URL.Path, "/v1/models/") {
		return FormatOpenAI
	}
	return FormatUnknown
}

// FormatOf returns the format a provider kind speaks; FormatUnknown for an
// unknown kind.
func FormatOf(kind string) Format {
	switch kind {
	case "anthropic":
		return FormatAnthropic
	case "mistral":
		return FormatOpenAI
	case "gemini":
		return FormatGemini
	default:
		return FormatUnknown
	}
}

// GeminiStreams reports whether a Gemini path asks for a streamed response.
func GeminiStreams(path string) bool {
	_, action, ok := geminiModelAndAction(path)
	return ok && action == "streamGenerateContent"
}

func isGeminiPath(path string) bool {
	if strings.HasPrefix(path, "/v1beta/") {
		return true
	}
	_, _, ok := geminiModelAndAction(path)
	return ok
}

// geminiModelAndAction matches /models/{model}:{action} at the end of
// path, where {model} has no "/" or ":" and {action} is one of the known
// Gemini actions. It matches regardless of what precedes "/models/", so a
// longer prefix (specs/0011's Vertex path) still matches.
func geminiModelAndAction(path string) (model, action string, ok bool) {
	idx := strings.LastIndex(path, "/models/")
	if idx < 0 {
		return "", "", false
	}
	rest := path[idx+len("/models/"):]
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return "", "", false
	}
	model, action = rest[:i], rest[i+1:]
	if model == "" || strings.ContainsAny(model, "/:") {
		return "", "", false
	}
	if !geminiActions[action] {
		return "", "", false
	}
	return model, action, true
}

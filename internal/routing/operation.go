package routing

import (
	"net/http"
	"strings"
)

// Operation is the format-neutral name of a supported operation.
type Operation string

const (
	OpUnsupported Operation = ""
	OpMessages    Operation = "messages"
	OpCountTokens Operation = "count_tokens"
	OpCompletion  Operation = "completion"
	OpEmbeddings  Operation = "embeddings"
	OpModels      Operation = "models"
)

// HasContent reports whether op carries content (every operation but models).
func (op Operation) HasContent() bool {
	return op != OpModels && op != OpUnsupported
}

// DetectOperation applies specs/0009-technical, Design -> Operation
// detection. Method and path must both match a supported row.
func DetectOperation(f Format, r *http.Request) Operation {
	path := r.URL.Path
	switch f {
	case FormatAnthropic, FormatOpenAI:
		if r.Method == http.MethodGet && isModelsPath(path, "/v1/models", "/") {
			return OpModels
		}
		if r.Method != http.MethodPost {
			return OpUnsupported
		}
		switch {
		case f == FormatAnthropic && path == "/v1/messages":
			return OpMessages
		case f == FormatAnthropic && path == "/v1/messages/count_tokens":
			return OpCountTokens
		case f == FormatOpenAI && path == "/v1/chat/completions":
			return OpMessages
		case f == FormatOpenAI && path == "/v1/fim/completions":
			return OpCompletion
		case f == FormatOpenAI && path == "/v1/embeddings":
			return OpEmbeddings
		}
	case FormatGemini:
		if r.Method == http.MethodGet && isModelsPath(path, "/v1beta/models", "/:") {
			return OpModels
		}
		if r.Method != http.MethodPost {
			return OpUnsupported
		}
		_, action, ok := geminiModelAndAction(path)
		if !ok {
			return OpUnsupported
		}
		switch action {
		case "generateContent", "streamGenerateContent":
			return OpMessages
		case "countTokens":
			return OpCountTokens
		case "embedContent", "batchEmbedContents":
			return OpEmbeddings
		}
	}
	return OpUnsupported
}

// isModelsPath matches base exactly, or base followed by one non-empty
// segment containing none of forbidden.
func isModelsPath(path, base, forbidden string) bool {
	if path == base {
		return true
	}
	id, ok := strings.CutPrefix(path, base+"/")
	return ok && id != "" && !strings.ContainsAny(id, forbidden)
}

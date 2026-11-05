package routing

import (
	"net/http"
	"testing"
)

// specs/0009-technical, Design -> Operation detection.
func TestDetectOperation(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		r      *http.Request
		want   Operation
	}{
		// --- anthropic ---------------------------------------------------------
		{"anthropic messages", FormatAnthropic, req(http.MethodPost, "/v1/messages"), OpMessages},
		{"anthropic count_tokens", FormatAnthropic, req(http.MethodPost, "/v1/messages/count_tokens"), OpCountTokens},
		{"anthropic model listing", FormatAnthropic, req(http.MethodGet, "/v1/models"), OpModels},
		{"anthropic one model", FormatAnthropic, req(http.MethodGet, "/v1/models/claude-sonnet-5"), OpModels},
		{"anthropic message batches are unsupported", FormatAnthropic, req(http.MethodPost, "/v1/messages/batches"), OpUnsupported},
		{"anthropic one batch is unsupported", FormatAnthropic, req(http.MethodGet, "/v1/messages/batches/msgbatch_01"), OpUnsupported},
		{"anthropic files are unsupported", FormatAnthropic, req(http.MethodPost, "/v1/files"), OpUnsupported},
		{"anthropic GET on messages is unsupported", FormatAnthropic, req(http.MethodGet, "/v1/messages"), OpUnsupported},
		{"anthropic POST on models is unsupported", FormatAnthropic, req(http.MethodPost, "/v1/models"), OpUnsupported},
		{"anthropic chat completions path is unsupported", FormatAnthropic, req(http.MethodPost, "/v1/chat/completions"), OpUnsupported},

		// --- openai --------------------------------------------------------------
		{"openai chat completions", FormatOpenAI, req(http.MethodPost, "/v1/chat/completions"), OpMessages},
		{"openai fim completions", FormatOpenAI, req(http.MethodPost, "/v1/fim/completions"), OpCompletion},
		{"openai embeddings", FormatOpenAI, req(http.MethodPost, "/v1/embeddings"), OpEmbeddings},
		{"openai model listing", FormatOpenAI, req(http.MethodGet, "/v1/models"), OpModels},
		{"openai one model", FormatOpenAI, req(http.MethodGet, "/v1/models/mistral-large-latest"), OpModels},
		{"openai DELETE of a model is unsupported", FormatOpenAI, req(http.MethodDelete, "/v1/models/ft:mistral:abc"), OpUnsupported},
		{"openai GET on chat completions is unsupported", FormatOpenAI, req(http.MethodGet, "/v1/chat/completions"), OpUnsupported},
		{"openai models with an empty id is unsupported", FormatOpenAI, req(http.MethodGet, "/v1/models/"), OpUnsupported},
		{"openai models id with a slash is unsupported", FormatOpenAI, req(http.MethodGet, "/v1/models/a/b"), OpUnsupported},
		{"mistral agents completions are unsupported", FormatOpenAI, req(http.MethodPost, "/v1/agents/completions"), OpUnsupported},
		{"mistral ocr is unsupported", FormatOpenAI, req(http.MethodPost, "/v1/ocr"), OpUnsupported},
		{"mistral moderations are unsupported", FormatOpenAI, req(http.MethodPost, "/v1/moderations"), OpUnsupported},
		{"mistral files are unsupported", FormatOpenAI, req(http.MethodPost, "/v1/files"), OpUnsupported},
		{"mistral batch jobs are unsupported", FormatOpenAI, req(http.MethodPost, "/v1/batch/jobs"), OpUnsupported},
		{"mistral fine-tuning jobs are unsupported", FormatOpenAI, req(http.MethodPost, "/v1/fine_tuning/jobs"), OpUnsupported},

		// --- gemini --------------------------------------------------------------
		{"gemini generateContent", FormatGemini, req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent"), OpMessages},
		{"gemini streamGenerateContent", FormatGemini, req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"), OpMessages},
		{"gemini countTokens", FormatGemini, req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens"), OpCountTokens},
		{"gemini embedContent", FormatGemini, req(http.MethodPost, "/v1beta/models/text-embedding-004:embedContent"), OpEmbeddings},
		{"gemini batchEmbedContents", FormatGemini, req(http.MethodPost, "/v1beta/models/text-embedding-004:batchEmbedContents"), OpEmbeddings},
		{"gemini model listing", FormatGemini, req(http.MethodGet, "/v1beta/models"), OpModels},
		{"gemini one model", FormatGemini, req(http.MethodGet, "/v1beta/models/gemini-2.5-pro"), OpModels},
		{"gemini suffix under a long prefix", FormatGemini, req(http.MethodPost, "/v1beta1/projects/p/locations/l/publishers/google/models/gemini-2.5-pro:generateContent"), OpMessages},
		{"gemini GET on generateContent is unsupported", FormatGemini, req(http.MethodGet, "/v1beta/models/gemini-2.5-pro:generateContent"), OpUnsupported},
		{"gemini POST on the model listing is unsupported", FormatGemini, req(http.MethodPost, "/v1beta/models"), OpUnsupported},
		{"gemini unknown action is unsupported", FormatGemini, req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:notAnAction"), OpUnsupported},
		{"gemini one model with a colon is unsupported", FormatGemini, req(http.MethodGet, "/v1beta/models/gemini:x"), OpUnsupported},
		{"gemini files are unsupported", FormatGemini, req(http.MethodPost, "/v1beta/files"), OpUnsupported},
		{"gemini cached contents are unsupported", FormatGemini, req(http.MethodPost, "/v1beta/cachedContents"), OpUnsupported},
		{"gemini tuned models are unsupported", FormatGemini, req(http.MethodGet, "/v1beta/tunedModels"), OpUnsupported},

		// --- unknown format ------------------------------------------------------
		{"unknown format is unsupported whatever the path", FormatUnknown, req(http.MethodPost, "/v1/messages"), OpUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectOperation(tt.format, tt.r); got != tt.want {
				t.Fatalf("DetectOperation(%s, %s %s) = %q, want %q", tt.format, tt.r.Method, tt.r.URL.Path, got, tt.want)
			}
		})
	}
}

func TestOperation_Values(t *testing.T) {
	// The neutral names are part of the policy input contract (specs/0004
	// rule 11) and the trace record; pin their spelling.
	tests := []struct {
		op   Operation
		want string
	}{
		{OpUnsupported, ""},
		{OpMessages, "messages"},
		{OpCountTokens, "count_tokens"},
		{OpCompletion, "completion"},
		{OpEmbeddings, "embeddings"},
		{OpModels, "models"},
	}
	for _, tt := range tests {
		if string(tt.op) != tt.want {
			t.Fatalf("operation = %q, want %q", tt.op, tt.want)
		}
	}
}

func TestOperation_HasContent(t *testing.T) {
	tests := []struct {
		op   Operation
		want bool
	}{
		{OpMessages, true},
		{OpCountTokens, true},
		{OpCompletion, true},
		{OpEmbeddings, true},
		{OpModels, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.op), func(t *testing.T) {
			if got := tt.op.HasContent(); got != tt.want {
				t.Fatalf("%q.HasContent() = %v, want %v", tt.op, got, tt.want)
			}
		})
	}
}

package routing

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(method, path string, headers ...string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name string
		r    *http.Request
		want Format
	}{
		// --- rule 1: anthropic-version header, checked first -------------
		{"anthropic-version header alone selects anthropic", req(http.MethodPost, "/anything", "anthropic-version", "2023-06-01"), FormatAnthropic},
		{"anthropic-version header overrides an OpenAI-shaped path", req(http.MethodPost, "/v1/chat/completions", "anthropic-version", "2023-06-01"), FormatAnthropic},
		{"anthropic-version header overrides a Gemini-shaped path", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", "anthropic-version", "2023-06-01"), FormatAnthropic},
		{"anthropic-version header overrides an unknown path", req(http.MethodPost, "/no/such/route", "anthropic-version", "2023-06-01"), FormatAnthropic},

		// --- rule 2: Gemini path shapes -----------------------------------
		{"gemini generateContent", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent"), FormatGemini},
		{"gemini streamGenerateContent", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent"), FormatGemini},
		{"gemini countTokens", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens"), FormatGemini},
		{"gemini embedContent", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:embedContent"), FormatGemini},
		{"gemini batchEmbedContents", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:batchEmbedContents"), FormatGemini},
		{"gemini action suffix under a long prefix (specs/0011's Vertex path)", req(http.MethodPost, "/v1beta1/projects/p/locations/l/publishers/google/models/gemini-2.5-pro:generateContent"), FormatGemini},
		{"any /v1beta/ path, even with no model:action suffix", req(http.MethodGet, "/v1beta/models"), FormatGemini},
		{"an unrecognized action suffix is not gemini", req(http.MethodPost, "/v1beta/models/gemini-2.5-pro:notAnAction"), FormatGemini}, // /v1beta/ prefix still matches
		{"a model segment containing a slash does not match the suffix rule", req(http.MethodPost, "/models/foo/bar:generateContent"), FormatUnknown},
		{"a model segment containing a colon does not match the suffix rule", req(http.MethodPost, "/models/a:b:generateContent"), FormatUnknown},

		// --- rule 3: Anthropic URLs ---------------------------------------
		{"messages", req(http.MethodPost, "/v1/messages"), FormatAnthropic},
		{"count_tokens", req(http.MethodPost, "/v1/messages/count_tokens"), FormatAnthropic},
		{"trailing slash is not an exact match: unknown", req(http.MethodPost, "/v1/messages/"), FormatUnknown},

		// --- rule 4: OpenAI-style URLs -------------------------------------
		{"chat completions", req(http.MethodPost, "/v1/chat/completions"), FormatOpenAI},
		{"fim completions", req(http.MethodPost, "/v1/fim/completions"), FormatOpenAI},
		{"embeddings", req(http.MethodPost, "/v1/embeddings"), FormatOpenAI},
		{"model listing", req(http.MethodGet, "/v1/models"), FormatOpenAI},
		{"one model's details", req(http.MethodGet, "/v1/models/gpt-4"), FormatOpenAI},
		{"openai trailing slash on the bare listing is still exact", req(http.MethodGet, "/v1/models/"), FormatOpenAI}, // starts with "/v1/models/"

		// --- rule 5: unknown -------------------------------------------------
		{"root path", req(http.MethodGet, "/"), FormatUnknown},
		{"unrelated path", req(http.MethodPost, "/v2/whatever"), FormatUnknown},
		{"empty path-like string", req(http.MethodGet, "/v1/chat/completionsX"), FormatUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectFormat(tt.r); got != tt.want {
				t.Fatalf("DetectFormat(%s) = %q, want %q", tt.r.URL.Path, got, tt.want)
			}
		})
	}
}

func TestDetectFormat_MethodDoesNotMatter(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if got := DetectFormat(req(m, "/v1/messages")); got != FormatAnthropic {
			t.Fatalf("method %s: DetectFormat = %q, want %q", m, got, FormatAnthropic)
		}
	}
}

func TestFormatOf(t *testing.T) {
	tests := []struct {
		kind string
		want Format
	}{
		{"anthropic", FormatAnthropic},
		{"mistral", FormatOpenAI},
		{"gemini", FormatGemini},
		{"", FormatUnknown}, // there is no passthrough kind any more
		{"unknown-kind", FormatUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			if got := FormatOf(tt.kind); got != tt.want {
				t.Fatalf("FormatOf(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

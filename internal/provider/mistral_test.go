package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewMistralProvider_InjectsBearerKeyAndStripsCallerAuth(t *testing.T) {
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	t.Setenv("TEST_MISTRAL_KEY", "mistral-secret")

	p, err := NewMistralProvider(upstream.URL, "TEST_MISTRAL_KEY")
	if err != nil {
		t.Fatalf("NewMistralProvider: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer caller-supplied")
	req.Header.Set("x-api-key", "caller-supplied-key")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotAuth != "Bearer mistral-secret" {
		t.Fatalf("upstream Authorization = %q, want %q", gotAuth, "Bearer mistral-secret")
	}
	if gotAPIKey != "" {
		t.Fatalf("upstream x-api-key = %q, want empty (caller-supplied value must be stripped)", gotAPIKey)
	}
	if p.Name() != "mistral" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "mistral")
	}
}

func TestNewMistralProvider_RejectsInvalidInput(t *testing.T) {
	t.Setenv("TEST_MISTRAL_KEY_SET", "mistral-secret")

	tests := []struct {
		name      string
		baseURL   string
		apiKeyEnv string
	}{
		{"relative URL", "/no-host", "TEST_MISTRAL_KEY_SET"},
		{"unparseable URL", "://bad", "TEST_MISTRAL_KEY_SET"},
		{"empty api key env name", "https://example.com", ""},
		{"api key env not set", "https://example.com", "TEST_MISTRAL_KEY_UNSET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewMistralProvider(tt.baseURL, tt.apiKeyEnv); err == nil {
				t.Fatalf("NewMistralProvider(%q, %q) = nil error, want error", tt.baseURL, tt.apiKeyEnv)
			}
		})
	}
}

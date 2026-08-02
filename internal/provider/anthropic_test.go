package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewAnthropicProvider_InjectsAPIKeyAndStripsCallerAuth(t *testing.T) {
	var gotAPIKey, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	t.Setenv("TEST_ANTHROPIC_KEY", "sk-secret")

	p, err := NewAnthropicProvider(upstream.URL, "TEST_ANTHROPIC_KEY")
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer caller-supplied")
	req.Header.Set("x-api-key", "caller-supplied-key")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotAPIKey != "sk-secret" {
		t.Fatalf("upstream x-api-key = %q, want %q", gotAPIKey, "sk-secret")
	}
	if gotAuth != "" {
		t.Fatalf("upstream Authorization = %q, want empty (caller-supplied value must be stripped)", gotAuth)
	}
	if p.Name() != "anthropic" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "anthropic")
	}
}

func TestNewAnthropicProvider_RejectsInvalidInput(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY_SET", "sk-secret")

	tests := []struct {
		name      string
		baseURL   string
		apiKeyEnv string
	}{
		{"relative URL", "/no-host", "TEST_ANTHROPIC_KEY_SET"},
		{"unparseable URL", "://bad", "TEST_ANTHROPIC_KEY_SET"},
		{"empty api key env name", "https://example.com", ""},
		{"api key env not set", "https://example.com", "TEST_ANTHROPIC_KEY_UNSET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewAnthropicProvider(tt.baseURL, tt.apiKeyEnv); err == nil {
				t.Fatalf("NewAnthropicProvider(%q, %q) = nil error, want error", tt.baseURL, tt.apiKeyEnv)
			}
		})
	}
}

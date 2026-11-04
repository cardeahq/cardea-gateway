package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewGeminiProvider_InjectsAPIKeyHeaderAndStripsCallerCredentials(t *testing.T) {
	var gotGoogKey, gotAuth, gotKeyParam string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGoogKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotKeyParam = r.URL.Query().Get("key")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	t.Setenv("TEST_GEMINI_KEY", "gm-secret")

	p, err := NewGeminiProvider(upstream.URL, "TEST_GEMINI_KEY")
	if err != nil {
		t.Fatalf("NewGeminiProvider: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent?key=caller-supplied-key", nil)
	req.Header.Set("Authorization", "Bearer caller-supplied")
	req.Header.Set("x-goog-api-key", "caller-supplied-goog-key")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotGoogKey != "gm-secret" {
		t.Fatalf("upstream x-goog-api-key = %q, want %q", gotGoogKey, "gm-secret")
	}
	if gotAuth != "" {
		t.Fatalf("upstream Authorization = %q, want empty (caller-supplied value must be stripped)", gotAuth)
	}
	if gotKeyParam != "" {
		t.Fatalf("upstream key query param = %q, want empty (caller-supplied value must be stripped)", gotKeyParam)
	}
	if p.Name() != "gemini" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "gemini")
	}
}

func TestNewGeminiProvider_KeepsTheCallerPathAndOtherQueryParams(t *testing.T) {
	var gotPath, gotOther string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotOther = r.URL.Query().Get("alt")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	t.Setenv("TEST_GEMINI_KEY_2", "gm-secret")
	p, err := NewGeminiProvider(upstream.URL, "TEST_GEMINI_KEY_2")
	if err != nil {
		t.Fatalf("NewGeminiProvider: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=caller-key", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("upstream path = %q, want the caller's path unchanged", gotPath)
	}
	if gotOther != "sse" {
		t.Fatalf("upstream alt query param = %q, want %q (preserved)", gotOther, "sse")
	}
}

func TestNewGeminiProvider_RejectsInvalidInput(t *testing.T) {
	t.Setenv("TEST_GEMINI_KEY_SET", "gm-secret")

	tests := []struct {
		name      string
		baseURL   string
		apiKeyEnv string
	}{
		{"relative URL", "/no-host", "TEST_GEMINI_KEY_SET"},
		{"unparseable URL", "://bad", "TEST_GEMINI_KEY_SET"},
		{"empty api key env name", "https://generativelanguage.googleapis.com", ""},
		{"api key env not set", "https://generativelanguage.googleapis.com", "TEST_GEMINI_KEY_UNSET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewGeminiProvider(tt.baseURL, tt.apiKeyEnv); err == nil {
				t.Fatalf("NewGeminiProvider(%q, %q) = nil error, want error", tt.baseURL, tt.apiKeyEnv)
			}
		})
	}
}

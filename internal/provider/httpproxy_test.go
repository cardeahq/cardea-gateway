package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHTTPProxy_ForwardsRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream got path %q, want /chat/completions", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-response"))
	}))
	defer upstream.Close()

	p, err := NewHTTPProxy("test", upstream.URL)
	if err != nil {
		t.Fatalf("NewHTTPProxy: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "upstream-response" {
		t.Fatalf("body = %q, want %q", body, "upstream-response")
	}
	if p.Name() != "test" {
		t.Fatalf("Name() = %q, want %q", p.Name(), "test")
	}
}

func TestNewHTTPProxy_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		pName   string
		baseURL string
	}{
		{"empty name", "", "https://example.com"},
		{"relative URL", "p", "/no-host"},
		{"unparseable URL", "p", "://bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewHTTPProxy(tt.pName, tt.baseURL); err == nil {
				t.Fatalf("NewHTTPProxy(%q, %q) = nil error, want error", tt.pName, tt.baseURL)
			}
		})
	}
}

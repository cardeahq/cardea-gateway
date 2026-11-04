package provider

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// upstreamFailedWriter records whether UpstreamFailed was called, and
// whether that happened before the status was written.
type upstreamFailedWriter struct {
	*httptest.ResponseRecorder
	headerWritten bool
	failed        bool
	failedBefore  bool
}

func (w *upstreamFailedWriter) WriteHeader(status int) {
	w.headerWritten = true
	w.ResponseRecorder.WriteHeader(status)
}

func (w *upstreamFailedWriter) Write(b []byte) (int, error) {
	w.headerWritten = true
	return w.ResponseRecorder.Write(b)
}

func (w *upstreamFailedWriter) UpstreamFailed() {
	w.failed = true
	w.failedBefore = !w.headerWritten
}

func TestErrorHandler_Writes502AndSignalsUpstreamFailedFirst(t *testing.T) {
	w := &upstreamFailedWriter{ResponseRecorder: httptest.NewRecorder()}
	errorHandler(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), errors.New("dial tcp: connection refused"))

	if !w.failed {
		t.Fatal("errorHandler did not call UpstreamFailed on a writer that implements it")
	}
	if !w.failedBefore {
		t.Fatal("UpstreamFailed was called after the status was written, want before")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestErrorHandler_Writes502OnAPlainWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	errorHandler(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), errors.New("dial tcp: connection refused"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestErrorHandler_DoesNotLeakTheErrorToTheCaller(t *testing.T) {
	const detail = "internal-upstream-address-10.0.0.7"
	rec := httptest.NewRecorder()
	errorHandler(rec, httptest.NewRequest(http.MethodPost, "/", nil), errors.New("dial tcp "+detail))
	if got := rec.Body.String(); got != "" {
		t.Fatalf("body = %q, want a bare 502", got)
	}
}

// closingUpstream accepts connections and closes them without answering.
func closingUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func unreachableURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func buildProviders(t *testing.T, baseURL string) map[string]Provider {
	t.Helper()
	t.Setenv("TEST_ANTHROPIC_KEY_FOR_ERRORS", "sk-secret")
	anthropic, err := NewAnthropicProvider(baseURL, "TEST_ANTHROPIC_KEY_FOR_ERRORS")
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	t.Setenv("TEST_GEMINI_KEY_FOR_ERRORS", "gm-secret")
	gemini, err := NewGeminiProvider(baseURL, "TEST_GEMINI_KEY_FOR_ERRORS")
	if err != nil {
		t.Fatalf("NewGeminiProvider: %v", err)
	}
	proxy, err := NewHTTPProxy("test", baseURL)
	if err != nil {
		t.Fatalf("NewHTTPProxy: %v", err)
	}
	return map[string]Provider{"anthropic": anthropic, "gemini": gemini, "http proxy": proxy}
}

func TestProviders_SignalUpstreamFailedWhenTheProviderNeverAnswers(t *testing.T) {
	upstreams := map[string]func(*testing.T) string{
		"upstream closes the connection": closingUpstream,
		"upstream unreachable":           unreachableURL,
	}
	for uname, upstream := range upstreams {
		for pname, p := range buildProviders(t, upstream(t)) {
			t.Run(uname+"/"+pname, func(t *testing.T) {
				w := &upstreamFailedWriter{ResponseRecorder: httptest.NewRecorder()}
				p.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

				if !w.failed {
					t.Fatal("the proxy did not call UpstreamFailed")
				}
				if w.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502", w.Code)
				}
			})
		}
	}
}

// --- stripCallerCredentials (specs/0009) ----------------------------------

func TestStripCallerCredentials_RemovesAllFourHeadersAndTheKeyParamKeepingOthers(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages?key=caller-key&other=keep-me", nil)
	r.Header.Set("Authorization", "Bearer caller-token")
	r.Header.Set("Proxy-Authorization", "Basic caller-proxy")
	r.Header.Set("x-api-key", "caller-api-key")
	r.Header.Set("x-goog-api-key", "caller-goog-key")
	r.Header.Set("X-Other-Header", "keep-me-too")

	stripCallerCredentials(r)

	for _, h := range []string{"Authorization", "Proxy-Authorization", "x-api-key", "x-goog-api-key"} {
		if v := r.Header.Get(h); v != "" {
			t.Fatalf("header %s = %q, want stripped", h, v)
		}
	}
	if got := r.Header.Get("X-Other-Header"); got != "keep-me-too" {
		t.Fatalf("unrelated header X-Other-Header = %q, want preserved", got)
	}
	q := r.URL.Query()
	if q.Get("key") != "" {
		t.Fatalf("query param key = %q, want stripped", q.Get("key"))
	}
	if q.Get("other") != "keep-me" {
		t.Fatalf("unrelated query param other = %q, want preserved", q.Get("other"))
	}
}

func TestStripCallerCredentials_HeaderNameIsCaseInsensitive(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("AUTHORIZATION", "Bearer caller-token")
	r.Header.Set("X-API-KEY", "caller-api-key")
	r.Header.Set("X-Goog-Api-Key", "caller-goog-key")

	stripCallerCredentials(r)

	if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" || r.Header.Get("x-goog-api-key") != "" {
		t.Fatalf("headers not fully stripped: %v", r.Header)
	}
}

func TestStripCallerCredentials_NoCredentialsIsANoOp(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages?other=keep-me", nil)
	r.Header.Set("X-Other-Header", "keep-me-too")

	stripCallerCredentials(r)

	if got := r.Header.Get("X-Other-Header"); got != "keep-me-too" {
		t.Fatalf("X-Other-Header = %q, want preserved", got)
	}
	if got := r.URL.Query().Get("other"); got != "keep-me" {
		t.Fatalf("query param other = %q, want preserved", got)
	}
}

// --- AC14: each provider forwards only its own credential -----------------

func TestProviders_EachSetsOnlyItsOwnCredentialAndStripsEveryCallerValue(t *testing.T) {
	type captured struct {
		auth, proxyAuth, apiKey, googKey, keyParam string
	}
	capture := func() (*captured, http.HandlerFunc) {
		c := &captured{}
		return c, func(w http.ResponseWriter, r *http.Request) {
			c.auth = r.Header.Get("Authorization")
			c.proxyAuth = r.Header.Get("Proxy-Authorization")
			c.apiKey = r.Header.Get("x-api-key")
			c.googKey = r.Header.Get("x-goog-api-key")
			c.keyParam = r.URL.Query().Get("key")
			w.WriteHeader(http.StatusOK)
		}
	}

	callerReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages?key=caller-key-param", nil)
		r.Header.Set("Authorization", "Bearer caller-token")
		r.Header.Set("Proxy-Authorization", "Basic caller-proxy")
		r.Header.Set("x-api-key", "caller-api-key")
		r.Header.Set("x-goog-api-key", "caller-goog-key")
		return r
	}

	tests := []struct {
		name  string
		build func(t *testing.T, baseURL string) Provider
		check func(c *captured)
	}{
		{
			name: "anthropic",
			build: func(t *testing.T, baseURL string) Provider {
				t.Setenv("TEST_AC14_ANTHROPIC_KEY", "sk-anthropic-secret")
				p, err := NewAnthropicProvider(baseURL, "TEST_AC14_ANTHROPIC_KEY")
				if err != nil {
					t.Fatalf("NewAnthropicProvider: %v", err)
				}
				return p
			},
			check: func(c *captured) {
				if c.apiKey != "sk-anthropic-secret" {
					t.Fatalf("x-api-key = %q, want the Gateway's key", c.apiKey)
				}
				if c.auth != "" || c.proxyAuth != "" || c.googKey != "" || c.keyParam != "" {
					t.Fatalf("other credentials leaked through: %+v", c)
				}
			},
		},
		{
			name: "mistral",
			build: func(t *testing.T, baseURL string) Provider {
				t.Setenv("TEST_AC14_MISTRAL_KEY", "mistral-secret")
				p, err := NewMistralProvider(baseURL, "TEST_AC14_MISTRAL_KEY")
				if err != nil {
					t.Fatalf("NewMistralProvider: %v", err)
				}
				return p
			},
			check: func(c *captured) {
				if c.auth != "Bearer mistral-secret" {
					t.Fatalf("Authorization = %q, want the Gateway's key", c.auth)
				}
				if c.proxyAuth != "" || c.apiKey != "" || c.googKey != "" || c.keyParam != "" {
					t.Fatalf("other credentials leaked through: %+v", c)
				}
			},
		},
		{
			name: "gemini",
			build: func(t *testing.T, baseURL string) Provider {
				t.Setenv("TEST_AC14_GEMINI_KEY", "gemini-secret")
				p, err := NewGeminiProvider(baseURL, "TEST_AC14_GEMINI_KEY")
				if err != nil {
					t.Fatalf("NewGeminiProvider: %v", err)
				}
				return p
			},
			check: func(c *captured) {
				if c.googKey != "gemini-secret" {
					t.Fatalf("x-goog-api-key = %q, want the Gateway's key", c.googKey)
				}
				if c.auth != "" || c.proxyAuth != "" || c.apiKey != "" || c.keyParam != "" {
					t.Fatalf("other credentials leaked through: %+v", c)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, handler := capture()
			upstream := httptest.NewServer(handler)
			defer upstream.Close()

			p := tt.build(t, upstream.URL)
			rec := httptest.NewRecorder()
			p.Handler().ServeHTTP(rec, callerReq())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			tt.check(c)
		})
	}
}

func TestProviders_DoNotSignalUpstreamFailedWhenTheProviderAnswers502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	for pname, p := range buildProviders(t, upstream.URL) {
		t.Run(pname, func(t *testing.T) {
			w := &upstreamFailedWriter{ResponseRecorder: httptest.NewRecorder()}
			p.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

			if w.failed {
				t.Fatal("UpstreamFailed was called although the provider answered")
			}
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want the provider's 502", w.Code)
			}
		})
	}
}

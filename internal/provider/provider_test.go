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
	proxy, err := NewHTTPProxy("test", baseURL)
	if err != nil {
		t.Fatalf("NewHTTPProxy: %v", err)
	}
	return map[string]Provider{"anthropic": anthropic, "http proxy": proxy}
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

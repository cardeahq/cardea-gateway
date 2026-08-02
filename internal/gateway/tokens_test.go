package gateway

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestCountTokensStage_AccumulatesUsageAcrossRequests(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`))
	})

	tokens := &tokenCounter{}
	handler := countTokensStage(zap.NewNop(), tokens)(upstream)

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if body := rec.Body.String(); body != `{"usage":{"input_tokens":10,"output_tokens":5}}` {
			t.Fatalf("client body = %q, want passthrough of upstream body", body)
		}
	}

	if got := tokens.sent.Load(); got != 20 {
		t.Fatalf("total sent = %d, want 20", got)
	}
	if got := tokens.received.Load(); got != 10 {
		t.Fatalf("total received = %d, want 10", got)
	}
}

func TestCountTokensStage_DecodesGzippedUsage(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		_, _ = gw.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`))
		_ = gw.Close()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	})

	tokens := &tokenCounter{}
	handler := countTokensStage(zap.NewNop(), tokens)(upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := tokens.sent.Load(); got != 10 {
		t.Fatalf("total sent = %d, want 10", got)
	}
	if got := tokens.received.Load(); got != 5 {
		t.Fatalf("total received = %d, want 5", got)
	}
}

func TestCountTokensStage_IgnoresNonUsageBodies(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\"}\n\n"))
	})

	tokens := &tokenCounter{}
	handler := countTokensStage(zap.NewNop(), tokens)(upstream)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if tokens.sent.Load() != 0 || tokens.received.Load() != 0 {
		t.Fatalf("expected no tokens counted for non-JSON body, got sent=%d received=%d",
			tokens.sent.Load(), tokens.received.Load())
	}
}

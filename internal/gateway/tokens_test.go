package gateway

import (
	"context"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/agentkeeper/agentkeeper/internal/pipeline"
)

// tokenStage must satisfy both halves of the pipeline.Stage contract: it
// never denies, but it does need to observe responses to count tokens.
var (
	_ pipeline.Stage            = (*tokenStage)(nil)
	_ pipeline.ResponseObserver = (*tokenStage)(nil)
)

func TestTokenStage_Name(t *testing.T) {
	s := &tokenStage{logger: zaptest.NewLogger(t), counter: &tokenCounter{}}
	if got := s.Name(); got != "token_counting" {
		t.Fatalf("Name() = %q, want %q", got, "token_counting")
	}
}

func TestTokenStage_RequestAlwaysAllows(t *testing.T) {
	s := &tokenStage{logger: zaptest.NewLogger(t), counter: &tokenCounter{}}
	call := &pipeline.Call{Body: []byte(`{"prompt":"hi"}`)}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if decision != pipeline.Allow() {
		t.Fatalf("Request() decision = %+v, want pipeline.Allow()", decision)
	}
}

func TestTokenStage_ResponseAccumulatesUsageAcrossCalls(t *testing.T) {
	counter := &tokenCounter{}
	s := &tokenStage{logger: zaptest.NewLogger(t), counter: counter}

	for range 2 {
		call := &pipeline.Call{ResponseBody: []byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`)}
		s.Response(context.Background(), call)
	}

	if got := counter.sent.Load(); got != 20 {
		t.Fatalf("total sent = %d, want 20", got)
	}
	if got := counter.received.Load(); got != 10 {
		t.Fatalf("total received = %d, want 10", got)
	}
}

func TestTokenStage_ResponseIgnoresNonUsageBodies(t *testing.T) {
	counter := &tokenCounter{}
	s := &tokenStage{logger: zaptest.NewLogger(t), counter: counter}

	call := &pipeline.Call{ResponseBody: []byte("data: {\"type\":\"content_block_delta\"}\n\n")}
	s.Response(context.Background(), call)

	if counter.sent.Load() != 0 || counter.received.Load() != 0 {
		t.Fatalf("expected no tokens counted for non-JSON body, got sent=%d received=%d",
			counter.sent.Load(), counter.received.Load())
	}
}

func TestTokenStage_ResponseToleratesDeniedCallWithNoResponseBody(t *testing.T) {
	counter := &tokenCounter{}
	s := &tokenStage{logger: zaptest.NewLogger(t), counter: counter}

	call := &pipeline.Call{
		Denial:       &pipeline.Denial{Stage: "rate_limiting", Status: 429, Message: "denied"},
		ResponseBody: nil,
	}

	// Must not panic on a denied call whose ResponseBody is empty or
	// absent, per the ResponseObserver contract.
	s.Response(context.Background(), call)

	if counter.sent.Load() != 0 || counter.received.Load() != 0 {
		t.Fatalf("expected no tokens counted for a denied call, got sent=%d received=%d",
			counter.sent.Load(), counter.received.Load())
	}
}

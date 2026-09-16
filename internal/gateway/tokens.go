package gateway

import (
	"context"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/agentkeeper/agentkeeper/internal/pipeline"
	"github.com/agentkeeper/agentkeeper/internal/provider"
)

// tokenCounter tracks a running, in-memory total of tokens forwarded
// through the gateway. It resets on process restart.
type tokenCounter struct {
	sent     atomic.Int64
	received atomic.Int64
}

func (c *tokenCounter) add(sent, received int64) {
	c.sent.Add(sent)
	c.received.Add(received)
}

// tokenStage never denies a call; it only observes the response to keep a
// running token total.
type tokenStage struct {
	logger  *zap.Logger
	counter *tokenCounter
}

func (s *tokenStage) Name() string { return "token_counting" }

func (s *tokenStage) Request(_ context.Context, _ *pipeline.Call) (pipeline.Decision, error) {
	return pipeline.Allow(), nil
}

func (s *tokenStage) Response(_ context.Context, c *pipeline.Call) {
	if c.Denial != nil {
		return
	}

	sent, received, ok := provider.ParseUsage(c.ResponseBody)
	if !ok {
		s.logger.Warn("could not find token usage in payload")
		return
	}

	s.counter.add(sent, received)
	s.logger.Info(
		"token usage",
		zap.Int64("total_sent", s.counter.sent.Load()),
		zap.Int64("total_received", s.counter.received.Load()),
	)
}

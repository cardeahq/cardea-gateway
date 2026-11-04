package gateway

import (
	"context"

	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/routing"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type tracingStage struct {
	tracer *trace.Tracer
}

func (s *tracingStage) Name() string { return "tracing" }

func (s *tracingStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	// A nil route only happens in a pipeline built without a routing
	// stage (stage tests); it keeps today's behavior.
	readFull := c.Route == nil || c.Route.Single || c.Format == routing.FormatAnthropic
	opts := trace.RequestOptions{
		ToolResults:  readFull,
		GeminiStream: c.Format == routing.FormatGemini && routing.GeminiStreams(c.Request.URL.Path),
	}
	info, err := s.tracer.Request(c.Request, c.Body, c.AgentID, opts)
	if err != nil {
		return pipeline.Decision{}, err
	}
	return pipeline.AllowTraced(info, s.tracer.NewResponseTap(readFull)), nil
}

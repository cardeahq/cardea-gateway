package gateway

import (
	"context"

	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/provider"
	"github.com/cardeahq/cardea-gateway/internal/routing"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type tracingStage struct {
	tracer *trace.Tracer
	// providers gives each routed call's provider, whose ResponseFormat
	// reads its response.
	providers *provider.Set
}

func (s *tracingStage) Name() string { return "tracing" }

func (s *tracingStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	// Request-side parsing is still Anthropic-shaped. A nil route only
	// happens in a pipeline built without a routing stage (stage tests).
	readFull := c.Route == nil || c.Route.Single || c.Format == routing.FormatAnthropic
	opts := trace.RequestOptions{
		ToolResults:  readFull,
		GeminiStream: c.Format == routing.FormatGemini && routing.GeminiStreams(c.Request.URL.Path),
	}
	info, err := s.tracer.Request(c.Request, c.Body, c.AgentID, opts)
	if err != nil {
		return pipeline.Decision{}, err
	}
	return pipeline.AllowTraced(info, s.tracer.NewResponseTap(s.responseFormat(c))), nil
}

// responseFormat is the format of the provider the call was routed to;
// nil, leaving the response unread, when there is no route.
func (s *tracingStage) responseFormat(c *pipeline.Call) trace.ResponseFormat {
	if c.Route == nil || s.providers == nil {
		return nil
	}
	p, ok := s.providers.Get(c.Route.Provider)
	if !ok {
		return nil
	}
	return p.ResponseFormat()
}

package gateway

import (
	"context"

	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type tracingStage struct {
	tracer *trace.Tracer
}

func (s *tracingStage) Name() string { return "tracing" }

func (s *tracingStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	info, err := s.tracer.Request(c.Request, c.Body, c.AgentID)
	if err != nil {
		return pipeline.Decision{}, err
	}
	return pipeline.AllowTraced(info, s.tracer.NewResponseTap()), nil
}

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
	opts := trace.RequestOptions{
		Format:       string(c.Format),
		Operation:    string(c.Operation),
		GeminiStream: c.Format == routing.FormatGemini && routing.GeminiStreams(c.Request.URL.Path),
	}
	if c.Format == routing.FormatGemini {
		// Read from the URL here rather than c.Model, which routing
		// leaves nil in single mode.
		if m, found, _ := routing.ExtractModel(c.Format, c.Request, nil); found {
			opts.Model = &m
		}
	}
	info, err := s.tracer.Request(c.Request, c.Body, c.AgentID, opts)
	if err != nil {
		return pipeline.Decision{}, err
	}
	return pipeline.AllowTraced(info, s.tracer.NewResponseTap(string(c.Format), string(c.Operation))), nil
}

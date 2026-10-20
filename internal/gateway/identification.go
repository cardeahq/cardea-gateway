package gateway

import (
	"context"

	"github.com/cardeahq/cardea-gateway/internal/identification"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
)

const identificationStageName = "identification"

type identificationStage struct {
	registry *identification.Registry
}

func (s *identificationStage) Name() string { return identificationStageName }

func (s *identificationStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	return pipeline.AllowAs(s.registry.Identify(c.Request)), nil
}

package gateway

import (
	"context"
	"net/http"

	"github.com/cardeahq/cardea-gateway/internal/authorization"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
)

const authorizationDenialMessage = "not authorized"

type authorizationStage struct {
	gate *authorization.Gate
}

func (s *authorizationStage) Name() string { return "authorization" }

func (s *authorizationStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	result := s.gate.Authorize(c.Request.Header.Get("Authorization"))
	if !result.OK {
		return pipeline.Deny(http.StatusUnauthorized, authorizationDenialMessage, string(result.Reason)), nil
	}
	return pipeline.Allow(), nil
}

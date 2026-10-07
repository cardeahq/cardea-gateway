package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/policy"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type policiesStage struct {
	engine *policy.Engine
	logger *zap.Logger
}

func (s *policiesStage) Name() string { return "policies" }

func (s *policiesStage) Request(ctx context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	req := policy.Request{
		CallID:  c.ID,
		Method:  c.Request.Method,
		Path:    c.Request.URL.Path,
		Headers: c.Request.Header,
		Body:    c.Body,
		AgentID: c.AgentID,
	}
	if c.Trace != nil && c.Trace.Session.Status == "claimed" && c.Trace.Session.ID != nil {
		req.SessionID = *c.Trace.Session.ID
	}

	res, err := s.engine.Evaluate(ctx, req)
	if err != nil {
		kind := "internal"
		var ee *policy.EvalError
		if errors.As(err, &ee) {
			kind = ee.Kind
		}
		if s.logger != nil {
			s.logger.Error("policy evaluation failed",
				zap.String("call_id", c.ID), zap.String("kind", kind), zap.String("revision", s.engine.Revision()))
		}
		return pipeline.Decision{}, err
	}

	decision := pipeline.Allow()
	if len(res.Rules) > 0 {
		reason := "rules=" + strings.Join(res.Rules, ",") + " revision=" + res.Revision
		decision = pipeline.Deny(http.StatusForbidden, "request denied by policy", reason)
	}
	if res.Revision == "" {
		return decision, nil
	}
	rules := res.Rules
	if rules == nil {
		rules = []string{}
	}
	return decision.WithDetail(trace.StageDetail{Revision: res.Revision, Rules: rules}), nil
}

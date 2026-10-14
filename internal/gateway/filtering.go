package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/filtering"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type filteringStage struct {
	engine *filtering.Engine
	logger *zap.Logger
}

func (s *filteringStage) Name() string { return "filtering" }

func (s *filteringStage) Request(ctx context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	res, err := s.engine.Redact(ctx, filtering.Request{CallID: c.ID, Path: c.Request.URL.Path, Body: c.Body})
	if err != nil {
		kind := "internal"
		var fe *filtering.FilterError
		if errors.As(err, &fe) {
			kind = fe.Kind
		}
		if s.logger != nil {
			s.logger.Error("filtering failed", zap.String("call_id", c.ID), zap.String("kind", kind))
		}
		return pipeline.Decision{}, err
	}

	if len(res.Counts) == 0 {
		return pipeline.Allow(), nil
	}

	c.Body = res.Body
	c.Request.Body = io.NopCloser(bytes.NewReader(res.Body))
	c.Request.ContentLength = int64(len(res.Body))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(res.Body)))

	counts := make(map[string]int, len(res.Counts))
	fields := []zap.Field{zap.String("call_id", c.ID)}
	for _, cat := range []filtering.Category{filtering.CategoryEmail, filtering.CategorySSN, filtering.CategoryAPIKey} {
		if n, ok := res.Counts[cat]; ok {
			counts[string(cat)] = n
			fields = append(fields, zap.Int(string(cat), n))
		}
	}
	if s.logger != nil {
		s.logger.Info("request filtered", fields...)
	}
	return pipeline.Allow().WithDetail(trace.StageDetail{Redactions: counts}), nil
}

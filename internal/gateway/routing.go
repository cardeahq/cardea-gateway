package gateway

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/provider"
	"github.com/cardeahq/cardea-gateway/internal/routing"
)

type routingStage struct {
	table *routing.Table
}

func (s *routingStage) Name() string { return "routing" }

func (s *routingStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	if s.table.Single() {
		r, _ := s.table.Resolve(routing.FormatUnknown, "", false)
		return pipeline.AllowRouted(r, routing.FormatOf(r.Kind), nil), nil
	}

	format := routing.DetectFormat(c.Request)
	model, found, err := routing.ExtractModel(format, c.Request, c.Body)
	if err != nil {
		return pipeline.DenyWithModel(http.StatusBadRequest, "the request names its model more than once", "repeated_model", nil), nil
	}

	var modelPtr *string
	if found {
		modelPtr = &model
	}

	route, rej := s.table.Resolve(format, model, found)
	if rej == routing.RejectNone {
		return pipeline.AllowRouted(route, format, modelPtr), nil
	}

	status, message := rejectionResponse(rej)
	reason := string(rej) + " format=" + string(format)
	if found {
		reason += " model=" + routing.Truncate(model)
	}
	return pipeline.DenyWithModel(status, message, reason, modelPtr), nil
}

func rejectionResponse(rej routing.Rejection) (status int, message string) {
	switch rej {
	case routing.RejectFormatMismatch:
		return http.StatusBadRequest, "the model is not available for this API"
	default: // RejectNoProvider, RejectUnknownFormat
		return http.StatusNotFound, "no provider serves this request"
	}
}

// dispatcher sends a routed request to its provider's handler.
type dispatcher struct {
	set    *provider.Set
	logger *zap.Logger
}

func (d *dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := pipeline.RouteFrom(r.Context())
	if !ok {
		d.logError("dispatcher: no route in request context; the pipeline must run a routing stage before the dispatcher")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	p, ok := d.set.Get(route.Provider)
	if !ok {
		d.logError("dispatcher: no provider configured under the routed name", zap.String("provider", route.Provider))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	p.Handler().ServeHTTP(w, r)
}

func (d *dispatcher) logError(msg string, fields ...zap.Field) {
	if d.logger != nil {
		d.logger.Error(msg, fields...)
	}
}

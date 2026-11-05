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
	format := routing.DetectFormat(c.Request)
	if format == routing.FormatUnknown {
		return deny(routing.RejectUnknownFormat, format, "", false), nil
	}
	op := routing.DetectOperation(format, c.Request)
	if op == routing.OpUnsupported {
		reason := string(routing.RejectUnsupportedOp) + " format=" + string(format) +
			" method=" + c.Request.Method + " path=" + routing.Truncate(c.Request.URL.Path)
		status, message := rejectionResponse(routing.RejectUnsupportedOp)
		return pipeline.DenyWithModel(status, message, reason, nil), nil
	}

	if s.table.Single() {
		route, rej := s.table.Resolve(format, "", false)
		if rej != routing.RejectNone {
			return deny(rej, format, "", false), nil
		}
		return pipeline.AllowRouted(route, format, op, nil), nil
	}

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
		return pipeline.AllowRouted(route, format, op, modelPtr), nil
	}
	return deny(rej, format, model, found), nil
}

// deny builds a routing denial whose reason names the rejection, the
// format and, when one was found, the truncated model.
func deny(rej routing.Rejection, format routing.Format, model string, found bool) pipeline.Decision {
	status, message := rejectionResponse(rej)
	reason := string(rej) + " format=" + string(format)
	var modelPtr *string
	if found {
		reason += " model=" + routing.Truncate(model)
		modelPtr = &model
	}
	return pipeline.DenyWithModel(status, message, reason, modelPtr)
}

func rejectionResponse(rej routing.Rejection) (status int, message string) {
	switch rej {
	case routing.RejectFormatMismatch:
		return http.StatusBadRequest, "the model is not available for this API"
	default: // no_provider, unknown_format, unsupported_operation, format_not_served
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

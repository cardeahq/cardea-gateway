package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/cardeahq/cardea-gateway/internal/routing"
)

// specs/0009: the routing stage attaches a route, format and model to the
// call via AllowRouted, or denies with just the model via DenyWithModel.
// These tests pin down what the pipeline does with either decision,
// independent of any particular routing algorithm.

func testRoute() routing.Route {
	return routing.Route{Provider: "anthropic", Kind: "anthropic", Formats: []routing.Format{routing.FormatAnthropic}, Patterns: []string{"claude-*"}}
}

// routedStage always allows, attaching route/format/model via AllowRouted.
type routedStage struct {
	name   string
	route  routing.Route
	format routing.Format
	model  *string
}

func (s *routedStage) Name() string { return s.name }
func (s *routedStage) Request(context.Context, *Call) (Decision, error) {
	return AllowRouted(s.route, s.format, s.model), nil
}

// deniedWithModelStage always denies via DenyWithModel.
type deniedWithModelStage struct {
	name    string
	status  int
	message string
	reason  string
	model   *string
}

func (s *deniedWithModelStage) Name() string { return s.name }
func (s *deniedWithModelStage) Request(context.Context, *Call) (Decision, error) {
	return DenyWithModel(s.status, s.message, s.reason, s.model), nil
}

func TestAllowRouted_PopulatesCallFields(t *testing.T) {
	// A stage only ever sees Call fields as the pipeline applied them
	// *before* that stage ran, so this is observed via the next stage,
	// exactly as AgentID is elsewhere in this package's tests.
	route := testRoute()
	model := "claude-sonnet-5"
	routingStage := &fakeStage{name: "routing", decision: AllowRouted(route, routing.FormatAnthropic, &model)}
	next := &fakeStage{name: "tracing", decision: Allow()}

	p := New(zaptest.NewLogger(t), 1<<20, nil, routingStage, next)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	doRequest(t, p, upstream, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if next.lastCall == nil {
		t.Fatal("stage never ran")
	}
	c := next.lastCall
	if c.Route == nil || !reflect.DeepEqual(*c.Route, route) {
		t.Fatalf("Call.Route = %+v, want %+v", c.Route, route)
	}
	if c.Format != routing.FormatAnthropic {
		t.Fatalf("Call.Format = %q, want %q", c.Format, routing.FormatAnthropic)
	}
	if c.Model == nil || *c.Model != model {
		t.Fatalf("Call.Model = %v, want %q", c.Model, model)
	}
}

func TestAllowRouted_ALaterPlainAllowCannotOverwriteTheRoute(t *testing.T) {
	route := testRoute()
	model := "claude-sonnet-5"
	routing1 := &fakeStage{name: "routing", decision: AllowRouted(route, routing.FormatAnthropic, &model)}
	later := &fakeStage{name: "tracing", decision: Allow()}

	p := New(zaptest.NewLogger(t), 1<<20, nil, routing1, later)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if later.lastCall == nil {
		t.Fatal("later stage never ran")
	}
	c := later.lastCall
	if c.Route == nil || !reflect.DeepEqual(*c.Route, route) {
		t.Fatalf("Call.Route = %+v after a later plain Allow(), want it to still be %+v", c.Route, route)
	}
	if c.Format != routing.FormatAnthropic {
		t.Fatalf("Call.Format = %q after a later plain Allow(), want %q", c.Format, routing.FormatAnthropic)
	}
	if c.Model == nil || *c.Model != model {
		t.Fatalf("Call.Model = %v after a later plain Allow(), want %q", c.Model, model)
	}
}

func TestDenyWithModel_SetsCallModelButNoRoute(t *testing.T) {
	model := "mistral-large-latest"
	stage := &fakeObserverStage{fakeStage: &fakeStage{
		name:     "routing",
		decision: DenyWithModel(http.StatusBadRequest, "the model is not available for this API", "format_mismatch", &model),
	}}

	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("next must not be called") })
	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if stage.lastResponse == nil {
		t.Fatal("observer never ran")
	}
	if stage.lastResponse.Model == nil || *stage.lastResponse.Model != model {
		t.Fatalf("Call.Model = %v, want %q", stage.lastResponse.Model, model)
	}
	if stage.lastResponse.Route != nil {
		t.Fatalf("Call.Route = %+v, want nil: DenyWithModel never attaches a route", stage.lastResponse.Route)
	}
}

// TestPipeline_RouteAttachedToUpstreamRequestContext: RouteFrom sees the
// route the pipeline attached, in the upstream handler, exactly when a
// stage allowed the call with AllowRouted.
func TestPipeline_RouteAttachedToUpstreamRequestContext(t *testing.T) {
	route := testRoute()
	stage := &routedStage{name: "routing", route: route, format: routing.FormatAnthropic}

	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	var gotRoute routing.Route
	var found bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRoute, found = RouteFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if !found {
		t.Fatal("RouteFrom did not find a route in the upstream handler's request context")
	}
	if !reflect.DeepEqual(gotRoute, route) {
		t.Fatalf("RouteFrom = %+v, want %+v", gotRoute, route)
	}
}

// TestPipeline_NoRoutingStageForwardsWithoutPanickingAndWithNoRouteInContext
// keeps existing tests, built without a routing stage, working: the
// dispatcher's no-route path (an internal error in production, since
// routing always runs there) must still be reachable without the
// pipeline itself panicking.
func TestPipeline_NoRoutingStageForwardsWithoutPanickingAndWithNoRouteInContext(t *testing.T) {
	stage := &fakeStage{name: "identification", decision: Allow()}

	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	var found bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, found = RouteFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: a pipeline with no routing stage must still forward", rec.Code, http.StatusOK)
	}
	if found {
		t.Fatal("RouteFrom found a route although no stage ever attached one")
	}
}

// --- AC18 (specs/0009-functional): a DenyWithModel request record names
// the model, has no provider key, and no response record is written. ---

func TestDenyWithModel_RequestRecordNamesModelHasNoProviderAndNoResponseRecord(t *testing.T) {
	model := "mistral-large-latest"
	stage := &deniedWithModelStage{
		name: "routing", status: http.StatusBadRequest,
		message: "the model is not available for this API",
		reason:  "format_mismatch format=anthropic model=mistral-large-latest",
		model:   &model,
	}
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("next must not be called") })
	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	req := sink.one(t, "request")
	if req["model"] != model {
		t.Fatalf("request record model = %v, want %q", req["model"], model)
	}
	if _, present := req["provider"]; present {
		t.Fatalf("request record has a provider key = %v, want it entirely absent", req["provider"])
	}
	if n := len(sink.byType("response")); n != 0 {
		t.Fatalf("a denied call has %d response records, want 0", n)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if env.Error.Message != "the model is not available for this API" {
		t.Fatalf("caller message = %q, want the model-not-available message", env.Error.Message)
	}
}

func TestDenyWithModel_WithNilModelLeavesCallModelNil(t *testing.T) {
	stage := &deniedWithModelStage{name: "routing", status: http.StatusNotFound, message: "no provider serves this request", reason: "no_provider format=anthropic", model: nil}
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("next must not be called") })
	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	req := sink.one(t, "request")
	if v, present := req["model"]; !present || v != nil {
		t.Fatalf("request record model = %#v, want an explicit null", v)
	}
}

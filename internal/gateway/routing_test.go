package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/provider"
	"github.com/cardeahq/cardea-gateway/internal/routing"
)

var _ pipeline.Stage = (*routingStage)(nil)

func anthropicEntry(name string) routing.Route {
	return routing.Route{Provider: name, Kind: "anthropic", Formats: []routing.Format{routing.FormatAnthropic}, Patterns: []string{"claude-*"}}
}

func mistralEntry(name string) routing.Route {
	return routing.Route{Provider: name, Kind: "mistral", Formats: []routing.Format{routing.FormatOpenAI}, Patterns: []string{"mistral-*"}}
}

func mustRoutingTable(t *testing.T, routes []routing.Route, defaults map[routing.Format]string) *routing.Table {
	t.Helper()
	tbl, err := routing.NewTable(routes, defaults)
	if err != nil {
		t.Fatalf("routing.NewTable: %v", err)
	}
	return tbl
}

func TestRoutingStage_Name(t *testing.T) {
	if got := (&routingStage{}).Name(); got != "routing" {
		t.Fatalf("Name() = %q, want %q", got, "routing")
	}
}

func TestRoutingStage_AllowRoutedPopulatesTheCall(t *testing.T) {
	tbl := mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic")}, nil)
	stage := &routingStage{table: tbl}
	capture := &capturingStage{}

	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, stage, capture)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	c := capture.lastCall()
	if c == nil {
		t.Fatal("capturing stage never ran")
	}
	if c.Route == nil || c.Route.Provider != "anthropic" {
		t.Fatalf("Call.Route = %+v, want provider anthropic", c.Route)
	}
	if c.Format != routing.FormatAnthropic {
		t.Fatalf("Call.Format = %q, want %q", c.Format, routing.FormatAnthropic)
	}
	if c.Operation != routing.OpMessages {
		t.Fatalf("Call.Operation = %q, want %q", c.Operation, routing.OpMessages)
	}
	if c.Model == nil || *c.Model != "claude-sonnet-5" {
		t.Fatalf("Call.Model = %v, want claude-sonnet-5", c.Model)
	}
}

func TestRoutingStage_SingleModeSkipsModelExtraction(t *testing.T) {
	tbl := routing.NewSingleTable(routing.Route{Provider: "anthropic", Kind: "anthropic", Formats: []routing.Format{routing.FormatAnthropic}, Single: true})
	stage := &routingStage{table: tbl}
	capture := &capturingStage{}

	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, stage, capture)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// A body that would be a 400 (repeated model) in list mode.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"a","model":"b"}`))
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: single mode does not extract the model, so a repeated one is not a routing denial", rec.Code)
	}
	c := capture.lastCall()
	if c == nil || c.Route == nil || !c.Route.Single {
		t.Fatalf("Call.Route = %+v, want the single route", c.Route)
	}
	if c.Model != nil {
		t.Fatalf("Call.Model = %v, want nil: single mode does not extract a model", c.Model)
	}
	if c.Format != routing.FormatAnthropic || c.Operation != routing.OpMessages {
		t.Fatalf("Call.Format/Operation = %q/%q, want anthropic/messages: single mode detects both", c.Format, c.Operation)
	}
}

func TestRoutingStage_SingleModeSetsTheDetectedOperation(t *testing.T) {
	tbl := routing.NewSingleTable(routing.Route{Provider: "mistral", Kind: "mistral", Formats: []routing.Format{routing.FormatOpenAI}, Single: true})
	tests := []struct {
		method, path string
		want         routing.Operation
	}{
		{http.MethodPost, "/v1/chat/completions", routing.OpMessages},
		{http.MethodPost, "/v1/fim/completions", routing.OpCompletion},
		{http.MethodPost, "/v1/embeddings", routing.OpEmbeddings},
		{http.MethodGet, "/v1/models", routing.OpModels},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			capture := &capturingStage{}
			p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, &routingStage{table: tbl}, capture)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			rec := httptest.NewRecorder()
			p.Handler(next).ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{"model":"anything"}`)))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			c := capture.lastCall()
			if c == nil || c.Format != routing.FormatOpenAI || c.Operation != tt.want {
				t.Fatalf("Call = %+v, want format openai and operation %q", c, tt.want)
			}
		})
	}
}

func TestRoutingStage_SingleModeRejections(t *testing.T) {
	// specs/0009 functional rule 4: single mode rejects another format and
	// any unsupported operation, as list mode does (rule 10).
	tbl := routing.NewSingleTable(routing.Route{Provider: "anthropic", Kind: "anthropic", Formats: []routing.Format{routing.FormatAnthropic}, Single: true})
	tests := []struct {
		name       string
		req        func() *http.Request
		wantReason string
	}{
		{"an OpenAI-style chat request", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
		}, "format_not_served"},
		{"a Gemini generate request", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(`{"contents":[]}`))
		}, "format_not_served"},
		{"an Anthropic message-batches request", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages/batches", strings.NewReader(`{"requests":[]}`))
			r.Header.Set("anthropic-version", "2023-06-01")
			return r
		}, "unsupported_operation"},
		{"an unknown path", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/no/such/route", strings.NewReader(`{}`))
		}, "unknown_format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := observed()
			p := pipeline.New(logger, 1<<20, nil, &routingStage{table: tbl})
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called on a routing denial") })
			rec := httptest.NewRecorder()
			p.Handler(next).ServeHTTP(rec, tt.req())

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			env := decodeErr(t, rec)
			if env.Error.Stage != "routing" || env.Error.Message != "no provider serves this request" {
				t.Fatalf("error = %+v, want stage routing and the generic no-provider message", env.Error)
			}
			if reason := lastWarnReason(logs); !strings.HasPrefix(reason, tt.wantReason) {
				t.Fatalf("logged reason = %q, want it to start with %q", reason, tt.wantReason)
			}
		})
	}
}

func TestRoutingStage_UnsupportedOperationReasonNamesMethodAndPathButNoQuery(t *testing.T) {
	tbl := mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic"), mistralEntry("mistral")}, map[routing.Format]string{routing.FormatOpenAI: "mistral"})
	tests := []struct {
		name       string
		req        func() *http.Request
		wantReason string
	}{
		{"anthropic message batches", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages/batches?beta=true", strings.NewReader(`{"requests":[]}`))
			r.Header.Set("anthropic-version", "2023-06-01")
			return r
		}, "unsupported_operation format=anthropic method=POST path=/v1/messages/batches"},
		{"mistral model deletion", func() *http.Request {
			return httptest.NewRequest(http.MethodDelete, "/v1/models/ft:mistral:abc?key=SECRETQUERY", nil)
		}, "unsupported_operation format=openai method=DELETE path=/v1/models/ft:mistral:abc"},
		{"gemini files", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1beta/files?key=SECRETQUERY", strings.NewReader(`{}`))
		}, "unsupported_operation format=gemini method=POST path=/v1beta/files"},
		{"gemini cached contents", func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1beta/cachedContents", strings.NewReader(`{}`))
		}, "unsupported_operation format=gemini method=POST path=/v1beta/cachedContents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := observed()
			p := pipeline.New(logger, 1<<20, nil, &routingStage{table: tbl})
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called") })
			rec := httptest.NewRecorder()
			p.Handler(next).ServeHTTP(rec, tt.req())

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			reason := lastWarnReason(logs)
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			for _, e := range logs.All() {
				if strings.Contains(e.Message+" "+fmtFields(e.ContextMap()), "SECRETQUERY") {
					t.Fatalf("a log line carries the query string: %s %v", e.Message, e.ContextMap())
				}
			}
		})
	}
}

func TestRoutingStage_UnsupportedOperationPathIsTruncatedLikeAModel(t *testing.T) {
	tbl := mustRoutingTable(t, []routing.Route{mistralEntry("mistral")}, nil)
	logger, logs := observed()
	p := pipeline.New(logger, 1<<20, nil, &routingStage{table: tbl})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called") })
	// POST on one model's path: OpenAI-style format, unsupported operation.
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/models/"+strings.Repeat("a", 400), nil))

	reason := lastWarnReason(logs)
	_, path, ok := strings.Cut(reason, " path=")
	if !ok {
		t.Fatalf("reason = %q, want a path= field", reason)
	}
	if len(path) > routing.MaxModelLen {
		t.Fatalf("logged path is %d bytes, want at most %d", len(path), routing.MaxModelLen)
	}
}

func TestRoutingStage_DenialMapping(t *testing.T) {
	tests := []struct {
		name        string
		table       *routing.Table
		req         func() *http.Request
		wantStatus  int
		wantMessage string
		wantReason  string
	}{
		{
			name:  "repeated top-level model",
			table: mustRoutingTable(t, []routing.Route{mistralEntry("mistral")}, nil),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"a","model":"b"}`))
			},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "the request names its model more than once",
			wantReason:  "repeated_model",
		},
		{
			name:  "format mismatch: model matches a provider of the wrong format",
			table: mustRoutingTable(t, []routing.Route{mistralEntry("mistral")}, nil),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"mistral-large-latest","messages":[]}`))
			},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "the model is not available for this API",
			wantReason:  "format_mismatch",
		},
		{
			name:  "no provider: no model and no default",
			table: mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic")}, nil),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
			},
			wantStatus:  http.StatusNotFound,
			wantMessage: "no provider serves this request",
			wantReason:  "no_provider",
		},
		{
			name:  "unsupported operation: a supported path with the wrong method",
			table: mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic")}, nil),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
			},
			wantStatus:  http.StatusNotFound,
			wantMessage: "no provider serves this request",
			wantReason:  "unsupported_operation",
		},
		{
			name:  "unsupported operation is checked before the model",
			table: mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic")}, nil),
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/v1/messages/batches", strings.NewReader(`{"model":"claude-sonnet-5","model":"x"}`))
				r.Header.Set("anthropic-version", "2023-06-01")
				return r
			},
			wantStatus:  http.StatusNotFound,
			wantMessage: "no provider serves this request",
			wantReason:  "unsupported_operation",
		},
		{
			name:  "unknown format",
			table: mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic")}, nil),
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/no/such/route", strings.NewReader(`{}`))
			},
			wantStatus:  http.StatusNotFound,
			wantMessage: "no provider serves this request",
			wantReason:  "unknown_format",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := observed()
			stage := &routingStage{table: tt.table}
			p := pipeline.New(logger, 1<<20, nil, stage)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called on a routing denial") })
			rec := httptest.NewRecorder()
			p.Handler(next).ServeHTTP(rec, tt.req())

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			env := decodeErr(t, rec)
			if env.Error.Stage != "routing" {
				t.Fatalf("error.stage = %q, want routing", env.Error.Stage)
			}
			if env.Error.Message != tt.wantMessage {
				t.Fatalf("error.message = %q, want %q", env.Error.Message, tt.wantMessage)
			}
			var reason string
			for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
				if r, ok := e.ContextMap()["reason"].(string); ok {
					reason = r
				}
			}
			if !strings.HasPrefix(reason, tt.wantReason) {
				t.Fatalf("logged reason = %q, want it to start with %q", reason, tt.wantReason)
			}
			if got := rec.Header().Get("X-Should-Retry"); got != "false" {
				t.Fatalf("X-Should-Retry = %q, want false", got)
			}
		})
	}
}

func TestRoutingStage_DenialReasonNamesFormatAndModelWhenFound(t *testing.T) {
	logger, logs := observed()
	tbl := mustRoutingTable(t, []routing.Route{mistralEntry("mistral")}, nil)
	stage := &routingStage{table: tbl}
	p := pipeline.New(logger, 1<<20, nil, stage)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called") })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"mistral-large-latest","messages":[]}`))
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var reason string
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if r, ok := e.ContextMap()["reason"].(string); ok {
			reason = r
		}
	}
	if !strings.Contains(reason, "format=anthropic") || !strings.Contains(reason, "model=mistral-large-latest") {
		t.Fatalf("reason = %q, want it to name both the format and the model", reason)
	}
}

func TestRoutingStage_DenialReasonOmitsModelWhenNoneFound(t *testing.T) {
	logger, logs := observed()
	tbl := mustRoutingTable(t, []routing.Route{anthropicEntry("anthropic")}, nil)
	stage := &routingStage{table: tbl}
	p := pipeline.New(logger, 1<<20, nil, stage)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called") })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	var reason string
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if r, ok := e.ContextMap()["reason"].(string); ok {
			reason = r
		}
	}
	if strings.Contains(reason, "model=") {
		t.Fatalf("reason = %q, want no model= when routing found none", reason)
	}
}

// --- dispatcher --------------------------------------------------------------

func TestDispatcher_RoutesToTheNamedProvidersHandler(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	t.Setenv("TEST_DISPATCHER_KEY", "sk-secret")
	prov, err := provider.NewAnthropicProvider(upstream.URL, "TEST_DISPATCHER_KEY")
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	set := provider.NewSingleSet("anthropic", prov)
	tbl := routing.NewSingleTable(routing.Route{Provider: "anthropic", Kind: "anthropic", Formats: []routing.Format{routing.FormatAnthropic}, Single: true})

	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, &routingStage{table: tbl})
	rec := httptest.NewRecorder()
	p.Handler(&dispatcher{set: set}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	if !reached {
		t.Fatal("the dispatcher did not forward the call to the routed provider")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestDispatcher_NoRouteInContextRespondsFiveHundredAndForwardsNothing(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	t.Setenv("TEST_DISPATCHER_KEY_2", "sk-secret")
	prov, err := provider.NewAnthropicProvider(upstream.URL, "TEST_DISPATCHER_KEY_2")
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	set := provider.NewSingleSet("anthropic", prov)

	d := &dispatcher{set: set}
	rec := httptest.NewRecorder()
	// No pipeline at all: the request context carries no route, the way a
	// pipeline wired without a routing stage would leave it.
	d.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if reached {
		t.Fatal("the dispatcher forwarded a call with no route in context")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestDispatcher_RouteNamingAnUnknownProviderRespondsFiveHundred(t *testing.T) {
	logger, logs := observed()
	tbl := mustRoutingTable(t, []routing.Route{anthropicEntry("ghost")}, nil)
	set, err := provider.NewSet(nil) // empty set: "ghost" is not in it
	if err != nil {
		t.Fatalf("provider.NewSet(nil): %v", err)
	}

	p := pipeline.New(logger, 1<<20, nil, &routingStage{table: tbl})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	p.Handler(&dispatcher{set: set, logger: logger}).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n == 0 {
		t.Fatal("no error-level line logged for a route naming an unconfigured provider")
	}
}

func lastWarnReason(logs *observer.ObservedLogs) string {
	var reason string
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if r, ok := e.ContextMap()["reason"].(string); ok {
			reason = r
		}
	}
	return reason
}

func fmtFields(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprint(&b, v)
		b.WriteByte(' ')
	}
	return b.String()
}

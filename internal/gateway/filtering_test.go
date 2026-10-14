package gateway

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"

	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/filtering"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
)

var _ pipeline.Stage = (*filteringStage)(nil)

// filteringStage must not observe responses (specs/0006: request only,
// matching the functional non-goal that responses are untouched).
var _ = func() bool {
	var s pipeline.Stage = &filteringStage{}
	if _, isObserver := s.(pipeline.ResponseObserver); isObserver {
		panic("filteringStage implements ResponseObserver; filtering never sees a response")
	}
	return true
}()

const (
	filterEmail = "person@example.com"
	filterSSN   = "123-45-6789"
)

func filteringCfg() config.FilteringConfig {
	return config.FilteringConfig{
		Categories: config.CategoriesConfig{Email: true, SSN: true, APIKey: true},
		Cache:      config.FilterCacheConfig{Backend: "memory", Capacity: 1000, TTL: time.Minute},
	}
}

func filteringGatewayConfig(t *testing.T, upstreamURL string) config.Config {
	t.Helper()
	cfg := tracingGatewayConfig(t, upstreamURL)
	cfg.Pipeline.Stages.Filtering = true
	cfg.Filtering = filteringCfg()
	return cfg
}

func filterBody(system string) string {
	return `{"model":"claude-sonnet-5","system":"` + system + `","messages":[{"role":"user","content":"hi"}]}`
}

// --- buildStages --------------------------------------------------------------

func TestBuildStages_FilteringComesAfterPoliciesAndFollowsItsConfigBoolean(t *testing.T) {
	tests := []struct {
		name                                             string
		identification, tracing, policies, filterEnabled bool
		want                                             []string
	}{
		{"all four on", true, true, true, true, []string{"identification", "tracing", "policies", "filtering"}},
		{"filtering off", true, true, true, false, []string{"identification", "tracing", "policies"}},
		{"filtering on, policies off", true, true, false, true, []string{"identification", "tracing", "filtering"}},
		{"filtering only", false, false, false, true, []string{"filtering"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildCfg(t, tt.identification, tt.tracing, tt.policies, "")
			cfg.Pipeline.Stages.Filtering = tt.filterEnabled
			cfg.Filtering = filteringCfg()

			stages, sink, _, fEng, err := buildStages(cfg, zaptest.NewLogger(t))
			if err != nil {
				t.Fatalf("buildStages: %v", err)
			}
			if sink != nil {
				t.Cleanup(func() { _ = sink.Close() })
			}
			if fEng != nil {
				t.Cleanup(func() { _ = fEng.Close() })
			}
			var names []string
			for _, s := range stages {
				names = append(names, s.Name())
			}
			if !slices.Equal(names, tt.want) {
				t.Fatalf("stage order = %v, want %v", names, tt.want)
			}
			if tt.filterEnabled && fEng == nil {
				t.Fatal("buildStages returned a nil filtering engine with the filtering stage on")
			}
			if !tt.filterEnabled && fEng != nil {
				t.Fatal("buildStages returned a non-nil filtering engine with the filtering stage off")
			}
		})
	}
}

func TestBuildStages_FilteringEngineErrorFailsConstruction(t *testing.T) {
	cfg := buildCfg(t, false, true, false, "")
	cfg.Pipeline.Stages.Filtering = true
	cfg.Filtering = filteringCfg()
	cfg.Filtering.Cache.Capacity = 0 // invalid: NewMemoryCache requires a positive capacity

	stages, sink, _, fEng, err := buildStages(cfg, zaptest.NewLogger(t))
	if sink != nil {
		_ = sink.Close()
	}
	if err == nil {
		t.Fatal("buildStages = nil error, want the filtering engine construction failure")
	}
	if stages != nil || fEng != nil {
		t.Fatalf("stages=%v fEng=%v, want nil on error", stages, fEng)
	}
}

// --- the stage itself, against a real engine ----------------------------------

func TestFilteringStage_RedactionAttachesDetailAndLogsCategoriesInFixedOrder(t *testing.T) {
	eng, err := filtering.New(filteringCfg(), "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("filtering.New: %v", err)
	}
	defer func() { _ = eng.Close() }()

	logger, logs := observed()
	sink := &captureSink{}
	p := pipeline.New(logger, 1<<20, sink, &filteringStage{engine: eng, logger: logger})

	var upstreamCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(filterBody(filterEmail+" "+filterSSN))))

	if rec.Code != http.StatusOK || !upstreamCalled {
		t.Fatalf("status=%d upstreamCalled=%v, want 200 and the call forwarded", rec.Code, upstreamCalled)
	}

	req := sink.request(t)
	st := stageEntry(t, req, "filtering")
	if st["decision"] != "allowed" {
		t.Fatalf("filtering decision = %v, want allowed", st["decision"])
	}
	detail, ok := st["detail"].(map[string]any)
	if !ok {
		t.Fatalf("detail = %#v, want an object", st["detail"])
	}
	redactions, ok := detail["redactions"].(map[string]any)
	if !ok {
		t.Fatalf("detail.redactions = %#v, want an object", detail["redactions"])
	}
	if redactions["email"] != float64(1) || redactions["ssn"] != float64(1) {
		t.Fatalf("detail.redactions = %v, want email:1 ssn:1", redactions)
	}
	if _, present := redactions["api_key"]; present {
		t.Fatalf("detail.redactions = %v, want no api_key key: nothing of that category was found", redactions)
	}

	var found bool
	for _, e := range logs.FilterLevelExact(zapcore.InfoLevel).All() {
		if e.Message != "request filtered" {
			continue
		}
		var keys []string
		for _, f := range e.Context {
			keys = append(keys, f.Key)
		}
		if slices.Equal(keys, []string{"call_id", "email", "ssn"}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no \"request filtered\" line with fields in the fixed order [call_id email ssn]; logs:\n%s", logText(logs))
	}
}

func TestFilteringStage_ZeroRedactionsAttachesNoDetailAndLogsNothing(t *testing.T) {
	eng, err := filtering.New(filteringCfg(), "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("filtering.New: %v", err)
	}
	defer func() { _ = eng.Close() }()

	logger, logs := observed()
	sink := &captureSink{}
	p := pipeline.New(logger, 1<<20, sink, &filteringStage{engine: eng, logger: logger})

	rec := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(filterBody("nothing to see here"))))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	st := stageEntry(t, sink.request(t), "filtering")
	if st["decision"] != "allowed" {
		t.Fatalf("decision = %v, want allowed", st["decision"])
	}
	if v, present := st["detail"]; !present || v != nil {
		t.Fatalf("detail = %#v, want an explicit null when nothing was redacted", v)
	}
	for _, e := range logs.All() {
		if e.Level >= zapcore.InfoLevel {
			t.Fatalf("a call with nothing detected wrote a %s log line: %s", e.Level, e.Message)
		}
	}
}

func TestFilteringStage_EngineErrorBecomesThe503AndTheUpstreamIsNeverCalled(t *testing.T) {
	eng, err := filtering.New(filteringCfg(), "anthropic", zap.NewNop())
	if err != nil {
		t.Fatalf("filtering.New: %v", err)
	}
	if err := eng.Close(); err != nil { // forces any future cache.Set to fail
		t.Fatalf("Close: %v", err)
	}

	logger, logs := observed()
	sink := &captureSink{}
	p := pipeline.New(logger, 1<<20, sink, &filteringStage{engine: eng, logger: logger})

	var upstreamCalled bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalled = true })
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(filterBody(filterEmail))))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if upstreamCalled {
		t.Fatal("the upstream received a call whose redaction could not be written to the cache")
	}
	e := decodeErr(t, rec)
	if e.Error.Type != "gateway_unavailable" || e.Error.Stage != "filtering" {
		t.Fatalf("error = %+v, want gateway_unavailable from filtering", e.Error)
	}
	if d := stageEntry(t, sink.request(t), "filtering")["decision"]; d != "failed" {
		t.Fatalf("filtering decision = %v, want failed", d)
	}

	var logged bool
	for _, le := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		ctx := le.ContextMap()
		if ctx["kind"] == "cache_write" && ctx["call_id"] != nil {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no error-level line names kind=cache_write; logs:\n%s", logText(logs))
	}
}

// --- end to end: a real Engine behind the real pipeline and provider -----------

func TestFiltering_ContentLengthMatchesTheRewrittenBodyNotTheOriginal(t *testing.T) {
	f, url := newFakeAnthropic(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	cfg := filteringGatewayConfig(t, url)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	body := filterBody(filterEmail)
	rec := serve(g, post(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	got := f.last(t)
	if len(got.body) == len(body) {
		t.Fatalf("provider received %d bytes, same length as the original request: the body was not rewritten", len(got.body))
	}
	if got.contentLength != int64(len(got.body)) {
		t.Fatalf("r.ContentLength = %d, want %d (the rewritten body's length)", got.contentLength, len(got.body))
	}
	if cl := got.header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(got.body)) {
		t.Fatalf("Content-Length header = %q, want %d", cl, len(got.body))
	}
}

func TestFiltering_EndToEnd(t *testing.T) {
	t.Run("default config redacts an email and the response is relayed byte-identical (criteria 1, 11)", func(t *testing.T) {
		const upstreamBody = `{"id":"msg_1","type":"message","content":[{"type":"text","text":"[email:not-a-real-placeholder]"}]}`
		f, url := newFakeAnthropic(t, func(w http.ResponseWriter, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(upstreamBody))
		})
		cfg := filteringGatewayConfig(t, url)
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

		rec := serve(g, post(filterBody(filterEmail)))
		if rec.Code != http.StatusOK || rec.Body.String() != upstreamBody {
			t.Fatalf("status=%d body=%q, want 200 and the provider's response unchanged (filtering never touches responses)", rec.Code, rec.Body.String())
		}
		got := f.last(t).body
		if strings.Contains(string(got), filterEmail) {
			t.Fatalf("provider received the real email: %s", got)
		}
		if !strings.Contains(string(got), "[email:") {
			t.Fatalf("provider did not receive a placeholder: %s", got)
		}
	})

	t.Run("no PII reaches the provider byte-identical (criterion 5)", func(t *testing.T) {
		f, url := newFakeAnthropic(t, loopResponder)
		cfg := filteringGatewayConfig(t, url)
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

		rec := serve(g, post(bodyA))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := string(f.last(t).body); got != bodyA {
			t.Fatalf("provider received %q, want the request byte-identical", got)
		}
	})

	t.Run("stage disabled forwards PII of every category unredacted (criterion 7)", func(t *testing.T) {
		f, url := newFakeAnthropic(t, loopResponder)
		cfg := filteringGatewayConfig(t, url)
		cfg.Pipeline.Stages.Filtering = false
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

		body := filterBody(filterEmail + " " + filterSSN)
		rec := serve(g, post(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := string(f.last(t).body); got != body {
			t.Fatalf("provider received %q, want the unredacted body unchanged with the stage off", got)
		}
	})

	t.Run("a cache write failure denies with 503 and the upstream is never called (criterion 10)", func(t *testing.T) {
		f, url := newFakeAnthropic(t, loopResponder)
		cfg := filteringGatewayConfig(t, url)
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
		// Force the cache write that a redaction needs to fail: closing the
		// engine closes its cache, and Set on a closed cache errors.
		if g.filterEngine == nil {
			t.Fatal("gateway has no filterEngine with filtering enabled")
		}
		if err := g.filterEngine.Close(); err != nil {
			t.Fatalf("filterEngine.Close: %v", err)
		}

		rec := serve(g, post(filterBody(filterEmail)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if f.count() != 0 {
			t.Fatalf("provider calls = %d, want 0: the call must never reach it unredacted or partially redacted", f.count())
		}
	})

	t.Run("PII only in a tool_use argument and a tool_result is still redacted (criterion 12)", func(t *testing.T) {
		f, url := newFakeAnthropic(t, loopResponder)
		cfg := filteringGatewayConfig(t, url)
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

		body := `{"model":"m","messages":[` +
			`{"role":"user","content":"please look this up"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"query":"` + filterEmail + `"}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"found ` + filterSSN + `"}]}]}`
		rec := serve(g, post(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		got := string(f.last(t).body)
		if strings.Contains(got, filterEmail) || strings.Contains(got, filterSSN) {
			t.Fatalf("provider received unredacted PII from a tool call or result: %s", got)
		}
	})

	t.Run("a PII-shaped string in the tools list's description is left untouched (criterion 13)", func(t *testing.T) {
		f, url := newFakeAnthropic(t, loopResponder)
		cfg := filteringGatewayConfig(t, url)
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

		body := `{"model":"m","tools":[{"name":"x","description":"contact ` + filterEmail + `"}],` +
			`"messages":[{"role":"user","content":"hi"}]}`
		rec := serve(g, post(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := string(f.last(t).body); !strings.Contains(got, filterEmail) {
			t.Fatalf("the tools list was redacted, want it untouched: %s", got)
		}
	})
}

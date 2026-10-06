package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/agentkeeper/agentkeeper/internal/config"
	"github.com/agentkeeper/agentkeeper/internal/trace"
)

// fakeSink keeps every record it accepts, as decoded JSON, since the JSON
// is the contract. fail, if set, decides per record type whether to
// refuse it.
type fakeSink struct {
	mu      sync.Mutex
	records []map[string]any
	log     *eventLog
	fail    func(recordType string) error
}

func (s *fakeSink) Write(r trace.Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("fakeSink: marshal: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("fakeSink: unmarshal: %w", err)
	}
	kind, _ := m["record"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		if err := s.fail(kind); err != nil {
			return err
		}
	}
	s.records = append(s.records, m)
	if s.log != nil {
		s.log.record("sink." + kind)
	}
	return nil
}

func (s *fakeSink) setFail(f func(string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = f
}

func (s *fakeSink) byType(kind string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, r := range s.records {
		if r["record"] == kind {
			out = append(out, r)
		}
	}
	return out
}

func (s *fakeSink) one(t *testing.T, kind string) map[string]any {
	t.Helper()
	rs := s.byType(kind)
	if len(rs) != 1 {
		t.Fatalf("sink has %d %s records, want exactly 1", len(rs), kind)
	}
	return rs[0]
}

func (s *fakeSink) waitFor(t *testing.T, kind string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.byType(kind)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d %s records, have %d", n, kind, len(s.byType(kind)))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errSinkDown = errors.New("sink unavailable")

const testTraceKeyEnv = "AK_PIPELINE_TEST_TRACE_KEY"

func newTestTracer(t *testing.T, maxReadBytes int64) *trace.Tracer {
	t.Helper()
	t.Setenv(testTraceKeyEnv, strings.Repeat("k", 32))
	tr, err := trace.NewTracer(config.TracingConfig{
		FingerprintKeyEnv: testTraceKeyEnv,
		MaxReadBytes:      maxReadBytes,
		Sink:              config.TraceSinkConfig{Type: "logging"},
	}, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("trace.NewTracer: %v", err)
	}
	return tr
}

// tracingTestStage mirrors gateway's tracingStage, which this package
// cannot import.
type tracingTestStage struct {
	tracer *trace.Tracer
	log    *eventLog
}

func (s *tracingTestStage) Name() string { return "tracing" }

func (s *tracingTestStage) Request(_ context.Context, c *Call) (Decision, error) {
	if s.log != nil {
		s.log.record("tracing.Request")
	}
	info, err := s.tracer.Request(c.Request, c.Body, c.AgentID)
	if err != nil {
		return Decision{}, err
	}
	return AllowTraced(info, s.tracer.NewResponseTap()), nil
}

type agentStage struct{ agentID string }

func (s *agentStage) Name() string { return "identification" }
func (s *agentStage) Request(context.Context, *Call) (Decision, error) {
	return AllowAs(s.agentID), nil
}

func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// errorLogNames reports whether some error-level entry carries id in its
// message or in any field value.
func errorLogNames(logs *observer.ObservedLogs, id string) bool {
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		if strings.Contains(e.Message, id) {
			return true
		}
		for _, v := range e.ContextMap() {
			if fmt.Sprint(v) == id {
				return true
			}
		}
	}
	return false
}

// --- SSE fixtures ---------------------------------------------------------------

func sseEvent(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

func toolUseEvents(index int, id, name, args string) []string {
	partial, _ := json.Marshal(args)
	return []string{
		sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)),
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, partial)),
		sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)),
	}
}

func textEvents(index int, text string) []string {
	quoted, _ := json.Marshal(text)
	return []string{
		sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)),
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, quoted)),
		sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)),
	}
}

func messageStart() string {
	return sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5-20260801","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
}

func messageEnd() []string {
	return []string{
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

// toolStream is a stream with some text and one tool call.
func toolStream() []string {
	events := []string{messageStart()}
	events = append(events, textEvents(0, "Let me look.")...)
	events = append(events, toolUseEvents(1, "toolu_01", "bash", `{"command":"ls"}`)...)
	return append(events, messageEnd()...)
}

// sseUpstream writes events one per Write, flushing after each, and stops
// at the first write error, like ReverseProxy.
func sseUpstream(events []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			if _, err := w.Write([]byte(e)); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
}

const streamedBody = `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func postBody(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
}

func str(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = obj[k]
	}
	return v
}

func stagesOf(t *testing.T, rec map[string]any) []map[string]any {
	t.Helper()
	arr, ok := rec["stages"].([]any)
	if !ok {
		t.Fatalf("stages = %#v, want an array", rec["stages"])
	}
	out := make([]map[string]any, len(arr))
	for i, s := range arr {
		out[i] = s.(map[string]any)
	}
	return out
}

func toolCallsOf(t *testing.T, rec map[string]any) []map[string]any {
	t.Helper()
	arr, ok := rec["tool_calls"].([]any)
	if !ok {
		t.Fatalf("tool_calls = %#v, want an array", rec["tool_calls"])
	}
	out := make([]map[string]any, len(arr))
	for i, s := range arr {
		out[i] = s.(map[string]any)
	}
	return out
}

func findingKinds(t *testing.T, rec map[string]any) []string {
	t.Helper()
	arr, ok := rec["findings"].([]any)
	if !ok {
		t.Fatalf("findings = %#v, want an array", rec["findings"])
	}
	var out []string
	for _, f := range arr {
		out = append(out, f.(map[string]any)["kind"].(string))
	}
	return out
}

func parseTS(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("timestamp = %#v, want a string", v)
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return ts
}

// --- Records per call ----------------------------------------------------------------

func TestRecords_ForwardedCallGetsOneRequestAndOneResponseRecord(t *testing.T) {
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})

	rec := doRequest(t, p, sseUpstream(toolStream()), postBody(streamedBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	req := sink.one(t, "request")
	resp := sink.one(t, "response")
	callID, _ := req["call_id"].(string)
	if !requestIDPattern.MatchString(callID) {
		t.Fatalf("call_id = %q, want to match %s", callID, requestIDPattern)
	}
	if resp["call_id"] != callID {
		t.Fatalf("response call_id = %v, want %q (pairs with the request record)", resp["call_id"], callID)
	}
	for _, r := range []map[string]any{req, resp} {
		if r["schema"] != "agentkeeper.trace/v1" {
			t.Fatalf("%s record schema = %v, want agentkeeper.trace/v1", r["record"], r["schema"])
		}
	}
	if got := str(req, "outcome", "result"); got != "forwarded" {
		t.Fatalf("request outcome.result = %v, want forwarded", got)
	}
	for _, k := range []string{"status", "stage", "reason"} {
		if v := str(req, "outcome", k); v != nil {
			t.Fatalf("forwarded request outcome.%s = %v, want null", k, v)
		}
	}
	if req["model"] != "claude-sonnet-5" || req["stream"] != true {
		t.Fatalf("request model/stream = %v/%v, want claude-sonnet-5/true", req["model"], req["stream"])
	}
	if got := str(resp, "outcome", "result"); got != "delivered" {
		t.Fatalf("response outcome.result = %v, want delivered", got)
	}
	if str(resp, "outcome", "upstream_status") != float64(200) || str(resp, "outcome", "status") != float64(200) {
		t.Fatalf("response outcome = %v, want upstream_status 200 and status 200", resp["outcome"])
	}
	if resp["streamed"] != true {
		t.Fatalf("response streamed = %v, want true", resp["streamed"])
	}
	tcs := toolCallsOf(t, resp)
	if len(tcs) != 1 || tcs[0]["tool_call_id"] != "toolu_01" || tcs[0]["outcome"] != "delivered" {
		t.Fatalf("tool_calls = %v, want toolu_01 delivered", tcs)
	}

	received := parseTS(t, req["received_at"])
	started := parseTS(t, resp["started_at"])
	if started.Before(received) {
		t.Fatalf("response started_at %v is before request received_at %v", started, received)
	}
	fb, ok := resp["first_byte_us"].(float64)
	if !ok || fb < 0 {
		t.Fatalf("first_byte_us = %#v, want a non-negative number", resp["first_byte_us"])
	}
	if d, _ := resp["duration_us"].(float64); d < fb {
		t.Fatalf("duration_us = %v, want >= first_byte_us %v", d, fb)
	}
}

func TestRecords_EveryFieldPresent(t *testing.T) {
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	doRequest(t, p, sseUpstream(toolStream()), postBody(streamedBody))

	req, resp := sink.one(t, "request"), sink.one(t, "response")
	wantReq := []string{"schema", "record", "call_id", "agent_id", "session", "received_at", "duration_us",
		"model", "stream", "size_bytes", "outcome", "stages", "tool_results", "findings"}
	wantResp := []string{"schema", "record", "call_id", "agent_id", "session", "started_at", "first_byte_us",
		"duration_us", "outcome", "stages", "streamed", "response_id", "response_model", "stop_reason", "tokens",
		"tool_calls", "findings"}
	if got := slices.Sorted(maps.Keys(req)); !slices.Equal(got, slices.Sorted(slices.Values(wantReq))) {
		t.Fatalf("request record keys = %v, want %v", got, wantReq)
	}
	if got := slices.Sorted(maps.Keys(resp)); !slices.Equal(got, slices.Sorted(slices.Values(wantResp))) {
		t.Fatalf("response record keys = %v, want %v", got, wantResp)
	}
	for _, k := range []string{"tool_results", "findings"} {
		if _, ok := req[k].([]any); !ok {
			t.Fatalf("request %s = %#v, want [] rather than null", k, req[k])
		}
	}
	if _, ok := resp["findings"].([]any); !ok {
		t.Fatalf("response findings = %#v, want [] rather than null", resp["findings"])
	}
}

func TestRecords_AgentAndSessionAreOnBothRecords(t *testing.T) {
	t.Run("with an identified agent", func(t *testing.T) {
		sink := &fakeSink{}
		p := New(zaptest.NewLogger(t), 1<<20, sink, &agentStage{agentID: "agent-x"}, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
		doRequest(t, p, sseUpstream(toolStream()), postBody(streamedBody))

		req, resp := sink.one(t, "request"), sink.one(t, "response")
		if req["agent_id"] != "agent-x" || resp["agent_id"] != "agent-x" {
			t.Fatalf("agent_id = %v / %v, want agent-x on both records", req["agent_id"], resp["agent_id"])
		}
		if fmt.Sprint(req["session"]) != fmt.Sprint(resp["session"]) {
			t.Fatalf("session differs between records: %v vs %v", req["session"], resp["session"])
		}
		if str(req, "session", "status") != "absent" {
			t.Fatalf("session.status = %v, want absent (no sources configured)", str(req, "session", "status"))
		}
	})

	t.Run("without identification", func(t *testing.T) {
		sink := &fakeSink{}
		p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
		doRequest(t, p, sseUpstream(toolStream()), postBody(streamedBody))

		req, resp := sink.one(t, "request"), sink.one(t, "response")
		if v, ok := req["agent_id"]; !ok || v != nil {
			t.Fatalf("request agent_id = %#v, want null", v)
		}
		if v, ok := resp["agent_id"]; !ok || v != nil {
			t.Fatalf("response agent_id = %#v, want null", v)
		}
	})
}

// --- Stage results --------------------------------------------------------------------

func TestRecords_RequestStageDecisionsAndOutcome(t *testing.T) {
	tests := []struct {
		name          string
		stages        func() []Stage
		wantDecisions []string
		wantResult    string
		wantStatus    int
		wantStage     string
		wantReason    string
	}{
		{
			name: "all allowed: forwarded",
			stages: func() []Stage {
				return []Stage{allowStage("s1", nil), allowStage("s2", nil), allowStage("s3", nil)}
			},
			wantDecisions: []string{"allowed", "allowed", "allowed"},
			wantResult:    "forwarded",
		},
		{
			name: "second of three denies: third is not_run",
			stages: func() []Stage {
				return []Stage{allowStage("s1", nil), denyStage("s2", http.StatusForbidden, "no", "policy violated", nil), allowStage("s3", nil)}
			},
			wantDecisions: []string{"allowed", "denied", "not_run"},
			wantResult:    "denied",
			wantStatus:    http.StatusForbidden,
			wantStage:     "s2",
			wantReason:    "policy violated",
		},
		{
			name: "second of three errs: failed, third is not_run",
			stages: func() []Stage {
				return []Stage{allowStage("s1", nil), errStage("s2", errors.New("backend down"), nil), allowStage("s3", nil)}
			},
			wantDecisions: []string{"allowed", "failed", "not_run"},
			wantResult:    "failed",
			wantStatus:    http.StatusServiceUnavailable,
			wantStage:     "s2",
		},
		{
			name: "first of three panics: failed, the rest not_run",
			stages: func() []Stage {
				return []Stage{&fakeObserverStage{fakeStage: &fakeStage{name: "s1", panicRequest: true}}, allowStage("s2", nil), allowStage("s3", nil)}
			},
			wantDecisions: []string{"failed", "not_run", "not_run"},
			wantResult:    "failed",
			wantStatus:    http.StatusServiceUnavailable,
			wantStage:     "s1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			p := New(zaptest.NewLogger(t), 1<<20, sink, tt.stages()...)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			rec := doRequest(t, p, next, postBody(`{"model":"m"}`))

			req := sink.one(t, "request")
			stages := stagesOf(t, req)
			if len(stages) != len(tt.wantDecisions) {
				t.Fatalf("stages = %v, want %d entries", stages, len(tt.wantDecisions))
			}
			for i, s := range stages {
				if want := fmt.Sprintf("s%d", i+1); s["name"] != want {
					t.Fatalf("stages[%d].name = %v, want %s (pipeline order)", i, s["name"], want)
				}
				if s["decision"] != tt.wantDecisions[i] {
					t.Fatalf("stages[%d].decision = %v, want %s", i, s["decision"], tt.wantDecisions[i])
				}
				if tt.wantDecisions[i] == "not_run" {
					if s["started_at"] != nil || s["duration_us"] != nil {
						t.Fatalf("not_run stages[%d] has timings %v/%v, want null", i, s["started_at"], s["duration_us"])
					}
					continue
				}
				parseTS(t, s["started_at"])
				if d, ok := s["duration_us"].(float64); !ok || d < 0 {
					t.Fatalf("stages[%d].duration_us = %#v, want a non-negative number", i, s["duration_us"])
				}
			}

			if got := str(req, "outcome", "result"); got != tt.wantResult {
				t.Fatalf("outcome.result = %v, want %s", got, tt.wantResult)
			}
			if tt.wantResult == "forwarded" {
				sink.one(t, "response")
				return
			}
			if got := str(req, "outcome", "status"); got != float64(tt.wantStatus) || rec.Code != tt.wantStatus {
				t.Fatalf("outcome.status = %v, caller got %d; want both %d", got, rec.Code, tt.wantStatus)
			}
			if got := str(req, "outcome", "stage"); got != tt.wantStage {
				t.Fatalf("outcome.stage = %v, want %s", got, tt.wantStage)
			}
			if tt.wantReason != "" && str(req, "outcome", "reason") != tt.wantReason {
				t.Fatalf("outcome.reason = %v, want %q", str(req, "outcome", "reason"), tt.wantReason)
			}
			if str(req, "outcome", "reason") == nil {
				t.Fatal("outcome.reason = null, want the audit-only reason")
			}
			if n := len(sink.byType("response")); n != 0 {
				t.Fatalf("refused call has %d response records, want 0", n)
			}
		})
	}
}

func TestRecords_CallRefusedBeforeTracingRanHasTracingDefaults(t *testing.T) {
	tests := []struct {
		name    string
		maxBody int64
		req     *http.Request
		stages  func(tr *trace.Tracer) []Stage
		status  int
	}{
		{
			name:    "body over max_body_bytes (413)",
			maxBody: 10,
			req:     postBody(streamedBody),
			stages:  func(tr *trace.Tracer) []Stage { return []Stage{&tracingTestStage{tracer: tr}} },
			status:  http.StatusRequestEntityTooLarge,
		},
		{
			name:    "body read error (400)",
			maxBody: 1 << 20,
			req:     httptest.NewRequest(http.MethodPost, "/v1/messages", errorReader{}),
			stages:  func(tr *trace.Tracer) []Stage { return []Stage{&tracingTestStage{tracer: tr}} },
			status:  http.StatusBadRequest,
		},
		{
			name:    "denied by a stage before tracing",
			maxBody: 1 << 20,
			req:     postBody(streamedBody),
			stages: func(tr *trace.Tracer) []Stage {
				return []Stage{denyStage("identification", http.StatusUnauthorized, "no", "no credential", nil), &tracingTestStage{tracer: tr}}
			},
			status: http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			p := New(zaptest.NewLogger(t), tt.maxBody, sink, tt.stages(newTestTracer(t, 1<<20))...)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("upstream must not be called") })
			rec := doRequest(t, p, next, tt.req)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}

			req := sink.one(t, "request")
			if str(req, "session", "status") != "absent" {
				t.Fatalf("session.status = %v, want absent", str(req, "session", "status"))
			}
			if v, ok := req["model"]; !ok || v != nil {
				t.Fatalf("model = %#v, want null", v)
			}
			if trs, ok := req["tool_results"].([]any); !ok || len(trs) != 0 {
				t.Fatalf("tool_results = %#v, want []", req["tool_results"])
			}
			if got := str(req, "outcome", "result"); got != "denied" {
				t.Fatalf("outcome.result = %v, want denied", got)
			}
			if n := len(sink.byType("response")); n != 0 {
				t.Fatalf("refused call has %d response records, want 0", n)
			}
		})
	}
}

func TestRecords_RequestSizeBytes(t *testing.T) {
	tests := []struct {
		name     string
		maxBody  int64
		req      *http.Request
		wantSize any
	}{
		{"accepted body gives its exact length", 1 << 20, postBody(streamedBody), float64(len(streamedBody))},
		{"413 gives null", 10, postBody(streamedBody), nil},
		{"400 gives null", 1 << 20, httptest.NewRequest(http.MethodPost, "/", errorReader{}), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			p := New(zaptest.NewLogger(t), tt.maxBody, sink, allowStage("s1", nil))
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			doRequest(t, p, next, tt.req)

			req := sink.one(t, "request")
			if v, ok := req["size_bytes"]; !ok || v != tt.wantSize {
				t.Fatalf("size_bytes = %#v, want %#v", v, tt.wantSize)
			}
		})
	}
}

func TestRecords_TracingReadFailureFailsClosed(t *testing.T) {
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	var upstreamCalled atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstreamCalled.Store(true) })

	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`
	rec := doRequest(t, p, next, postBody(body))

	if upstreamCalled.Load() {
		t.Fatal("upstream was called for a request tracing could not read")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "agentkeeper_unavailable" || env.Error.Stage != "tracing" {
		t.Fatalf("error = %+v, want agentkeeper_unavailable from stage tracing", env.Error)
	}
	req := sink.one(t, "request")
	if str(req, "outcome", "result") != "failed" || str(req, "outcome", "stage") != "tracing" {
		t.Fatalf("outcome = %v, want failed at stage tracing", req["outcome"])
	}
}

// --- Order ------------------------------------------------------------------------------

func TestRecords_RequestRecordBeforeUpstreamResponseRecordAfterObservers(t *testing.T) {
	log := &eventLog{}
	sink := &fakeSink{log: log}
	p := New(zaptest.NewLogger(t), 1<<20, sink,
		allowStage("s1", log),
		&tracingTestStage{tracer: newTestTracer(t, 1<<20), log: log},
		allowStage("s3", log),
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record("upstream")
		sseUpstream(toolStream()).ServeHTTP(w, r)
	})
	doRequest(t, p, next, postBody(streamedBody))

	want := []string{"s1.Request", "tracing.Request", "s3.Request", "sink.request", "upstream", "s3.Response", "s1.Response", "sink.response"}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

func TestRecords_ResponseStagesListTracingFirstThenObserversInRunOrder(t *testing.T) {
	sink := &fakeSink{}
	plain := &fakeStage{name: "plain", decision: Allow()} // not an observer: not listed
	p := New(zaptest.NewLogger(t), 1<<20, sink,
		allowStage("a", nil),
		&tracingTestStage{tracer: newTestTracer(t, 1<<20)},
		plain,
		allowStage("b", nil),
	)
	doRequest(t, p, sseUpstream(toolStream()), postBody(streamedBody))

	stages := stagesOf(t, sink.one(t, "response"))
	var names []string
	for _, s := range stages {
		names = append(names, s["name"].(string))
		if s["observation"] != "completed" {
			t.Fatalf("stage %v observation = %v, want completed", s["name"], s["observation"])
		}
		parseTS(t, s["started_at"])
		if d, ok := s["duration_us"].(float64); !ok || d < 0 {
			t.Fatalf("stage %v duration_us = %#v, want a non-negative number", s["name"], s["duration_us"])
		}
	}
	if want := []string{"tracing", "b", "a"}; !slices.Equal(names, want) {
		t.Fatalf("response stages = %v, want %v", names, want)
	}
}

func TestRecords_ObserverPanicIsObservationFailedAndStillDelivered(t *testing.T) {
	sink := &fakeSink{}
	panicky := &fakeObserverStage{fakeStage: &fakeStage{name: "audit", decision: Allow()}, panicResponse: true}
	p := New(zaptest.NewLogger(t), 1<<20, sink, panicky, allowStage("other", nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	})
	rec := doRequest(t, p, next, postBody(`{}`))

	if rec.Code != http.StatusOK || rec.Body.String() != "upstream-ok" {
		t.Fatalf("caller got %d %q, want 200 upstream-ok", rec.Code, rec.Body.String())
	}
	resp := sink.one(t, "response")
	if str(resp, "outcome", "result") != "delivered" {
		t.Fatalf("outcome.result = %v, want delivered", str(resp, "outcome", "result"))
	}
	got := map[string]any{}
	for _, s := range stagesOf(t, resp) {
		got[s["name"].(string)] = s["observation"]
	}
	if got["audit"] != "failed" || got["other"] != "completed" {
		t.Fatalf("observations = %v, want audit failed and other completed", got)
	}
}

// --- Sink failures -------------------------------------------------------------------------

func TestRecords_SinkRefusingRequestRecordDeniesAndRecovers(t *testing.T) {
	logger, logs := observedLogger()
	sink := &fakeSink{fail: func(string) error { return errSinkDown }}
	p := New(logger, 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	var upstreamCalls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		sseUpstream(toolStream()).ServeHTTP(w, r)
	})

	rec := doRequest(t, p, next, postBody(streamedBody))
	if upstreamCalls.Load() != 0 {
		t.Fatal("upstream was called although the request record could not be written")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "agentkeeper_unavailable" || env.Error.Stage != "tracing" || env.Error.Message != "governance unavailable" {
		t.Fatalf("error = %+v, want agentkeeper_unavailable / tracing / governance unavailable", env.Error)
	}
	callID := rec.Header().Get("X-Agentkeeper-Request-Id")
	if !errorLogNames(logs, callID) {
		t.Fatalf("no error-level log names call %s: %v", callID, logs.All())
	}

	sink.setFail(nil)
	rec2 := doRequest(t, p, next, postBody(streamedBody))
	if rec2.Code != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("after the sink recovered: status %d, upstream calls %d; want 200 and 1", rec2.Code, upstreamCalls.Load())
	}
	sink.one(t, "request")
	sink.one(t, "response")
}

func TestRecords_SinkRefusingRequestRecordOfARefusedCallKeepsTheRefusal(t *testing.T) {
	logger, logs := observedLogger()
	sink := &fakeSink{fail: func(string) error { return errSinkDown }}
	p := New(logger, 1<<20, sink, denyStage("identification", http.StatusUnauthorized, "could not identify caller", "no credential", nil))
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("upstream must not be called") })

	rec := doRequest(t, p, next, postBody(`{}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: the original refusal stands", rec.Code)
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Type != "agentkeeper_denied" || env.Error.Stage != "identification" {
		t.Fatalf("error = %+v, want agentkeeper_denied from identification", env.Error)
	}
	if callID := rec.Header().Get("X-Agentkeeper-Request-Id"); !errorLogNames(logs, callID) {
		t.Fatalf("no error-level log names call %s: %v", callID, logs.All())
	}
}

func TestRecords_SinkRefusingResponseRecordLogsItAndNextCallIsDenied(t *testing.T) {
	logger, logs := observedLogger()
	sink := &fakeSink{}
	// Accept the first request record, then stop accepting anything.
	sink.fail = func(kind string) error {
		if kind == "request" && len(sink.records) == 0 {
			return nil
		}
		return errSinkDown
	}
	p := New(logger, 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	events := toolStream()
	upstream := sseUpstream(events)

	rec := doRequest(t, p, upstream, postBody(streamedBody))
	if rec.Code != http.StatusOK || rec.Body.String() != strings.Join(events, "") {
		t.Fatalf("caller got %d with %d bytes, want 200 and the full response", rec.Code, rec.Body.Len())
	}
	callID, _ := sink.one(t, "request")["call_id"].(string)
	if !errorLogNames(logs, callID) {
		t.Fatalf("lost response record for %s is not named in an error-level log: %v", callID, logs.All())
	}

	rec2 := doRequest(t, p, upstream, postBody(streamedBody))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("next call status = %d, want 503 while the sink refuses records", rec2.Code)
	}
}

// --- The tap on the relay -------------------------------------------------------------------

func TestRecords_StreamingWithTracingIsByteIdenticalAndUnbuffered(t *testing.T) {
	events := toolStream()
	release := make([]chan struct{}, len(events))
	for i := range release {
		release[i] = make(chan struct{})
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter passed to next must support http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, e := range events {
			_, _ = w.Write([]byte(e))
			flusher.Flush()
			<-release[i]
		}
	})

	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	srv := httptest.NewServer(p.Handler(next))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(streamedBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for i, want := range events {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(reader, got); err != nil {
			t.Fatalf("reading event %d: %v", i, err)
		}
		if string(got) != want {
			t.Fatalf("event %d = %q, want %q", i, got, want)
		}
		if n := len(sink.byType("response")); n != 0 {
			t.Fatalf("response record written before the stream ended (after event %d)", i)
		}
		close(release[i])
	}
	if rest, _ := io.ReadAll(reader); len(rest) != 0 {
		t.Fatalf("caller received %d unexpected extra bytes: %q", len(rest), rest)
	}

	sink.waitFor(t, "response", 1)
	tcs := toolCallsOf(t, sink.one(t, "response"))
	if len(tcs) != 1 || tcs[0]["tool_call_id"] != "toolu_01" {
		t.Fatalf("tool_calls = %v, want toolu_01", tcs)
	}
}

func TestRecords_ToolCallPastMaxBodyBytesIsRecorded(t *testing.T) {
	const maxBody = 64
	events := []string{messageStart()}
	events = append(events, textEvents(0, strings.Repeat("long text ", 200))...)
	events = append(events, toolUseEvents(1, "toolu_late", "bash", `{"command":"ls"}`)...)
	events = append(events, messageEnd()...)

	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), maxBody, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	body := `{"model":"m","stream":true,"messages":[]}`
	if len(body) > maxBody {
		t.Fatalf("test setup: request body %d bytes exceeds max_body_bytes", len(body))
	}
	rec := doRequest(t, p, sseUpstream(events), postBody(body))

	if rec.Body.String() != strings.Join(events, "") {
		t.Fatal("caller did not receive the full response")
	}
	resp := sink.one(t, "response")
	tcs := toolCallsOf(t, resp)
	if len(tcs) != 1 || tcs[0]["tool_call_id"] != "toolu_late" || tcs[0]["recorded"] != true {
		t.Fatalf("tool_calls = %v, want toolu_late recorded in full", tcs)
	}
	if kinds := findingKinds(t, resp); !slices.Contains(kinds, "response_truncated") {
		t.Fatalf("findings = %v, want response_truncated", kinds)
	}
}

func TestRecords_NonStreamedBodyOverReadLimitIsUnreadButCompleted(t *testing.T) {
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 256)})
	upstreamBody := `{"id":"msg_01","model":"m-1","content":[{"type":"text","text":"` + strings.Repeat("x", 1000) +
		`"},{"type":"tool_use","id":"toolu_01","name":"bash","input":{"command":"ls"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	})
	rec := doRequest(t, p, next, postBody(`{"model":"m","messages":[]}`))

	if rec.Body.String() != upstreamBody {
		t.Fatal("caller did not receive the response unchanged")
	}
	resp := sink.one(t, "response")
	if tcs := toolCallsOf(t, resp); len(tcs) != 0 {
		t.Fatalf("tool_calls = %v, want []", tcs)
	}
	if kinds := findingKinds(t, resp); !slices.Contains(kinds, "tool_calls_unread") {
		t.Fatalf("findings = %v, want tool_calls_unread", kinds)
	}
	for _, s := range stagesOf(t, resp) {
		if s["name"] == "tracing" && s["observation"] != "completed" {
			t.Fatalf("tracing observation = %v, want completed: the read limit is not a failure", s["observation"])
		}
	}
}

func TestRecords_UnansweredUpstream(t *testing.T) {
	tests := []struct {
		name               string
		signal             bool
		wantResult         string
		wantUpstreamStatus any
	}{
		{"UpstreamFailed called: unanswered", true, "unanswered", nil},
		{"provider itself answered 502: delivered", false, "delivered", float64(http.StatusBadGateway)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				f, ok := w.(interface{ UpstreamFailed() })
				if !ok {
					t.Error("ResponseWriter passed to next does not implement UpstreamFailed()")
				} else if tt.signal {
					f.UpstreamFailed()
				}
				w.WriteHeader(http.StatusBadGateway)
			})
			rec := doRequest(t, p, next, postBody(streamedBody))

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", rec.Code)
			}
			resp := sink.one(t, "response")
			if got := str(resp, "outcome", "result"); got != tt.wantResult {
				t.Fatalf("outcome.result = %v, want %s", got, tt.wantResult)
			}
			if v := str(resp, "outcome", "upstream_status"); v != tt.wantUpstreamStatus {
				t.Fatalf("outcome.upstream_status = %#v, want %#v", v, tt.wantUpstreamStatus)
			}
			if v := str(resp, "outcome", "status"); v != float64(http.StatusBadGateway) {
				t.Fatalf("outcome.status = %v, want 502", v)
			}
		})
	}
}

// failingWriter relays the first ok writes, then fails every write as a
// disconnected client would.
type failingWriter struct {
	*httptest.ResponseRecorder
	ok     int
	writes int
}

func (w *failingWriter) Write(b []byte) (int, error) {
	if w.writes >= w.ok {
		return 0, errors.New("client disconnected")
	}
	w.writes++
	return w.ResponseRecorder.Write(b)
}

func TestRecords_ClientDisconnectMarksUnrelayedToolCallNotDelivered(t *testing.T) {
	events := []string{messageStart()}
	events = append(events, toolUseEvents(0, "toolu_first", "bash", `{"command":"ls"}`)...)
	events = append(events, toolUseEvents(1, "toolu_second", "bash", `{"command":"pwd"}`)...)
	events = append(events, messageEnd()...)

	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	// message_start, the first tool call, and the second's start get through;
	// the second's delta is the first write to fail.
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), ok: 5}
	p.Handler(sseUpstream(events)).ServeHTTP(w, postBody(streamedBody))

	tcs := toolCallsOf(t, sink.one(t, "response"))
	if len(tcs) != 2 {
		t.Fatalf("tool_calls = %v, want 2", tcs)
	}
	if tcs[0]["tool_call_id"] != "toolu_first" || tcs[0]["outcome"] != "delivered" {
		t.Fatalf("tool_calls[0] = %v, want toolu_first delivered", tcs[0])
	}
	if tcs[1]["tool_call_id"] != "toolu_second" || tcs[1]["outcome"] != "not_delivered" {
		t.Fatalf("tool_calls[1] = %v, want toolu_second not_delivered", tcs[1])
	}
}

func TestRecords_NilSinkWritesNoRecordsAndStillRelays(t *testing.T) {
	events := toolStream()
	p := New(zaptest.NewLogger(t), 1<<20, nil, &tracingTestStage{tracer: newTestTracer(t, 1<<20)})
	rec := doRequest(t, p, sseUpstream(events), postBody(streamedBody))

	if rec.Code != http.StatusOK || rec.Body.String() != strings.Join(events, "") {
		t.Fatalf("caller got %d with %d bytes, want 200 and the full stream", rec.Code, rec.Body.Len())
	}
}

// --- stage detail (specs/0003, as amended by 0004) -----------------------------

// detailStage is a stage that attaches a detail to its decision.
type detailStage struct {
	name   string
	deny   bool
	detail trace.StageDetail
}

func (s *detailStage) Name() string { return s.name }
func (s *detailStage) Request(context.Context, *Call) (Decision, error) {
	d := Allow()
	if s.deny {
		d = Deny(http.StatusForbidden, "request denied by policy", "rules="+strings.Join(s.detail.Rules, ","))
	}
	return d.WithDetail(s.detail), nil
}

func TestRequestRecord_StageDetail(t *testing.T) {
	tests := []struct {
		name       string
		stage      *detailStage
		wantStatus int
		wantDec    string
		wantRules  []any
	}{
		{
			name:       "detail reaches the record on a denial",
			stage:      &detailStage{name: "policies", deny: true, detail: trace.StageDetail{Revision: "abc123def456", Rules: []string{"a", "b"}}},
			wantStatus: http.StatusForbidden,
			wantDec:    "denied",
			wantRules:  []any{"a", "b"},
		},
		{
			name:       "detail reaches the record on an allow, with empty rules encoded as []",
			stage:      &detailStage{name: "policies", detail: trace.StageDetail{Revision: "abc123def456", Rules: []string{}}},
			wantStatus: http.StatusOK,
			wantDec:    "allowed",
			wantRules:  []any{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &fakeSink{}
			p := New(zaptest.NewLogger(t), 1<<20, sink, tt.stage)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			stages := stagesOf(t, sink.one(t, "request"))
			if len(stages) != 1 {
				t.Fatalf("%d stage entries, want 1", len(stages))
			}
			st := stages[0]
			if st["decision"] != tt.wantDec {
				t.Fatalf("decision = %v, want %s", st["decision"], tt.wantDec)
			}
			detail, ok := st["detail"].(map[string]any)
			if !ok {
				t.Fatalf("detail = %#v, want an object", st["detail"])
			}
			if detail["revision"] != "abc123def456" {
				t.Fatalf("detail.revision = %v, want abc123def456", detail["revision"])
			}
			rules, ok := detail["rules"].([]any)
			if !ok {
				t.Fatalf("detail.rules = %#v, want an array (never null)", detail["rules"])
			}
			if len(rules) != len(tt.wantRules) {
				t.Fatalf("detail.rules = %v, want %v", rules, tt.wantRules)
			}
			for i := range rules {
				if rules[i] != tt.wantRules[i] {
					t.Fatalf("detail.rules = %v, want %v", rules, tt.wantRules)
				}
			}
		})
	}
}

func TestRequestRecord_StageWithoutDetailEncodesNull(t *testing.T) {
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, allowStage("identification", nil), &detailStage{name: "policies", detail: trace.StageDetail{Revision: "r", Rules: []string{}}}, denyStage("later", http.StatusForbidden, "no", "no", nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	stages := stagesOf(t, sink.one(t, "request"))
	if len(stages) != 3 {
		t.Fatalf("%d stage entries, want 3", len(stages))
	}
	for _, i := range []int{0, 2} {
		v, present := stages[i]["detail"]
		if !present || v != nil {
			t.Fatalf("stage %v: detail = %#v (present=%v), want an explicit null", stages[i]["name"], v, present)
		}
	}
	if stages[1]["detail"] == nil {
		t.Fatal("the stage that attached a detail lost it")
	}
}

func TestRequestRecord_NotRunStageHasNullDetail(t *testing.T) {
	sink := &fakeSink{}
	p := New(zaptest.NewLogger(t), 1<<20, sink, denyStage("identification", http.StatusUnauthorized, "no", "no", nil), &detailStage{name: "policies", detail: trace.StageDetail{Revision: "r"}})
	doRequest(t, p, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))

	stages := stagesOf(t, sink.one(t, "request"))
	if stages[1]["decision"] != "not_run" {
		t.Fatalf("policies decision = %v, want not_run", stages[1]["decision"])
	}
	if v, present := stages[1]["detail"]; !present || v != nil {
		t.Fatalf("not_run stage detail = %#v (present=%v), want an explicit null", v, present)
	}
}

func TestDecision_WithDetailStillDeniesAndStillAllows(t *testing.T) {
	var nextCalled atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nextCalled.Store(true) })

	p := New(zaptest.NewLogger(t), 1<<20, nil, &detailStage{name: "policies", deny: true, detail: trace.StageDetail{Revision: "r", Rules: []string{"x"}}})
	if rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil)); rec.Code != http.StatusForbidden || nextCalled.Load() {
		t.Fatalf("a denial with detail: status=%d forwarded=%v, want 403 and not forwarded", rec.Code, nextCalled.Load())
	}
	p = New(zaptest.NewLogger(t), 1<<20, nil, &detailStage{name: "policies", detail: trace.StageDetail{Revision: "r", Rules: []string{}}})
	if rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil)); rec.Code != http.StatusOK || !nextCalled.Load() {
		t.Fatalf("an allow with detail: status=%d forwarded=%v, want 200 and forwarded", rec.Code, nextCalled.Load())
	}
}

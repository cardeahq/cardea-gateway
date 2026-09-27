package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/agentkeeper/agentkeeper/internal/config"
	"github.com/agentkeeper/agentkeeper/internal/logging"
	"github.com/agentkeeper/agentkeeper/internal/pipeline"
	"github.com/agentkeeper/agentkeeper/internal/provider"
)

var _ pipeline.Stage = (*tracingStage)(nil)

const (
	gwTraceKeyEnv     = "AK_GW_TRACE_TEST_KEY"
	gwAnthropicKeyEnv = "AK_GW_TRACE_TEST_ANTHROPIC_KEY"
	gwAgentXKeyEnv    = "AK_GW_TRACE_TEST_AGENT_X_KEY"
	gwAgentYKeyEnv    = "AK_GW_TRACE_TEST_AGENT_Y_KEY"
	agentXKey         = "agent-x-opaque-api-key"
	agentYKey         = "agent-y-opaque-api-key"
	sessionUUID       = "8d0c7a3e-5b1f-4c2a-9e6d-1f2a3b4c5d6e"
)

// --- Fake Anthropic upstream ------------------------------------------------------

type capturedRequest struct {
	header http.Header
	body   []byte
}

type fakeAnthropic struct {
	mu       sync.Mutex
	requests []capturedRequest
	respond  func(w http.ResponseWriter, body []byte)
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, capturedRequest{header: r.Header.Clone(), body: body})
	f.mu.Unlock()
	f.respond(w, body)
}

func (f *fakeAnthropic) last(t *testing.T) capturedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the provider received no request")
	}
	return f.requests[len(f.requests)-1]
}

func newFakeAnthropic(t *testing.T, respond func(w http.ResponseWriter, body []byte)) (*fakeAnthropic, string) {
	t.Helper()
	f := &fakeAnthropic{respond: respond}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func sseEvent(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

func messageStartEvent(id, model string) string {
	return sseEvent("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":1}}}`, id, model))
}

func toolUseEvents(index int, id, name string, partials ...string) []string {
	events := []string{sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name))}
	for _, p := range partials {
		q, _ := json.Marshal(p)
		events = append(events, sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`, index, q)))
	}
	return append(events, sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)))
}

func textEvents(index int, text string) []string {
	q, _ := json.Marshal(text)
	return []string{
		sseEvent("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)),
		sseEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, q)),
		sseEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)),
	}
}

func messageEndEvents(stopReason string) []string {
	return []string{
		sseEvent("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":42}}`, stopReason)),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

func toolCallStream(msgID, toolID, name string, partials ...string) []string {
	events := []string{messageStartEvent(msgID, "claude-sonnet-5-20260801")}
	events = append(events, textEvents(0, "Working on it.")...)
	events = append(events, toolUseEvents(1, toolID, name, partials...)...)
	return append(events, messageEndEvents("tool_use")...)
}

func endTurnStream(msgID string) []string {
	events := []string{messageStartEvent(msgID, "claude-sonnet-5-20260801")}
	events = append(events, textEvents(0, "All done.")...)
	return append(events, messageEndEvents("end_turn")...)
}

func writeStream(w http.ResponseWriter, events []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, _ = w.Write([]byte(e))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func streamResponder(events []string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) { writeStream(w, events) }
}

// --- Gateway construction -----------------------------------------------------------

func tracingGatewayConfig(t *testing.T, upstreamURL string) config.Config {
	t.Helper()
	t.Setenv(gwTraceKeyEnv, strings.Repeat("g", 32))
	return config.Config{
		Server:   config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Provider: config.ProviderConfig{Name: "anthropic", BaseURL: upstreamURL, APIKeyEnv: gwAnthropicKeyEnv},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{TokenCounting: true, Tracing: true},
		},
		Tracing: config.TracingConfig{
			FingerprintKeyEnv: gwTraceKeyEnv,
			MaxReadBytes:      16 << 20,
			Sink:              config.TraceSinkConfig{Type: "logging", Path: filepath.Join(t.TempDir(), "records.jsonl")},
		},
	}
}

func withAgents(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	t.Setenv(gwAgentXKeyEnv, agentXKey)
	t.Setenv(gwAgentYKeyEnv, agentYKey)
	cfg.Pipeline.Stages.Identification = true
	cfg.Agents = []config.AgentConfig{
		{AgentID: "agent-x", APIKeyEnv: gwAgentXKeyEnv},
		{AgentID: "agent-y", APIKeyEnv: gwAgentYKeyEnv},
	}
	return cfg
}

func newTracingGateway(t *testing.T, cfg config.Config, logger *zap.Logger) *Gateway {
	t.Helper()
	t.Setenv(gwAnthropicKeyEnv, "sk-test-upstream")
	prov, err := provider.NewAnthropicProvider(cfg.Provider.BaseURL, gwAnthropicKeyEnv)
	if err != nil {
		t.Fatalf("NewAnthropicProvider: %v", err)
	}
	g, err := New(cfg, logger, prov)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return g
}

func serve(g *Gateway, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(rec, req)
	return rec
}

func post(body string, headers ...string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

func readRecordLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading records: %v", err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range readRecordLines(t, path) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("record line is not JSON: %v: %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// callRecords returns the request and response records of the most recent
// call; resp is nil when the call has no response record.
func callRecords(t *testing.T, path string) (req, resp map[string]any) {
	t.Helper()
	records := readRecords(t, path)
	for i := len(records) - 1; i >= 0; i-- {
		if records[i]["record"] == "request" {
			req = records[i]
			break
		}
	}
	if req == nil {
		t.Fatal("no request record written")
	}
	for _, r := range records {
		if r["record"] == "response" && r["call_id"] == req["call_id"] {
			if resp != nil {
				t.Fatalf("call %v has more than one response record", req["call_id"])
			}
			resp = r
		}
	}
	return req, resp
}

func field(m map[string]any, keys ...string) any {
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

func list(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	arr, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want an array", key, m[key])
	}
	out := make([]map[string]any, len(arr))
	for i, v := range arr {
		out[i] = v.(map[string]any)
	}
	return out
}

func tsField(t *testing.T, m map[string]any, key string) time.Time {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want a timestamp string", key, m[key])
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s = %q: %v", key, s, err)
	}
	return ts
}

func logText(logs *observer.ObservedLogs) string {
	var b strings.Builder
	for _, e := range logs.All() {
		fmt.Fprintf(&b, "%s %v\n", e.Message, e.ContextMap())
	}
	return b.String()
}

func observed() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

// Request bodies for a three-call tool loop: A asks, B returns toolu_A's
// result, C returns toolu_B's result.
const (
	bodyA = `{"model":"claude-sonnet-5","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"list the files"}]}`
	bodyB = `{"model":"claude-sonnet-5","stream":true,"max_tokens":1024,"messages":[` +
		`{"role":"user","content":"list the files"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_A","content":"file1\nfile2"}]}]}`
	bodyC = `{"model":"claude-sonnet-5","stream":true,"max_tokens":1024,"messages":[` +
		`{"role":"user","content":"list the files"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A","name":"bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_A","content":"file1\nfile2"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_B","name":"read_file","input":{"path":"file1"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_B","content":"hello"}]}]}`
)

// loopResponder answers A, B and C deterministically.
func loopResponder(w http.ResponseWriter, body []byte) {
	switch {
	case bytes.Contains(body, []byte(`"tool_use_id":"toolu_B"`)):
		writeStream(w, endTurnStream("msg_C"))
	case bytes.Contains(body, []byte(`"tool_use_id":"toolu_A"`)):
		writeStream(w, toolCallStream("msg_B", "toolu_B", "read_file", `{"path": "file1"}`))
	default:
		writeStream(w, toolCallStream("msg_A", "toolu_A", "bash", `{"command": "ls"}`))
	}
}

// --- buildStages and gateway.New -----------------------------------------------------

func TestBuildStages_TracingSitsBetweenIdentificationAndTokenCounting(t *testing.T) {
	cfg := withAgents(t, tracingGatewayConfig(t, "http://127.0.0.1:1"))
	stages, sink, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if sink == nil {
		t.Fatal("buildStages returned a nil sink with tracing enabled")
	}
	t.Cleanup(func() { _ = sink.Close() })

	var names []string
	for _, s := range stages {
		names = append(names, s.Name())
	}
	if want := []string{"identification", "tracing", "token_counting"}; !slices.Equal(names, want) {
		t.Fatalf("stage order = %v, want %v", names, want)
	}
}

func TestBuildStages_TracingOffGivesNoSinkAndNoRecords(t *testing.T) {
	_, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	cfg.Pipeline.Stages.Tracing = false

	stages, sink, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if sink != nil {
		t.Fatalf("sink = %v, want nil with tracing disabled", sink)
	}
	for _, s := range stages {
		if s.Name() == "tracing" {
			t.Fatal("tracing stage present with tracing disabled")
		}
	}

	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
	if rec := serve(g, post(bodyA)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if lines := readRecordLines(t, cfg.Tracing.Sink.Path); len(lines) != 0 {
		t.Fatalf("tracing disabled but %d record lines were written", len(lines))
	}
}

func TestGatewayNew_TracingStartupChecks(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, c *config.Config)
		wantErr bool
	}{
		{
			name:   "fingerprint key env var unset: starts",
			mutate: func(t *testing.T, c *config.Config) { t.Setenv(gwTraceKeyEnv, "") },
		},
		{
			name:    "fingerprint key shorter than 32 bytes",
			mutate:  func(t *testing.T, c *config.Config) { t.Setenv(gwTraceKeyEnv, strings.Repeat("g", 31)) },
			wantErr: true,
		},
		{
			name: "session pattern that does not compile",
			mutate: func(t *testing.T, c *config.Config) {
				c.Tracing.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{Header: "X-Session-Id", Pattern: `([`}}}}
			},
			wantErr: true,
		},
		{
			name: "session pattern with two capture groups",
			mutate: func(t *testing.T, c *config.Config) {
				c.Tracing.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{Header: "X-Session-Id", Pattern: `^(a)-(b)$`}}}}
			},
			wantErr: true,
		},
		{
			name: "sink file that cannot be opened",
			mutate: func(t *testing.T, c *config.Config) {
				c.Tracing.Sink.Path = filepath.Join(t.TempDir(), "no-such-dir", "records.jsonl")
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tracingGatewayConfig(t, "http://127.0.0.1:1")
			tt.mutate(t, &cfg)
			t.Setenv(gwAnthropicKeyEnv, "sk-test-upstream")
			prov, err := provider.NewAnthropicProvider(cfg.Provider.BaseURL, gwAnthropicKeyEnv)
			if err != nil {
				t.Fatalf("NewAnthropicProvider: %v", err)
			}

			g, err := New(cfg, zaptest.NewLogger(t), prov)
			if tt.wantErr {
				if err == nil {
					t.Fatal("gateway.New = nil error, want startup to fail")
				}
				if g != nil {
					t.Fatal("gateway.New returned a *Gateway alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("gateway.New: %v", err)
			}
		})
	}
}

// --- End to end ------------------------------------------------------------------------

func TestTracing_ToolLoopJoinsAcrossCalls(t *testing.T) {
	_, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	if rec := serve(g, post(bodyA)); rec.Code != http.StatusOK {
		t.Fatalf("call A status = %d, want 200", rec.Code)
	}
	aReq, aResp := callRecords(t, cfg.Tracing.Sink.Path)
	if aResp == nil {
		t.Fatal("call A has no response record once its response ended")
	}

	if rec := serve(g, post(bodyB)); rec.Code != http.StatusOK {
		t.Fatalf("call B status = %d, want 200", rec.Code)
	}
	bReq, _ := callRecords(t, cfg.Tracing.Sink.Path)

	if aReq["call_id"] == bReq["call_id"] {
		t.Fatal("calls A and B share a call_id")
	}
	if trs := list(t, aReq, "tool_results"); len(trs) != 0 {
		t.Fatalf("A tool_results = %v, want []", trs)
	}

	tcs := list(t, aResp, "tool_calls")
	if len(tcs) != 1 {
		t.Fatalf("A tool_calls = %v, want one", tcs)
	}
	tc := tcs[0]
	if tc["tool_call_id"] != "toolu_A" || tc["tool_name"] != "bash" || tc["outcome"] != "delivered" || tc["recorded"] != true || tc["server_side"] != false {
		t.Fatalf("A tool call = %v, want toolu_A/bash delivered and recorded", tc)
	}
	if names, _ := tc["argument_names"].([]any); len(names) != 1 || names[0] != "command" {
		t.Fatalf("argument_names = %v, want [command]", tc["argument_names"])
	}
	if tc["size_bytes"] != float64(len(`{"command":"ls"}`)) {
		t.Fatalf("size_bytes = %v, want %d", tc["size_bytes"], len(`{"command":"ls"}`))
	}
	if fp, _ := tc["fingerprint"].(string); !strings.HasPrefix(fp, "fp_") || len(fp) != 19 {
		t.Fatalf("fingerprint = %v, want fp_ + 16 hex", tc["fingerprint"])
	}
	if field(aResp, "tokens", "sent") != float64(100) || field(aResp, "tokens", "received") != float64(42) {
		t.Fatalf("A tokens = %v, want sent 100, received 42", aResp["tokens"])
	}

	trs := list(t, bReq, "tool_results")
	if len(trs) != 1 || trs[0]["tool_call_id"] != "toolu_A" || trs[0]["is_error"] != false {
		t.Fatalf("B tool_results = %v, want toolu_A's result", trs)
	}
	relayed := tsField(t, tc, "relayed_at")
	received := tsField(t, bReq, "received_at")
	if d := received.Sub(relayed); d < 0 {
		t.Fatalf("tool duration = B.received_at - T.relayed_at = %v, want >= 0", d)
	}
}

func TestTracing_StageOrderAndDenialInRequestRecord(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := withAgents(t, tracingGatewayConfig(t, url))
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	t.Run("allowed", func(t *testing.T) {
		serve(g, post(bodyA, "Authorization", "Bearer "+agentXKey))
		req, resp := callRecords(t, cfg.Tracing.Sink.Path)
		var got []string
		for _, s := range list(t, req, "stages") {
			got = append(got, fmt.Sprintf("%s:%s", s["name"], s["decision"]))
		}
		if want := []string{"identification:allowed", "tracing:allowed", "token_counting:allowed"}; !slices.Equal(got, want) {
			t.Fatalf("request stages = %v, want %v", got, want)
		}
		if req["agent_id"] != "agent-x" || resp["agent_id"] != "agent-x" {
			t.Fatalf("agent_id = %v / %v, want agent-x", req["agent_id"], resp["agent_id"])
		}
		var respStages []string
		for _, s := range list(t, resp, "stages") {
			respStages = append(respStages, s["name"].(string))
		}
		if want := []string{"tracing", "token_counting"}; !slices.Equal(respStages, want) {
			t.Fatalf("response stages = %v, want %v (identification does not observe responses)", respStages, want)
		}
	})

	t.Run("denied by identification", func(t *testing.T) {
		f.mu.Lock()
		before := len(f.requests)
		f.mu.Unlock()

		rec := serve(g, post(bodyA))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		req, resp := callRecords(t, cfg.Tracing.Sink.Path)
		if resp != nil {
			t.Fatal("a refused call has a response record")
		}
		var got []string
		for _, s := range list(t, req, "stages") {
			got = append(got, fmt.Sprintf("%s:%s", s["name"], s["decision"]))
		}
		if want := []string{"identification:denied", "tracing:not_run", "token_counting:not_run"}; !slices.Equal(got, want) {
			t.Fatalf("request stages = %v, want %v", got, want)
		}
		if field(req, "outcome", "result") != "denied" || field(req, "outcome", "status") != float64(401) || field(req, "outcome", "stage") != "identification" {
			t.Fatalf("outcome = %v, want denied/401/identification", req["outcome"])
		}
		if req["model"] != nil || field(req, "session", "status") != "absent" {
			t.Fatalf("model/session = %v/%v, want null/absent: tracing never ran", req["model"], req["session"])
		}
		f.mu.Lock()
		after := len(f.requests)
		f.mu.Unlock()
		if after != before {
			t.Fatal("the provider received a refused call")
		}
	})
}

func TestTracing_CallerTraceparentIsNeverRecordedOrEchoed(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const spanID = "00f067aa0ba902b7"
	_, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	rec := serve(g, post(bodyA, "traceparent", "00-"+traceID+"-"+spanID+"-01"))

	for _, marker := range []string{traceID, spanID} {
		for _, line := range readRecordLines(t, cfg.Tracing.Sink.Path) {
			if strings.Contains(line, marker) {
				t.Fatalf("record carries the caller's traceparent (%s): %s", marker, line)
			}
		}
		if strings.Contains(rec.Body.String(), marker) || strings.Contains(fmt.Sprint(rec.Header()), marker) {
			t.Fatalf("response echoes the caller's traceparent (%s)", marker)
		}
	}
}

func TestTracing_SessionFromHeader(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := withAgents(t, tracingGatewayConfig(t, url))
	cfg.Tracing.Sessions = []config.SessionSourceList{{Agents: []string{"agent-x"}, Sources: []config.SessionSource{{Header: "X-Session-Id"}}}}
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	serve(g, post(bodyA, "Authorization", "Bearer "+agentXKey, "X-Session-Id", "s1"))

	req, resp := callRecords(t, cfg.Tracing.Sink.Path)
	for _, r := range []map[string]any{req, resp} {
		s := r["session"]
		if field(r, "session", "id") != "s1" || field(r, "session", "status") != "claimed" ||
			field(r, "session", "source") != "header" || field(r, "session", "source_name") != "X-Session-Id" {
			t.Fatalf("%s record session = %v, want s1 claimed from header X-Session-Id", r["record"], s)
		}
	}
	if got := f.last(t).header.Get("X-Session-Id"); got != "s1" {
		t.Fatalf("provider received X-Session-Id = %q, want it unchanged", got)
	}
}

func TestTracing_SessionPatternRecordsOnlyTheCapture(t *testing.T) {
	const userMarker, accountMarker = "abc123qqz", "def456qqz"
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := withAgents(t, tracingGatewayConfig(t, url))
	cfg.Tracing.Sessions = []config.SessionSourceList{{
		Agents:  []string{"agent-x"},
		Sources: []config.SessionSource{{BodyField: "metadata.user_id", Pattern: `_session_([0-9a-f-]{36})$`}},
	}}
	logger, logs := observed()
	g := newTracingGateway(t, cfg, logger)

	body := `{"model":"claude-sonnet-5","stream":true,"metadata":{"user_id":"user_` + userMarker + `_account_` + accountMarker + `_session_` + sessionUUID + `"},"messages":[{"role":"user","content":"hi"}]}`
	serve(g, post(body, "Authorization", "Bearer "+agentXKey))

	req, resp := callRecords(t, cfg.Tracing.Sink.Path)
	for _, r := range []map[string]any{req, resp} {
		if field(r, "session", "id") != sessionUUID || field(r, "session", "status") != "claimed" {
			t.Fatalf("%s record session = %v, want %s claimed", r["record"], r["session"], sessionUUID)
		}
	}
	for _, marker := range []string{userMarker, accountMarker} {
		for _, line := range readRecordLines(t, cfg.Tracing.Sink.Path) {
			if strings.Contains(line, marker) {
				t.Fatalf("record carries %q from outside the capture group: %s", marker, line)
			}
		}
		if strings.Contains(logText(logs), marker) {
			t.Fatalf("application logs carry %q:\n%s", marker, logText(logs))
		}
	}
	if got := f.last(t).body; string(got) != body {
		t.Fatalf("provider received a different body:\n got %s\nwant %s", got, body)
	}
}

func TestTracing_NoSessionSourcesMeansAbsent(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	rec := serve(g, post(bodyA, "X-Session-Id", "s1", "baggage", "session.id=s9"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	req, resp := callRecords(t, cfg.Tracing.Sink.Path)
	for _, r := range []map[string]any{req, resp} {
		if field(r, "session", "status") != "absent" || field(r, "session", "id") != nil ||
			field(r, "session", "source") != nil || field(r, "session", "source_name") != nil {
			t.Fatalf("%s record session = %v, want absent with null id/source/source_name", r["record"], r["session"])
		}
	}
	last := f.last(t)
	if last.header.Get("X-Session-Id") != "s1" || last.header.Get("baggage") != "session.id=s9" {
		t.Fatalf("provider headers = %v, want session-looking headers forwarded unchanged", last.header)
	}
}

func TestTracing_SameSessionFromTwoAgentsIsTwoSessions(t *testing.T) {
	_, url := newFakeAnthropic(t, loopResponder)
	cfg := withAgents(t, tracingGatewayConfig(t, url))
	cfg.Tracing.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{Header: "X-Session-Id"}}}}
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	serve(g, post(bodyA, "Authorization", "Bearer "+agentXKey, "X-Session-Id", "s1"))
	serve(g, post(bodyA, "Authorization", "Bearer "+agentYKey, "X-Session-Id", "s1"))

	sessions := map[string]int{}
	for _, r := range readRecords(t, cfg.Tracing.Sink.Path) {
		if field(r, "session", "status") != "claimed" {
			continue
		}
		sessions[fmt.Sprintf("%v/%v", r["agent_id"], field(r, "session", "id"))]++
	}
	if len(sessions) != 2 || sessions["agent-x/s1"] != 2 || sessions["agent-y/s1"] != 2 {
		t.Fatalf("records grouped by (agent_id, session.id) = %v, want two sessions of two records each", sessions)
	}
}

func TestTracing_NoArgumentOrResultValueAnywhere(t *testing.T) {
	const readLimit = 1024
	bigArgs := `{"content": "ARGMARKER_big ` + strings.Repeat("x", 2*readLimit) + `"}`
	var bigParts []string
	for i := 0; i < len(bigArgs); i += 100 {
		bigParts = append(bigParts, bigArgs[i:min(i+100, len(bigArgs))])
	}

	tests := []struct {
		name    string
		body    string
		stream  []string
		markers []string
	}{
		{
			name:    "well-formed tool call and tool result",
			body:    strings.Replace(bodyB, `"file1\nfile2"`, `"RESULTMARKER_content"`, 1),
			stream:  toolCallStream("msg_1", "toolu_1", "bash", `{"command": "cat ARGMARKER_well"}`),
			markers: []string{"ARGMARKER_well", "RESULTMARKER_content"},
		},
		{
			name:    "arguments that are not valid JSON",
			body:    bodyA,
			stream:  toolCallStream("msg_2", "toolu_2", "bash", `{"command": "rm ARGMARKER_invalid`),
			markers: []string{"ARGMARKER_invalid"},
		},
		{
			name:    "arguments over the read limit",
			body:    bodyA,
			stream:  toolCallStream("msg_3", "toolu_3", "write_file", bigParts...),
			markers: []string{"ARGMARKER_big"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, url := newFakeAnthropic(t, streamResponder(tt.stream))
			cfg := tracingGatewayConfig(t, url)
			cfg.Tracing.MaxReadBytes = readLimit
			logger, logs := observed()
			g := newTracingGateway(t, cfg, logger)

			rec := serve(g, post(tt.body))
			if rec.Body.String() != strings.Join(tt.stream, "") {
				t.Fatal("caller did not receive the response unchanged")
			}
			_, resp := callRecords(t, cfg.Tracing.Sink.Path)
			if resp == nil || len(list(t, resp, "tool_calls")) != 1 {
				t.Fatalf("response record = %v, want one tool call recorded", resp)
			}
			for _, marker := range tt.markers {
				for _, line := range readRecordLines(t, cfg.Tracing.Sink.Path) {
					if strings.Contains(line, marker) {
						t.Fatalf("record carries %s: %s", marker, line)
					}
				}
				if strings.Contains(logText(logs), marker) {
					t.Fatalf("application logs carry %s:\n%s", marker, logText(logs))
				}
			}
		})
	}
}

func TestTracing_ToolCallTracingCannotRecordIsStillRelayedAndLogged(t *testing.T) {
	stream := toolCallStream("msg_bad", "toolu_bad", "bash", `{"command": `)
	_, url := newFakeAnthropic(t, streamResponder(stream))
	cfg := tracingGatewayConfig(t, url)
	logger, logs := observed()
	g := newTracingGateway(t, cfg, logger)

	rec := serve(g, post(bodyA))
	if rec.Code != http.StatusOK || rec.Body.String() != strings.Join(stream, "") {
		t.Fatalf("caller got %d with %d bytes, want 200 and the stream byte-identical", rec.Code, rec.Body.Len())
	}

	_, resp := callRecords(t, cfg.Tracing.Sink.Path)
	if field(resp, "outcome", "result") != "delivered" {
		t.Fatalf("outcome.result = %v, want delivered", field(resp, "outcome", "result"))
	}
	var tracingObs any
	for _, s := range list(t, resp, "stages") {
		if s["name"] == "tracing" {
			tracingObs = s["observation"]
		}
	}
	if tracingObs != "failed" {
		t.Fatalf("tracing observation = %v, want failed", tracingObs)
	}
	tcs := list(t, resp, "tool_calls")
	if len(tcs) != 1 || tcs[0]["tool_call_id"] != "toolu_bad" || tcs[0]["recorded"] != false {
		t.Fatalf("tool_calls = %v, want toolu_bad with recorded false", tcs)
	}
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, want tool_use as the provider sent it", resp["stop_reason"])
	}

	callID, _ := resp["call_id"].(string)
	found := false
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		if strings.Contains(fmt.Sprint(e.Message, e.ContextMap()), callID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error-level log names call %s:\n%s", callID, logText(logs))
	}
}

func TestTracing_UnknownToolResultIsForwardedAndRecordedWithoutFinding(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	body := strings.ReplaceAll(bodyB, "toolu_A", "toolu_never_relayed")
	if rec := serve(g, post(body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := f.last(t).body; string(got) != body {
		t.Fatalf("provider received a different body:\n got %s\nwant %s", got, body)
	}
	req, _ := callRecords(t, cfg.Tracing.Sink.Path)
	trs := list(t, req, "tool_results")
	if len(trs) != 1 || trs[0]["tool_call_id"] != "toolu_never_relayed" {
		t.Fatalf("tool_results = %v, want the unknown result listed", trs)
	}
	if fs := list(t, req, "findings"); len(fs) != 0 {
		t.Fatalf("findings = %v, want []", fs)
	}
}

func TestTracing_ModelAliasAndResponseIdentifiers(t *testing.T) {
	const jsonResponse = `{"id":"R","type":"message","role":"assistant","model":"M-1","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"ls"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":6}}`
	tests := []struct {
		name    string
		body    string
		respond func(http.ResponseWriter, []byte)
	}{
		{
			name: "streamed",
			body: `{"model":"M","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			respond: func(w http.ResponseWriter, _ []byte) {
				events := []string{messageStartEvent("R", "M-1")}
				events = append(events, toolUseEvents(0, "toolu_1", "bash", `{"command":"ls"}`)...)
				writeStream(w, append(events, messageEndEvents("tool_use")...))
			},
		},
		{
			name: "not streamed",
			body: `{"model":"M","messages":[{"role":"user","content":"hi"}]}`,
			respond: func(w http.ResponseWriter, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(jsonResponse))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, url := newFakeAnthropic(t, tt.respond)
			cfg := tracingGatewayConfig(t, url)
			g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
			serve(g, post(tt.body))

			req, resp := callRecords(t, cfg.Tracing.Sink.Path)
			if req["model"] != "M" {
				t.Fatalf("request model = %v, want M", req["model"])
			}
			if resp["response_model"] != "M-1" || resp["response_id"] != "R" || resp["stop_reason"] != "tool_use" {
				t.Fatalf("response_model/response_id/stop_reason = %v/%v/%v, want M-1/R/tool_use",
					resp["response_model"], resp["response_id"], resp["stop_reason"])
			}
		})
	}
}

func TestTracing_UnreachableProviderIsUnanswered(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + ln.Addr().String()
	_ = ln.Close()

	cfg := tracingGatewayConfig(t, url)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
	rec := serve(g, post(bodyA))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	_, resp := callRecords(t, cfg.Tracing.Sink.Path)
	if resp == nil {
		t.Fatal("no response record for a forwarded call the provider never answered")
	}
	if field(resp, "outcome", "result") != "unanswered" {
		t.Fatalf("outcome.result = %v, want unanswered", field(resp, "outcome", "result"))
	}
	if v, ok := resp["outcome"].(map[string]any)["upstream_status"]; !ok || v != nil {
		t.Fatalf("outcome.upstream_status = %#v, want null", v)
	}
	if field(resp, "outcome", "status") != float64(http.StatusBadGateway) {
		t.Fatalf("outcome.status = %v, want 502", field(resp, "outcome", "status"))
	}
}

func TestTracing_StdoutCarriesOnlyRecordsByDefault(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	_, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	cfg.Tracing.Sink.Path = ""
	logger, err := logging.New(config.LoggingConfig{Level: "debug", Format: "json"})
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	g := newTracingGateway(t, cfg, logger)
	serve(g, post(bodyA))
	serve(g, post(bodyB))
	_ = logger.Sync()

	os.Stdout = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)

	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("stdout has %d lines, want 4 records (two calls):\n%s", len(lines), out)
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line %d is not a JSON record: %s", i, line)
		}
		if m["schema"] != "agentkeeper.trace/v1" || (m["record"] != "request" && m["record"] != "response") {
			t.Fatalf("stdout line %d is not a trace record: %s", i, line)
		}
	}
}

// --- Stateless: restarts, several instances, assembly from records alone ---------------------

var volatileKeys = map[string]bool{
	"call_id": true, "received_at": true, "started_at": true, "duration_us": true,
	"first_byte_us": true, "relayed_at": true,
}

// normalize drops values that legitimately differ between two runs of the
// same call (identifiers and clock readings), plus any keys in extra.
func normalize(v any, extra ...string) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if volatileKeys[k] || slices.Contains(extra, k) {
				out[k] = "<volatile>"
				continue
			}
			out[k] = normalize(val, extra...)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalize(val, extra...)
		}
		return out
	default:
		return v
	}
}

// runLoop sends A, B and C through the gateways given (one per call) and
// returns all records, and B's request and response records.
func runLoop(t *testing.T, url string, instances []int) (all []map[string]any, bRecords []map[string]any) {
	t.Helper()
	gateways := map[int]*Gateway{}
	paths := map[int]string{}
	for _, i := range instances {
		if _, ok := gateways[i]; ok {
			continue
		}
		cfg := tracingGatewayConfig(t, url)
		t.Setenv(gwTraceKeyEnv, os.Getenv("AK_GW_TRACE_TEST_RUN_KEY"))
		gateways[i] = newTracingGateway(t, cfg, zaptest.NewLogger(t))
		paths[i] = cfg.Tracing.Sink.Path
	}
	for n, body := range []string{bodyA, bodyB, bodyC} {
		if rec := serve(gateways[instances[n]], post(body)); rec.Code != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", n, rec.Code)
		}
		if n == 1 {
			req, resp := callRecords(t, paths[instances[n]])
			bRecords = []map[string]any{req, resp}
		}
	}
	seen := map[string]bool{}
	for _, i := range instances {
		if seen[paths[i]] {
			continue
		}
		seen[paths[i]] = true
		all = append(all, readRecords(t, paths[i])...)
	}
	return all, bRecords
}

type assembled struct {
	order     []string // call_ids, first to last
	durations []time.Duration
	toolIDs   []string
}

// assemble joins records as the functional spec's "Assembling a trace"
// describes, using nothing but the records.
func assemble(t *testing.T, records []map[string]any) assembled {
	t.Helper()
	requests := map[string]map[string]any{}
	responses := map[string]map[string]any{}
	for _, r := range records {
		id := r["call_id"].(string)
		switch r["record"] {
		case "request":
			requests[id] = r
		case "response":
			responses[id] = r
		}
	}
	type toolCall struct {
		from      string
		relayedAt time.Time
	}
	calls := map[string]toolCall{}
	for id, resp := range responses {
		for _, tc := range list(t, resp, "tool_calls") {
			calls[tc["tool_call_id"].(string)] = toolCall{from: id, relayedAt: tsField(t, tc, "relayed_at")}
		}
	}
	next := map[string]string{}
	incoming := map[string]bool{}
	link := map[string]string{}
	dur := map[string]time.Duration{}
	for id, req := range requests {
		for _, tr := range list(t, req, "tool_results") {
			tcID := tr["tool_call_id"].(string)
			tc, ok := calls[tcID]
			if !ok {
				continue
			}
			next[tc.from] = id
			incoming[id] = true
			link[tc.from] = tcID
			dur[tc.from] = tsField(t, req, "received_at").Sub(tc.relayedAt)
		}
	}
	var first string
	for id := range requests {
		if !incoming[id] {
			if first != "" {
				t.Fatalf("more than one call with no incoming link: %s and %s", first, id)
			}
			first = id
		}
	}
	var a assembled
	for id := first; id != ""; id = next[id] {
		a.order = append(a.order, id)
		if _, ok := next[id]; ok {
			a.durations = append(a.durations, dur[id])
			a.toolIDs = append(a.toolIDs, link[id])
		}
	}
	return a
}

func checkAssembly(t *testing.T, records []map[string]any) {
	t.Helper()
	if len(records) != 6 {
		t.Fatalf("got %d records, want 6 (three calls)", len(records))
	}
	want := assemble(t, records)
	if len(want.order) != 3 || !slices.Equal(want.toolIDs, []string{"toolu_A", "toolu_B"}) {
		t.Fatalf("assembled %d calls joined by %v, want 3 calls joined by toolu_A then toolu_B", len(want.order), want.toolIDs)
	}
	for i, d := range want.durations {
		if d < 0 {
			t.Fatalf("tool call %d duration = %v, want >= 0", i, d)
		}
	}
	// Whatever order the records arrive in, the assembly is the same.
	orders := [][]map[string]any{slices.Clone(records)}
	slices.Reverse(orders[0])
	rng := rand.New(rand.NewPCG(1, 2))
	for range 5 {
		shuffled := slices.Clone(records)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		orders = append(orders, shuffled)
	}
	for _, o := range orders {
		got := assemble(t, o)
		if !slices.Equal(got.order, want.order) || !slices.Equal(got.durations, want.durations) {
			t.Fatalf("assembly depends on record order: got %+v, want %+v", got, want)
		}
	}
}

func TestTracing_StatelessAcrossInstances(t *testing.T) {
	tests := []struct {
		name         string
		key          string
		ignoreFields []string
	}{
		{name: "shared fingerprint key: records are the same", key: strings.Repeat("s", 32)},
		{name: "no fingerprint key: only fingerprints differ", key: "", ignoreFields: []string{"fingerprint"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, url := newFakeAnthropic(t, loopResponder)
			t.Setenv("AK_GW_TRACE_TEST_RUN_KEY", tt.key)

			singleAll, singleB := runLoop(t, url, []int{0, 0, 0})
			splitAll, splitB := runLoop(t, url, []int{1, 2, 1})

			checkAssembly(t, singleAll)
			checkAssembly(t, splitAll)

			for i := range singleB {
				a, _ := json.Marshal(normalize(singleB[i], tt.ignoreFields...))
				b, _ := json.Marshal(normalize(splitB[i], tt.ignoreFields...))
				if !bytes.Equal(a, b) {
					t.Fatalf("B's %s record differs between one instance and two:\n one %s\n two %s", singleB[i]["record"], a, b)
				}
			}

			singleFP := field(list(t, singleB[0], "tool_results")[0], "fingerprint")
			splitFP := field(list(t, splitB[0], "tool_results")[0], "fingerprint")
			if tt.key != "" && singleFP != splitFP {
				t.Fatalf("with a shared key, fingerprints differ: %v vs %v", singleFP, splitFP)
			}
			if tt.key == "" && singleFP == splitFP {
				t.Fatalf("with generated keys, two instances gave the same fingerprint %v", singleFP)
			}
		})
	}
}

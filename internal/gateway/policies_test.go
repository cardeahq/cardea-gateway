package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"

	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/policy"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

var _ pipeline.Stage = (*policiesStage)(nil)

// policiesStage must not observe responses (specs/0004: request only).
var _ = func() bool {
	var s pipeline.Stage = &policiesStage{}
	_, isObserver := s.(pipeline.ResponseObserver)
	if isObserver {
		panic("policiesStage implements ResponseObserver; policies never see a response")
	}
	return true
}()

const (
	regoHead    = "package cardea.policy\n\n"
	genericDeny = "request denied by policy"
	revisionRe  = `[0-9a-f]{12}`
	marker      = "ZQXMARKER7"
	badBashBody = `{"model":"claude-sonnet-5","stream":true,"max_tokens":1024,"messages":[` +
		`{"role":"user","content":"clean up"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_R","name":"bash","input":{"command":"rm -rf /data"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_R","content":"removed"}]}]}`
	badBashBody2 = `{"model":"claude-sonnet-5","stream":true,"max_tokens":1024,"messages":[` +
		`{"role":"user","content":"clean up"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_R","name":"bash","input":{"command":"rm -rf /data"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_R","content":"removed"}]},` +
		`{"role":"assistant","content":"done"},{"role":"user","content":"now what"}]}`
)

var revisionPattern = regexp.MustCompile(`^` + revisionRe + `$`)

// --- helpers -----------------------------------------------------------------------

func writePolicyFile(t *testing.T, dir, rules string) string {
	t.Helper()
	p := filepath.Join(dir, "policy.rego")
	if err := os.WriteFile(p, []byte(regoHead+rules), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return p
}

func policyConfig(t *testing.T, upstream, rules string) config.Config {
	t.Helper()
	cfg := tracingGatewayConfig(t, upstream)
	cfg.Pipeline.Stages.Policies = true
	cfg.Policies = config.PoliciesConfig{
		Path:        writePolicyFile(t, t.TempDir(), rules),
		WatchSettle: 100 * time.Millisecond,
		EvalTimeout: 5 * time.Second,
	}
	return cfg
}

func policyBody(model string) string {
	return `{"model":"` + model + `","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
}

func denyModel(id, model string) string {
	return fmt.Sprintf(`deny contains %q if { input.model == %q }`, id, model)
}

type errBody struct {
	Error struct {
		Type      string `json:"type"`
		Stage     string `json:"stage"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body is not the denial envelope: %v: %s", err, rec.Body.String())
	}
	return e
}

func (f *fakeAnthropic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func stageEntry(t *testing.T, req map[string]any, name string) map[string]any {
	t.Helper()
	for _, s := range list(t, req, "stages") {
		if s["name"] == name {
			return s
		}
	}
	t.Fatalf("no %q stage in the request record: %v", name, req["stages"])
	return nil
}

func everything(t *testing.T, tracePath, logs string, recs ...*httptest.ResponseRecorder) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(logs)
	for _, l := range readRecordLines(t, tracePath) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	for _, r := range recs {
		fmt.Fprintf(&b, "%v\n%s\n", r.Header(), r.Body.String())
	}
	return b.String()
}

// --- buildStages -------------------------------------------------------------------

func buildCfg(t *testing.T, identification, tracing, policies bool, policyPath string) config.Config {
	t.Helper()
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Identification: identification, Tracing: tracing, Policies: policies},
		},
		Tracing:  fileSinkTracing(t),
		Policies: config.PoliciesConfig{Path: policyPath, WatchSettle: time.Second, EvalTimeout: time.Second},
	}
	if identification {
		t.Setenv("AK_GW_TEST_POLICY_AGENT_KEY", "policy-agent-key")
		cfg.Agents = []config.AgentConfig{{AgentID: "agent-a", APIKeyEnv: "AK_GW_TEST_POLICY_AGENT_KEY"}}
	}
	return cfg
}

func TestBuildStages_PoliciesComesThirdAndFollowsItsConfigBoolean(t *testing.T) {
	tests := []struct {
		name           string
		identification bool
		tracing        bool
		policies       bool
		want           []string
	}{
		{"all three on", true, true, true, []string{"identification", "tracing", "policies"}},
		{"policies off", true, true, false, []string{"identification", "tracing"}},
		{"policies on, tracing off", true, false, true, []string{"identification", "policies"}},
		{"policies only", false, false, true, []string{"policies"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildCfg(t, tt.identification, tt.tracing, tt.policies, "")
			stages, sink, eng, _, err := buildStages(cfg, zaptest.NewLogger(t))
			if err != nil {
				t.Fatalf("buildStages: %v", err)
			}
			if sink != nil {
				t.Cleanup(func() { _ = sink.Close() })
			}
			var names []string
			for _, s := range stages {
				names = append(names, s.Name())
			}
			if !slices.Equal(names, tt.want) {
				t.Fatalf("stage order = %v, want %v", names, tt.want)
			}
			if tt.policies && eng == nil {
				t.Fatal("buildStages returned a nil engine with the policies stage on")
			}
		})
	}
}

func TestBuildStages_PolicyErrorsFailConstruction(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.rego")
	if err := os.WriteFile(broken, []byte(regoHead+"deny contains \"x\" if { input.a == }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		file string
	}{
		{"missing policy file", filepath.Join(dir, "missing.rego"), "missing.rego"},
		{"policy that does not parse", broken, "broken.rego"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildCfg(t, false, true, true, tt.path)
			stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
			if sink != nil {
				_ = sink.Close()
			}
			if err == nil {
				t.Fatal("buildStages = nil error, want a failure")
			}
			if stages != nil {
				t.Fatalf("stages = %v, want nil on error", stages)
			}
			if !strings.Contains(err.Error(), tt.file) {
				t.Fatalf("error %q does not name %s", err, tt.file)
			}

			_, gerr := New(cfg, zaptest.NewLogger(t), stubProvider{handler: http.NotFoundHandler()})
			if gerr == nil {
				t.Fatal("gateway.New = nil error: serve must exit before listening on a bad policy")
			}
		})
	}
}

func TestBuildStages_PolicyConfiguredWithoutIdentificationWarns(t *testing.T) {
	policyPath := writePolicyFile(t, t.TempDir(), denyModel("r", "m"))
	tests := []struct {
		name           string
		identification bool
		path           string
		wantWarn       bool
	}{
		{"policy configured and identification off", false, policyPath, true},
		{"policy configured and identification on", true, policyPath, false},
		{"no policy and identification off", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, logs := observed()
			cfg := buildCfg(t, tt.identification, true, true, tt.path)
			_, sink, _, _, err := buildStages(cfg, logger)
			if err != nil {
				t.Fatalf("buildStages: %v", err)
			}
			t.Cleanup(func() { _ = sink.Close() })

			var warned bool
			for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
				if strings.Contains(strings.ToLower(entryString(e.Message, e.ContextMap())), "identification") {
					warned = true
				}
			}
			if warned != tt.wantWarn {
				t.Fatalf("warning naming identification = %v, want %v; logs:\n%s", warned, tt.wantWarn, logText(logs))
			}
		})
	}
}

func entryString(msg string, ctx map[string]any) string { return fmt.Sprintf("%s %v", msg, ctx) }

// --- no policy ---------------------------------------------------------------------

func TestPolicies_NoPolicyForwardsByteIdentical(t *testing.T) {
	const upstreamBody = `{"id":"msg_1","type":"message","content":[{"type":"text","text":"ok"}]}`
	f, url := newFakeAnthropic(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	})
	cfg := tracingGatewayConfig(t, url)
	cfg.Pipeline.Stages.Policies = true // on, but nothing configured
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"weird":  "spacing"}`
	rec := serve(g, post(body))

	if rec.Code != http.StatusOK || rec.Body.String() != upstreamBody {
		t.Fatalf("status=%d body=%q, want the upstream response unchanged", rec.Code, rec.Body.String())
	}
	if got := string(f.last(t).body); got != body {
		t.Fatalf("provider received %q, want the request byte-identical", got)
	}
	req, _ := callRecords(t, cfg.Tracing.Sink.Path)
	st := stageEntry(t, req, "policies")
	if st["decision"] != "allowed" {
		t.Fatalf("policies decision = %v, want allowed", st["decision"])
	}
	if v, present := st["detail"]; !present || v != nil {
		t.Fatalf("policies detail = %#v, want null when no policy is configured", v)
	}
}

// --- allow -------------------------------------------------------------------------

func TestPolicies_AllowedCallIsQuietAndRecordedWithRevisionAndEmptyRules(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := policyConfig(t, url, denyModel("no-opus", "claude-opus-4"))
	logger, logs := observed()
	g := newTracingGateway(t, cfg, logger)
	logs.TakeAll()

	rec := serve(g, post(policyBody("claude-sonnet-5")))
	if rec.Code != http.StatusOK || f.count() != 1 {
		t.Fatalf("status=%d provider calls=%d, want a forwarded 200", rec.Code, f.count())
	}
	for _, e := range logs.All() {
		if e.Level >= zapcore.InfoLevel {
			t.Fatalf("an allowed call wrote a %s log line: %s", e.Level, entryString(e.Message, e.ContextMap()))
		}
	}

	req, _ := callRecords(t, cfg.Tracing.Sink.Path)
	st := stageEntry(t, req, "policies")
	if st["decision"] != "allowed" {
		t.Fatalf("policies decision = %v, want allowed", st["decision"])
	}
	detail, ok := st["detail"].(map[string]any)
	if !ok {
		t.Fatalf("detail = %#v, want an object", st["detail"])
	}
	if rev, _ := detail["revision"].(string); !revisionPattern.MatchString(rev) {
		t.Fatalf("detail.revision = %v, want 12 hex characters", detail["revision"])
	}
	if rules, ok := detail["rules"].([]any); !ok || len(rules) != 0 {
		t.Fatalf("detail.rules = %#v, want []", detail["rules"])
	}
}

func TestPolicies_ResponseOfAnAllowedCallIsByteIdenticalAndItsRecordUnchanged(t *testing.T) {
	events := toolCallStream("msg_P", "toolu_P", "bash", `{"command": "ls"}`)
	want := strings.Join(events, "")

	run := func(t *testing.T, withPolicy bool) (*httptest.ResponseRecorder, map[string]any) {
		_, url := newFakeAnthropic(t, streamResponder(events))
		cfg := tracingGatewayConfig(t, url)
		if withPolicy {
			cfg = policyConfig(t, url, denyModel("no-opus", "claude-opus-4"))
		}
		g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
		rec := serve(g, post(policyBody("claude-sonnet-5")))
		_, resp := callRecords(t, cfg.Tracing.Sink.Path)
		return rec, resp
	}
	recControl, respControl := run(t, false)
	recPolicy, respPolicy := run(t, true)

	if recPolicy.Body.String() != want || recControl.Body.String() != want {
		t.Fatalf("response bodies differ from what the provider sent")
	}
	if recPolicy.Code != recControl.Code {
		t.Fatalf("status %d != %d", recPolicy.Code, recControl.Code)
	}
	for _, k := range []string{"streamed", "response_id", "response_model", "stop_reason", "tokens", "outcome", "findings"} {
		if !reflect.DeepEqual(respPolicy[k], respControl[k]) {
			t.Fatalf("response record %q = %v with a policy, %v without: policies must add nothing to a response record", k, respPolicy[k], respControl[k])
		}
	}
	names := func(r map[string]any) []string {
		var out []string
		for _, s := range list(t, r, "stages") {
			out = append(out, s["name"].(string))
		}
		return out
	}
	if !slices.Equal(names(respPolicy), names(respControl)) {
		t.Fatalf("response record stages %v with a policy, %v without", names(respPolicy), names(respControl))
	}
	if !slices.Equal(names(respPolicy), []string{"tracing"}) {
		t.Fatalf("response stages = %v, want only tracing: policies never observe a response", names(respPolicy))
	}
}

func TestPolicies_StageDisabledForwardsWhatARuleWouldDeny(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := policyConfig(t, url, denyModel("no-opus", "claude-opus-4"))
	cfg.Pipeline.Stages.Policies = false
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	rec := serve(g, post(policyBody("claude-opus-4")))
	if rec.Code != http.StatusOK || f.count() != 1 {
		t.Fatalf("status=%d provider calls=%d, want forwarded with the stage off", rec.Code, f.count())
	}
	req, _ := callRecords(t, cfg.Tracing.Sink.Path)
	for _, s := range list(t, req, "stages") {
		if s["name"] == "policies" {
			t.Fatal("a policies entry in the record with the stage off")
		}
	}
}

// --- deny --------------------------------------------------------------------------

func TestPolicies_DenialContract(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := policyConfig(t, url, `deny contains "no-opus" if { input.model == "claude-opus-4" }
deny contains "opus-needs-review" if { startswith(input.model, "claude-opus") }
deny contains "any-stream" if { input.stream == true }`)
	logger, logs := observed()
	g := newTracingGateway(t, cfg, logger)

	rec := serve(g, post(policyBody("claude-opus-4")))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Values("X-Should-Retry"); !slices.Equal(got, []string{"false"}) {
		t.Fatalf("X-Should-Retry = %v, want [false]", got)
	}
	if f.count() != 0 {
		t.Fatalf("the provider received %d requests, want none for a denied call", f.count())
	}
	e := decodeErr(t, rec)
	if e.Error.Type != "gateway_denied" || e.Error.Stage != "policies" || e.Error.Message != genericDeny {
		t.Fatalf("error = %+v, want gateway_denied from policies with the generic message", e.Error)
	}
	if !regexp.MustCompile(`^ak_[0-9a-f]{16}$`).MatchString(e.Error.RequestID) {
		t.Fatalf("request_id = %q, want a per-call identifier", e.Error.RequestID)
	}
	for _, id := range []string{"no-opus", "opus-needs-review", "any-stream"} {
		if strings.Contains(rec.Body.String(), id) || strings.Contains(fmt.Sprint(rec.Header()), id) {
			t.Fatalf("the caller can see rule id %q", id)
		}
	}

	req, resp := callRecords(t, cfg.Tracing.Sink.Path)
	if resp != nil {
		t.Fatal("a denied call has a response record")
	}
	if field(req, "outcome", "result") != "denied" || field(req, "outcome", "stage") != "policies" {
		t.Fatalf("outcome = %v, want denied by policies", req["outcome"])
	}
	st := stageEntry(t, req, "policies")
	if st["decision"] != "denied" {
		t.Fatalf("policies decision = %v, want denied", st["decision"])
	}
	detail := st["detail"].(map[string]any)
	rev, _ := detail["revision"].(string)
	if !revisionPattern.MatchString(rev) {
		t.Fatalf("detail.revision = %v, want 12 hex characters", detail["revision"])
	}
	gotRules := []string{}
	for _, r := range detail["rules"].([]any) {
		gotRules = append(gotRules, r.(string))
	}
	if want := []string{"any-stream", "no-opus", "opus-needs-review"}; !slices.Equal(gotRules, want) {
		t.Fatalf("detail.rules = %v, want %v (every fired rule, sorted)", gotRules, want)
	}

	var warned bool
	for _, le := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		text := entryString(le.Message, le.ContextMap())
		if strings.Contains(text, "no-opus") && strings.Contains(text, "opus-needs-review") &&
			strings.Contains(text, "any-stream") && strings.Contains(text, rev) && strings.Contains(text, e.Error.RequestID) {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warn line names the call id, the revision and all three rule ids; logs:\n%s", logText(logs))
	}
}

func TestPolicies_AgentRule(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := withAgents(t, policyConfig(t, url, `deny contains "agent-x-banned" if { input.agent.id == "agent-x" }`))
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	if rec := serve(g, post(policyBody("m"), "Authorization", "Bearer "+agentXKey)); rec.Code != http.StatusForbidden {
		t.Fatalf("agent-x status = %d, want 403", rec.Code)
	}
	if rec := serve(g, post(policyBody("m"), "Authorization", "Bearer "+agentYKey)); rec.Code != http.StatusOK {
		t.Fatalf("agent-y status = %d, want 200", rec.Code)
	}
	if f.count() != 1 {
		t.Fatalf("provider calls = %d, want 1 (only agent-y)", f.count())
	}
}

func TestPolicies_EarlierDenialMeansPoliciesNotRun(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := withAgents(t, policyConfig(t, url, `deny contains "always" if { true }`))
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	rec := serve(g, post(policyBody("m"))) // no credentials
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want identification's 401", rec.Code)
	}
	if f.count() != 0 {
		t.Fatal("the provider received a refused call")
	}
	req, _ := callRecords(t, cfg.Tracing.Sink.Path)
	if d := stageEntry(t, req, "policies")["decision"]; d != "not_run" {
		t.Fatalf("policies decision = %v, want not_run", d)
	}
}

func TestPolicies_SessionIsVisibleOnlyWhenClaimed(t *testing.T) {
	rules := `deny contains "has-session" if { input.session.id }`
	tests := []struct {
		name     string
		tracing  bool
		headers  []string
		wantCode int
	}{
		{"claimed session is visible to rules", true, []string{"X-Session-Id", "s1"}, http.StatusForbidden},
		{"no session found: no session key", true, nil, http.StatusOK},
		{"tracing off: no session key even with the header", false, []string{"X-Session-Id", "s1"}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, url := newFakeAnthropic(t, loopResponder)
			cfg := policyConfig(t, url, rules)
			cfg.Pipeline.Stages.Tracing = tt.tracing
			cfg.Tracing.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{Header: "X-Session-Id"}}}}
			g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
			if rec := serve(g, post(policyBody("m"), tt.headers...)); rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestPolicies_StatelessDetectAndHalt(t *testing.T) {
	rules := `deny contains "destructive-bash" if {
	some c in input.tool_calls
	c.name == "bash"
	contains(c.arguments.command, "rm -rf")
}`
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := policyConfig(t, url, rules)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	for i, body := range []string{badBashBody, badBashBody2} {
		if rec := serve(g, post(body)); rec.Code != http.StatusForbidden {
			t.Fatalf("request %d of the conversation: status = %d, want 403", i+1, rec.Code)
		}
	}
	if rec := serve(g, post(bodyA)); rec.Code != http.StatusOK {
		t.Fatalf("a clean conversation: status = %d, want 200", rec.Code)
	}
	if f.count() != 1 {
		t.Fatalf("provider calls = %d, want 1 (only the clean conversation)", f.count())
	}

	// The same denial from a Gateway that never saw the first request.
	f2, url2 := newFakeAnthropic(t, loopResponder)
	cfg2 := policyConfig(t, url2, rules)
	g2 := newTracingGateway(t, cfg2, zaptest.NewLogger(t))
	if rec := serve(g2, post(badBashBody2)); rec.Code != http.StatusForbidden || f2.count() != 0 {
		t.Fatalf("fresh gateway: status=%d provider calls=%d, want 403 and none: no state is needed", rec.Code, f2.count())
	}
}

// --- failures ----------------------------------------------------------------------

func TestPolicies_FailuresAre503WithoutShouldRetry(t *testing.T) {
	heavy := `deny contains "heavy" if {
	input.model == "m"
	some a in numbers.range(1, 3000)
	some b in numbers.range(1, 3000)
	some c in numbers.range(1, 3000)
	a + b + c == 0
}`
	tests := []struct {
		name     string
		rules    string
		body     string
		timeout  time.Duration
		wantKind string
	}{
		{"malformed decision", `deny contains 42 if { input.model == "m" }`, policyBody("m"), 0, "malformed_decision"},
		{"body that is not JSON", denyModel("r", "x"), `{"model":`, 0, "input"},
		{"repeated model key", denyModel("r", "x"), `{"model":"a","model":"b","messages":[]}`, 0, "input"},
		{"messages not an array", denyModel("r", "x"), `{"model":"m","messages":"nope"}`, 0, "input"},
		{"evaluation exceeds the limit", heavy, policyBody("m"), time.Millisecond, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, url := newFakeAnthropic(t, loopResponder)
			cfg := policyConfig(t, url, tt.rules)
			if tt.timeout > 0 {
				cfg.Policies.EvalTimeout = tt.timeout
			}
			logger, logs := observed()
			g := newTracingGateway(t, cfg, logger)
			logs.TakeAll()

			rec := serve(g, post(tt.body))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if v, present := rec.Header()["X-Should-Retry"]; present {
				t.Fatalf("X-Should-Retry = %v, want none on a failure", v)
			}
			if f.count() != 0 {
				t.Fatal("the provider received a call whose policy could not be evaluated")
			}
			e := decodeErr(t, rec)
			if e.Error.Type != "gateway_unavailable" || e.Error.Stage != "policies" {
				t.Fatalf("error = %+v, want gateway_unavailable from policies", e.Error)
			}

			req, _ := callRecords(t, cfg.Tracing.Sink.Path)
			if field(req, "outcome", "result") != "failed" {
				t.Fatalf("outcome = %v, want failed", req["outcome"])
			}
			if d := stageEntry(t, req, "policies")["decision"]; d != "failed" {
				t.Fatalf("policies decision = %v, want failed", d)
			}

			var logged bool
			for _, le := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
				text := entryString(le.Message, le.ContextMap())
				if strings.Contains(text, tt.wantKind) && strings.Contains(text, e.Error.RequestID) {
					logged = true
				}
			}
			if !logged {
				t.Fatalf("no error log names the failure kind %q and the call id; logs:\n%s", tt.wantKind, logText(logs))
			}
		})
	}
}

// --- no content in outputs ---------------------------------------------------------

func markerBody(m string) string {
	return `{"model":"claude-sonnet-5","stream":true,"max_tokens":1024,"system":"sys ` + m + `","messages":[` +
		`{"role":"user","content":"please ` + m + `"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_M","name":"bash","input":{"command":"` + m + `"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_M","content":"` + m + ` output"}]}]}`
}

func TestPolicies_NoCallContentInAnyOutput(t *testing.T) {
	fixed := `deny contains "marker-in-prompt" if {
	some m in input.messages
	some p in m.content
	p.type == "text"
	contains(p.text, "` + marker + `")
}
deny contains "marker-in-args" if {
	some c in input.tool_calls
	contains(c.arguments.command, "` + marker + `")
}
deny contains "marker-in-result" if {
	some r in input.tool_results
	contains(r.text, "` + marker + `")
}`
	tests := []struct {
		name     string
		rules    string
		body     string
		wantCode int
		// stripOK is the one substring in which the marker may appear.
		stripOK string
	}{
		{"denial with fixed ids", fixed, markerBody(marker), http.StatusForbidden, ""},
		{"failure: decision shaped from the prompt", `deny contains {"m": input.system} if { true }`, markerBody(marker), http.StatusServiceUnavailable, ""},
		{"failure: body that is not JSON", denyModel("r", "x"), `{"model":"` + marker, http.StatusServiceUnavailable, ""},
		{"failure: repeated key named after the marker", denyModel("r", "x"), `{"model":"a","` + marker + `":1,"` + marker + `":2,"messages":[]}`, http.StatusServiceUnavailable, ""},
		{"operator mistake: id built from an argument, one word", `deny contains sprintf("leak-%s", [c.arguments.command]) if { some c in input.tool_calls }`, markerBody(marker), http.StatusForbidden, "leak-" + marker},
		{"operator mistake: id built from text with spaces is normalized away", `deny contains sprintf("leak %s here", [input.system]) if { true }`, markerBody(marker), http.StatusForbidden, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, url := newFakeAnthropic(t, loopResponder)
			cfg := policyConfig(t, url, tt.rules)
			logger, logs := observed()
			g := newTracingGateway(t, cfg, logger)

			rec := serve(g, post(tt.body))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			// The caller never sees the marker, not even in the permitted id.
			if strings.Contains(rec.Body.String(), marker) || strings.Contains(fmt.Sprint(rec.Header()), marker) {
				t.Fatalf("the response to the caller carries call content: %s", rec.Body.String())
			}
			all := everything(t, cfg.Tracing.Sink.Path, logText(logs), rec)
			if tt.stripOK != "" {
				if !strings.Contains(all, tt.stripOK) {
					t.Logf("note: the id %q was not recorded anywhere", tt.stripOK)
				}
				all = strings.ReplaceAll(all, tt.stripOK, "")
			}
			if strings.Contains(all, marker) {
				t.Fatalf("call content reached a log line, a trace record or a response:\n%s", all)
			}
		})
	}
}

// --- reload ------------------------------------------------------------------------

func TestGateway_ReloadPolicy(t *testing.T) {
	f, url := newFakeAnthropic(t, loopResponder)
	cfg := policyConfig(t, url, denyModel("no-a", "m-a"))
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	if rec := serve(g, post(policyBody("m-a"))); rec.Code != http.StatusForbidden {
		t.Fatalf("before reload: m-a status = %d, want 403", rec.Code)
	}

	writePolicyFile(t, filepath.Dir(cfg.Policies.Path), denyModel("no-b", "m-b"))
	if err := g.ReloadPolicy(); err != nil {
		t.Fatalf("ReloadPolicy: %v", err)
	}
	if rec := serve(g, post(policyBody("m-a"))); rec.Code != http.StatusOK {
		t.Fatalf("after reload: m-a status = %d, want 200", rec.Code)
	}
	if rec := serve(g, post(policyBody("m-b"))); rec.Code != http.StatusForbidden {
		t.Fatalf("after reload: m-b status = %d, want 403", rec.Code)
	}

	// An invalid edit changes nothing for callers and is reported.
	if err := os.WriteFile(cfg.Policies.Path, []byte(regoHead+"deny contains \"x\" if { input.a == }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.ReloadPolicy(); err == nil {
		t.Fatal("ReloadPolicy of an invalid policy = nil, want an error")
	}
	if rec := serve(g, post(policyBody("m-b"))); rec.Code != http.StatusForbidden {
		t.Fatalf("after a failed reload: m-b status = %d, want the previous policy still enforced", rec.Code)
	}
	_ = f
}

func TestGateway_ReloadPolicyWithNoPolicyLogsAndSucceeds(t *testing.T) {
	_, url := newFakeAnthropic(t, loopResponder)
	cfg := tracingGatewayConfig(t, url)
	cfg.Pipeline.Stages.Policies = true
	logger, logs := observed()
	g := newTracingGateway(t, cfg, logger)
	logs.TakeAll()

	if err := g.ReloadPolicy(); err != nil {
		t.Fatalf("ReloadPolicy with no policy = %v, want nil", err)
	}
	var logged bool
	for _, le := range logs.FilterLevelExact(zapcore.InfoLevel).All() {
		if strings.Contains(strings.ToLower(le.Message), "policy") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("no info line says no policy is configured; logs:\n%s", logText(logs))
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestGateway_RunWatchesPolicyFilesWhenEnabled(t *testing.T) {
	_, url := newFakeAnthropic(t, loopResponder)
	cfg := policyConfig(t, url, denyModel("no-a", "m-a"))
	cfg.Policies.Watch = true
	cfg.Server.ListenAddr = freeAddr(t)
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = g.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait() })

	base := "http://" + cfg.Server.ListenAddr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	status := func(model string) int {
		resp, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(policyBody(model)))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if got := status("m-a"); got != http.StatusForbidden {
		t.Fatalf("m-a status = %d, want 403", got)
	}

	time.Sleep(300 * time.Millisecond) // let the watcher arm
	writePolicyFile(t, filepath.Dir(cfg.Policies.Path), denyModel("no-b", "m-b"))
	deadline = time.Now().Add(5 * time.Second)
	for status("m-b") != http.StatusForbidden {
		if time.Now().After(deadline) {
			t.Fatal("the edited policy never took effect: Run must start the watcher when policies.watch is on")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := status("m-a"); got != http.StatusOK {
		t.Fatalf("m-a status = %d after the watched reload, want 200", got)
	}
}

// --- the stage itself, against a real engine ----------------------------------------

type captureSink struct {
	mu      sync.Mutex
	records []map[string]any
}

func (s *captureSink) Write(r trace.Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, m)
	return nil
}

func (s *captureSink) request(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r["record"] == "request" {
			return r
		}
	}
	t.Fatal("no request record captured")
	return nil
}

// fixedTraceStage stands in for tracing: it reports a session the way the
// tracing stage would.
type fixedTraceStage struct{ session trace.Session }

func (fixedTraceStage) Name() string { return "tracing" }
func (s fixedTraceStage) Request(context.Context, *pipeline.Call) (pipeline.Decision, error) {
	return pipeline.AllowTraced(trace.RequestInfo{Session: s.session}, nil), nil
}

func realEngine(t *testing.T, rules string) *policy.Engine {
	t.Helper()
	e, err := policy.New(config.PoliciesConfig{
		Path:        writePolicyFile(t, t.TempDir(), rules),
		WatchSettle: time.Second,
		EvalTimeout: 5 * time.Second,
	}, "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return e
}

func strptr(s string) *string { return &s }

func TestPoliciesStage_AgainstARealEngine(t *testing.T) {
	const sessionRule = `deny contains "has-session" if { input.session.id == "s-claimed" }`
	claimed := trace.Session{ID: strptr("s-claimed"), Status: "claimed"}
	invalid := trace.Session{ID: strptr("s-claimed"), Status: "invalid"}
	absent := trace.Session{Status: "absent"}

	tests := []struct {
		name       string
		stages     func(eng *policy.Engine) []pipeline.Stage
		wantStatus int
	}{
		{"claimed session reaches the rules", func(e *policy.Engine) []pipeline.Stage {
			return []pipeline.Stage{fixedTraceStage{claimed}, &policiesStage{engine: e}}
		}, http.StatusForbidden},
		{"an invalid session is not passed", func(e *policy.Engine) []pipeline.Stage {
			return []pipeline.Stage{fixedTraceStage{invalid}, &policiesStage{engine: e}}
		}, http.StatusOK},
		{"an absent session gives no session key", func(e *policy.Engine) []pipeline.Stage {
			return []pipeline.Stage{fixedTraceStage{absent}, &policiesStage{engine: e}}
		}, http.StatusOK},
		{"tracing off (c.Trace is nil) gives no session key", func(e *policy.Engine) []pipeline.Stage {
			return []pipeline.Stage{&policiesStage{engine: e}}
		}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := realEngine(t, sessionRule)
			p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, tt.stages(eng)...)
			var forwarded bool
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded = true })
			rec := httptest.NewRecorder()
			p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(policyBody("m"))))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if forwarded != (tt.wantStatus == http.StatusOK) {
				t.Fatalf("forwarded = %v with status %d", forwarded, rec.Code)
			}
		})
	}
}

func TestPoliciesStage_DenyReasonCarriesOnlyIdsAndRevision(t *testing.T) {
	eng := realEngine(t, `deny contains "b-rule" if { input.model == "m" }
deny contains "a-rule" if { input.model == "m" }`)
	logger, logs := observed()
	sink := &captureSink{}
	p := pipeline.New(logger, 1<<20, sink, &policiesStage{engine: eng})
	rec := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(policyBody("m"))))

	if rec.Code != http.StatusForbidden || decodeErr(t, rec).Error.Message != genericDeny {
		t.Fatalf("status=%d body=%s, want 403 with the generic message", rec.Code, rec.Body.String())
	}
	var reason string
	for _, le := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if r, ok := le.ContextMap()["reason"].(string); ok {
			reason = r
		}
	}
	want := regexp.MustCompile(`^rules=a-rule,b-rule revision=` + revisionRe + `$`)
	if !want.MatchString(reason) {
		t.Fatalf("denial reason = %q, want %q", reason, want)
	}
	if got := field(sink.request(t), "outcome", "reason"); got != reason {
		t.Fatalf("record outcome.reason = %v, want the logged reason %q", got, reason)
	}
}

func TestPoliciesStage_EngineErrorBecomesTheUnavailableDenialThroughThePipeline(t *testing.T) {
	eng := realEngine(t, `deny contains "r" if { input.model == "never" }`)
	sink := &captureSink{}
	p := pipeline.New(zaptest.NewLogger(t), 1<<20, sink, &policiesStage{engine: eng})
	rec := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	e := decodeErr(t, rec)
	if e.Error.Type != "gateway_unavailable" || e.Error.Stage != "policies" {
		t.Fatalf("error = %+v, want gateway_unavailable from policies", e.Error)
	}
	if _, present := rec.Header()["X-Should-Retry"]; present {
		t.Fatal("a failure must not carry X-Should-Retry")
	}
	if d := stageEntry(t, sink.request(t), "policies")["decision"]; d != "failed" {
		t.Fatalf("policies decision = %v, want failed", d)
	}
}

// --- end to end --------------------------------------------------------------------

func TestPolicies_EndToEnd(t *testing.T) {
	const upstreamBody = `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`
	f, url := newFakeAnthropic(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	})
	cfg := policyConfig(t, url, denyModel("no-opus", "claude-opus-4"))
	g := newTracingGateway(t, cfg, zaptest.NewLogger(t))
	srv := httptest.NewServer(g.server.Handler)
	t.Cleanup(srv.Close)

	send := func(model string) (*http.Response, string) {
		resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"`+model+`","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	// An allowed call is forwarded and its response relayed byte-identical.
	resp, body := send("claude-haiku-4-5")
	if resp.StatusCode != http.StatusOK || body != upstreamBody {
		t.Fatalf("allowed call: status=%d body=%q, want 200 and the provider's body unchanged", resp.StatusCode, body)
	}
	if f.count() != 1 {
		t.Fatalf("provider calls = %d, want 1", f.count())
	}
	allowedReq, allowedResp := callRecords(t, cfg.Tracing.Sink.Path)
	if allowedResp == nil || field(allowedResp, "outcome", "result") != "delivered" {
		t.Fatalf("allowed call response record = %v, want a delivered response", allowedResp)
	}
	if st := stageEntry(t, allowedReq, "policies"); st["decision"] != "allowed" {
		t.Fatalf("policies decision = %v, want allowed", st["decision"])
	}

	// A denied call: 403, do-not-retry, generic body, never forwarded.
	resp, body = send("claude-opus-4")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied call status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Should-Retry"); got != "false" {
		t.Fatalf("X-Should-Retry = %q, want false", got)
	}
	if strings.Contains(body, "no-opus") {
		t.Fatalf("the denial body names the rule: %s", body)
	}
	var env errBody
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Type != "gateway_denied" || env.Error.Stage != "policies" || env.Error.Message != genericDeny {
		t.Fatalf("denial body = %s (err %v), want the generic gateway_denied from policies", body, err)
	}
	if f.count() != 1 {
		t.Fatalf("provider calls = %d after a denied call, want still 1", f.count())
	}

	req, resp2 := callRecords(t, cfg.Tracing.Sink.Path)
	if resp2 != nil {
		t.Fatal("a denied call has a response record")
	}
	st := stageEntry(t, req, "policies")
	if st["decision"] != "denied" {
		t.Fatalf("policies decision = %v, want denied", st["decision"])
	}
	detail := st["detail"].(map[string]any)
	if rev, _ := detail["revision"].(string); !revisionPattern.MatchString(rev) {
		t.Fatalf("detail.revision = %v, want 12 hex characters", detail["revision"])
	}
	if rules, _ := detail["rules"].([]any); len(rules) != 1 || rules[0] != "no-opus" {
		t.Fatalf("detail.rules = %v, want [no-opus]", detail["rules"])
	}
}

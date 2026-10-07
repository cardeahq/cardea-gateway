package policy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

const regoHeader = "package cardea.policy\n\n"

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// --- helpers -----------------------------------------------------------------------

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func cfgFor(path string, data ...string) config.PoliciesConfig {
	return config.PoliciesConfig{
		Path:        path,
		Data:        data,
		WatchSettle: 50 * time.Millisecond,
		EvalTimeout: 5 * time.Second,
	}
}

// newEngine writes rules (without the package line) to a temp policy file
// and loads an engine from it.
func newEngine(t *testing.T, rules string) (*Engine, string) {
	t.Helper()
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+rules)
	e, err := New(cfgFor(path), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e, path
}

func observedLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

func entryText(e observer.LoggedEntry) string {
	return fmt.Sprintf("%s %v", e.Message, e.ContextMap())
}

func allLogText(logs *observer.ObservedLogs) string {
	var b strings.Builder
	for _, e := range logs.All() {
		b.WriteString(entryText(e))
		b.WriteByte('\n')
	}
	return b.String()
}

func msgReq(body string) Request {
	return Request{
		CallID:  "ak_0123456789abcdef",
		Method:  http.MethodPost,
		Path:    "/v1/messages",
		Headers: http.Header{},
		Body:    []byte(body),
	}
}

func evalKind(t *testing.T, err error) string {
	t.Helper()
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("error = %v (%T), want *EvalError", err, err)
	}
	return ee.Kind
}

// slowRule fires after a regex with captures scans a very large system
// prompt: a single built-in call OPA cannot interrupt part-way.
const slowRule = `deny contains "slow" if {
	regex.find_all_string_submatch_n("(x*)(x*)(x*)(x*)(x*)y", input.system, 1)
}
`

func slowBody(mib int) string {
	return `{"model":"m","system":"` + strings.Repeat("x", mib<<20) + `","messages":[]}`
}

// --- decisions ---------------------------------------------------------------------

func TestEvaluate_Decisions(t *testing.T) {
	const bashLoop = `{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"%s"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`

	tests := []struct {
		name  string
		rules string
		req   func() Request
		want  []string
	}{
		{
			name:  "deny by model",
			rules: `deny contains "bad-model" if { input.model == "claude-opus-4" }`,
			req:   func() Request { return msgReq(`{"model":"claude-opus-4","messages":[]}`) },
			want:  []string{"bad-model"},
		},
		{
			name:  "other model is allowed",
			rules: `deny contains "bad-model" if { input.model == "claude-opus-4" }`,
			req:   func() Request { return msgReq(`{"model":"claude-haiku-4-5","messages":[]}`) },
		},
		{
			name: "deny by agent",
			rules: `deny contains "beta-small-model-only" if {
				input.agent.id == "agent-beta"
				input.model != "claude-haiku-4-5"
			}`,
			req: func() Request {
				r := msgReq(`{"model":"claude-sonnet-5","messages":[]}`)
				r.AgentID = "agent-beta"
				return r
			},
			want: []string{"beta-small-model-only"},
		},
		{
			name: "a different agent is allowed",
			rules: `deny contains "beta-small-model-only" if {
				input.agent.id == "agent-beta"
				input.model != "claude-haiku-4-5"
			}`,
			req: func() Request {
				r := msgReq(`{"model":"claude-sonnet-5","messages":[]}`)
				r.AgentID = "agent-alpha"
				return r
			},
		},
		{
			name: "deny by tool offered",
			rules: `deny contains "no-delete-tool" if {
				some t in input.tools
				t.name == "delete_database"
			}`,
			req: func() Request {
				return msgReq(`{"model":"m","messages":[],"tools":[{"name":"bash","input_schema":{}},{"name":"delete_database","input_schema":{}}]}`)
			},
			want: []string{"no-delete-tool"},
		},
		{
			name: "tool not offered is allowed",
			rules: `deny contains "no-delete-tool" if {
				some t in input.tools
				t.name == "delete_database"
			}`,
			req: func() Request {
				return msgReq(`{"model":"m","messages":[],"tools":[{"name":"bash","input_schema":{}}]}`)
			},
		},
		{
			name: "deny by tool call name and argument value",
			rules: `deny contains "no-rm-rf" if {
				some c in input.tool_calls
				c.name == "bash"
				contains(c.arguments.command, "rm -rf")
			}`,
			req:  func() Request { return msgReq(fmt.Sprintf(bashLoop, "rm -rf /")) },
			want: []string{"no-rm-rf"},
		},
		{
			name: "same tool with a harmless argument is allowed",
			rules: `deny contains "no-rm-rf" if {
				some c in input.tool_calls
				c.name == "bash"
				contains(c.arguments.command, "rm -rf")
			}`,
			req: func() Request { return msgReq(fmt.Sprintf(bashLoop, "ls")) },
		},
		{
			name: "deny by header",
			rules: `deny contains "red-team" if {
				"red" in input.call.headers["x-team"]
			}`,
			req: func() Request {
				r := msgReq(`{"model":"m","messages":[]}`)
				r.Headers.Set("X-Team", "red")
				return r
			},
			want: []string{"red-team"},
		},
		{
			name: "header with another value is allowed",
			rules: `deny contains "red-team" if {
				"red" in input.call.headers["x-team"]
			}`,
			req: func() Request {
				r := msgReq(`{"model":"m","messages":[]}`)
				r.Headers.Set("X-Team", "blue")
				return r
			},
		},
		{
			name: "a rule reading Authorization does not match: the header is stripped",
			rules: `deny contains "auth-seen" if { input.call.headers.authorization }
deny contains "key-seen" if { input.call.headers["x-api-key"] }
deny contains "cookie-seen" if { input.call.headers.cookie }`,
			req: func() Request {
				r := msgReq(`{"model":"m","messages":[]}`)
				r.Headers.Set("Authorization", "Bearer secret")
				r.Headers.Set("X-Api-Key", "secret")
				r.Headers.Set("Cookie", "a=b")
				return r
			},
		},
		{
			name:  "deny by operation",
			rules: `deny contains "no-count" if { input.operation.name == "count_tokens" }`,
			req: func() Request {
				r := msgReq(`{"model":"m","messages":[]}`)
				r.Path = "/v1/messages/count_tokens"
				return r
			},
			want: []string{"no-count"},
		},
		{
			name:  "other operations are allowed",
			rules: `deny contains "no-count" if { input.operation.name == "count_tokens" }`,
			req:   func() Request { return msgReq(`{"model":"m","messages":[]}`) },
		},
		{
			name:  "deny on a non-conversation path by raw path",
			rules: `deny contains "no-models" if { input.operation.path == "/v1/models" }`,
			req: func() Request {
				r := msgReq(`not json at all`)
				r.Method = http.MethodGet
				r.Path = "/v1/models"
				return r
			},
			want: []string{"no-models"},
		},
		{
			name: "time built-ins are available",
			rules: `deny contains "time-ok" if {
				time.weekday(time.now_ns()) in {"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
			}`,
			req:  func() Request { return msgReq(`{"model":"m","messages":[]}`) },
			want: []string{"time-ok"},
		},
		{
			name:  "absent agent does not match",
			rules: `deny contains "agent-rule" if { input.agent.id == "agent-beta" }`,
			req:   func() Request { return msgReq(`{"model":"m","messages":[]}`) },
		},
		{
			name:  "absent session does not match",
			rules: `deny contains "session-rule" if { input.session.id == "s-1" }`,
			req:   func() Request { return msgReq(`{"model":"m","messages":[]}`) },
		},
		{
			name:  "claimed session is visible to rules",
			rules: `deny contains "session-rule" if { input.session.id == "s-1" }`,
			req: func() Request {
				r := msgReq(`{"model":"m","messages":[]}`)
				r.SessionID = "s-1"
				return r
			},
			want: []string{"session-rule"},
		},
		{
			name:  "absent system prompt does not match",
			rules: `deny contains "system-rule" if { contains(input.system, "x") }`,
			req:   func() Request { return msgReq(`{"model":"m","messages":[]}`) },
		},
		{
			name:  "a conversation key on a non-conversation operation does not match",
			rules: `deny contains "model-rule" if { input.model == "m" }`,
			req: func() Request {
				r := msgReq(`{"model":"m"}`)
				r.Path = "/v1/models"
				return r
			},
		},
		{
			name:  "empty tool_calls is present and countable",
			rules: `deny contains "no-calls" if { count(input.tool_calls) == 0 }`,
			req:   func() Request { return msgReq(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`) },
			want:  []string{"no-calls"},
		},
		{
			name: "deny by credential pattern in conversation text",
			rules: `deny contains "credential-in-prompt" if {
				some m in input.messages
				some part in m.content
				part.type == "text"
				regex.match("sk-[A-Za-z0-9]{10,}", part.text)
			}`,
			req: func() Request {
				return msgReq(`{"model":"m","messages":[{"role":"user","content":"my key is sk-abcdefghijklmnop"}]}`)
			},
			want: []string{"credential-in-prompt"},
		},
		{
			name: "three rules firing at once are all returned, sorted",
			rules: `deny contains "zeta" if { input.model == "m" }
deny contains "alpha" if { input.operation.name == "messages" }
deny contains "mid.rule" if { count(input.tools) == 0 }`,
			req:  func() Request { return msgReq(`{"model":"m","messages":[]}`) },
			want: []string{"alpha", "mid.rule", "zeta"},
		},
		{
			name: "a rule that errors on a missing field is undefined, not a failure",
			rules: `deny contains "regex-on-missing" if { regex.match("a", input.system) }
deny contains "other" if { input.model == "m" }`,
			req:  func() Request { return msgReq(`{"model":"m","messages":[]}`) },
			want: []string{"other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newEngine(t, tt.rules)
			res, err := e.Evaluate(context.Background(), tt.req())
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !slices.Equal(res.Rules, tt.want) && !(len(res.Rules) == 0 && len(tt.want) == 0) {
				t.Fatalf("Rules = %v, want %v", res.Rules, tt.want)
			}
			if !revisionPattern.MatchString(res.Revision) {
				t.Fatalf("Revision = %q, want 12 hex characters", res.Revision)
			}
			if res.Revision != e.Revision() {
				t.Fatalf("Result.Revision = %q, Engine.Revision() = %q, want equal", res.Revision, e.Revision())
			}
		})
	}
}

func TestEvaluate_DataDrivenRule(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+`deny contains "blocked-model" if { input.model in data.blocked_models }`)
	data := writeFile(t, dir, "data.json", `{"blocked_models":["m-bad"]}`)
	e, err := New(cfgFor(path, data), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m-bad","messages":[]}`))
	if err != nil || !slices.Equal(res.Rules, []string{"blocked-model"}) {
		t.Fatalf("m-bad: rules=%v err=%v, want [blocked-model]", res.Rules, err)
	}
	res, err = e.Evaluate(context.Background(), msgReq(`{"model":"m-good","messages":[]}`))
	if err != nil || len(res.Rules) != 0 {
		t.Fatalf("m-good: rules=%v err=%v, want allow", res.Rules, err)
	}
}

func TestEvaluate_YAMLDataIsMergedAtTheRootOfData(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+`deny contains "limit" if { data.limits.max_tokens < input.parameters.max_tokens }`)
	data := writeFile(t, dir, "limits.yaml", "limits:\n  max_tokens: 100\n")
	e, err := New(cfgFor(path, data), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","max_tokens":500,"messages":[]}`))
	if err != nil || !slices.Equal(res.Rules, []string{"limit"}) {
		t.Fatalf("rules=%v err=%v, want [limit]", res.Rules, err)
	}
}

// --- no policy ---------------------------------------------------------------------

func TestNew_NoPolicyAllowsEverythingWithoutBuildingInput(t *testing.T) {
	e, err := New(config.PoliciesConfig{EvalTimeout: time.Second, WatchSettle: time.Second}, "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New with no path: %v", err)
	}
	if e.Revision() != "" {
		t.Fatalf("Revision = %q, want empty with no policy", e.Revision())
	}
	// A body that would be an input error under a policy must not matter:
	// no input is built.
	req := msgReq(`{ not json`)
	req.Path = "/v1/messages/count_tokens"
	res, err := e.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatalf("Evaluate with no policy: %v", err)
	}
	if res.Revision != "" || len(res.Rules) != 0 {
		t.Fatalf("Result = %+v, want the zero Result", res)
	}
}

func TestEngine_NoPolicyReloadIsHarmless(t *testing.T) {
	e, err := New(config.PoliciesConfig{EvalTimeout: time.Second, WatchSettle: time.Second}, "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Reload("signal"); err != nil {
		t.Fatalf("Reload with no policy = %v, want nil", err)
	}
	if e.Revision() != "" {
		t.Fatalf("Revision = %q after reload with no policy, want empty", e.Revision())
	}
}

// --- failures ----------------------------------------------------------------------

func TestEvaluate_InputErrorsFailClosedWithKindInput(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"messages body that is not JSON", "/v1/messages", `{"model":`},
		{"count_tokens body that is not JSON", "/v1/messages/count_tokens", `nope`},
		{"empty messages body", "/v1/messages", ``},
		{"repeated model at the top level", "/v1/messages", `{"model":"a","model":"b","messages":[]}`},
		{"messages is not an array", "/v1/messages", `{"model":"m","messages":"hello"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newEngine(t, `deny contains "x" if { input.model == "never" }`)
			req := msgReq(tt.body)
			req.Path = tt.path
			_, err := e.Evaluate(context.Background(), req)
			if err == nil {
				t.Fatal("Evaluate = nil error, want an input failure")
			}
			if k := evalKind(t, err); k != "input" {
				t.Fatalf("Kind = %q, want input", k)
			}
		})
	}
}

func TestEvaluate_MalformedDecision(t *testing.T) {
	tests := []struct {
		name  string
		rules string
	}{
		{"set of numbers", `deny contains 42 if { input.model == "m" }`},
		{"set of objects carrying input", `deny contains {"m": input.system} if { input.model == "m" }`},
	}
	const marker = "MARKER-SYSTEM-TEXT-9f3a"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newEngine(t, tt.rules)
			_, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","system":"`+marker+`","messages":[]}`))
			if err == nil {
				t.Fatal("Evaluate = nil error, want malformed_decision")
			}
			if k := evalKind(t, err); k != "malformed_decision" {
				t.Fatalf("Kind = %q, want malformed_decision", k)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("EvalError.Error() = %q carries an input value", err.Error())
			}
		})
	}
}

func TestEvaluate_TimeoutOnExpensiveRuleIsKindTimeout(t *testing.T) {
	// An interruptible but very expensive rule: a huge cross product.
	e := timeoutEngine(t, `deny contains "heavy" if {
	input.model == "m"
	some a in numbers.range(1, 3000)
	some b in numbers.range(1, 3000)
	some c in numbers.range(1, 3000)
	a + b + c == 0
}`, time.Millisecond)
	start := time.Now()
	_, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err == nil {
		t.Fatal("Evaluate = nil error, want timeout")
	}
	if k := evalKind(t, err); k != "timeout" {
		t.Fatalf("Kind = %q, want timeout", k)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Evaluate took %v with a 1ms limit", d)
	}
}

func timeoutEngine(t *testing.T, rules string, limit time.Duration) *Engine {
	t.Helper()
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+rules)
	cfg := cfgFor(path)
	cfg.EvalTimeout = limit
	e, err := New(cfg, "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestEvaluate_UninterruptibleBuiltinStillTimesOutAtTheLimit(t *testing.T) {
	body := slowBody(4)

	// Control: how long does the rule take when allowed to finish?
	control := timeoutEngine(t, slowRule, 60*time.Second)
	start := time.Now()
	res, err := control.Evaluate(context.Background(), msgReq(body))
	full := time.Since(start)
	if err != nil || !slices.Equal(res.Rules, []string{"slow"}) {
		t.Fatalf("control: rules=%v err=%v, want [slow]", res.Rules, err)
	}
	if full < 300*time.Millisecond {
		t.Skipf("the regex finished in %v; too fast on this machine to prove the limit holds for a built-in that cannot be interrupted", full)
	}

	e := timeoutEngine(t, slowRule, 20*time.Millisecond)
	start = time.Now()
	_, err = e.Evaluate(context.Background(), msgReq(body))
	took := time.Since(start)
	if err == nil {
		t.Fatal("Evaluate = nil error, want timeout")
	}
	if k := evalKind(t, err); k != "timeout" {
		t.Fatalf("Kind = %q, want timeout", k)
	}
	// Input building is part of the call; it must be far cheaper than the
	// regex, so answering well before the full run proves the caller was not
	// held until the built-in returned.
	if took > full/2 {
		t.Fatalf("Evaluate returned after %v with a 20ms limit; the rule alone takes %v: the limit did not hold", took, full)
	}
}

func TestEvaluate_CallerCancellationIsKindInternal(t *testing.T) {
	e := timeoutEngine(t, slowRule, 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err := e.Evaluate(ctx, msgReq(slowBody(4)))
	if err == nil {
		t.Skip("evaluation finished before the cancellation; machine too fast")
	}
	if k := evalKind(t, err); k != "internal" {
		t.Fatalf("Kind = %q, want internal for the caller's own cancellation", k)
	}
}

func TestEvaluate_TooManyAbandonedEvaluationsIsOverloaded(t *testing.T) {
	const abandoned = 16 // maxAbandoned
	e := timeoutEngine(t, `deny contains "r" if { false }`, 20*time.Millisecond)

	// Stand in for a built-in OPA cannot interrupt: it ignores the context
	// and returns only when released.
	release := make(chan struct{})
	e.eval = func(ctx context.Context, q rego.PreparedEvalQuery, input map[string]any) (rego.ResultSet, error) {
		<-release
		return defaultEval(ctx, q, input)
	}
	released := false
	releaseAll := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseAll)

	var wg sync.WaitGroup
	kinds := make([]string, abandoned)
	for i := range abandoned {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
			var ee *EvalError
			if errors.As(err, &ee) {
				kinds[i] = ee.Kind
			}
		}()
	}
	wg.Wait()
	for i, k := range kinds {
		if k != "timeout" {
			t.Fatalf("evaluation %d ended with kind %q, want timeout", i, k)
		}
	}

	start := time.Now()
	_, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err == nil {
		t.Fatalf("Evaluate = nil error with %d evaluations still running, want overloaded", abandoned)
	}
	if k := evalKind(t, err); k != "overloaded" {
		t.Fatalf("Kind = %q with %d evaluations still running, want overloaded", k, abandoned)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("overloaded took %v, want an immediate failure", d)
	}

	// The count falls back when the abandoned evaluations end.
	releaseAll()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
		if err == nil {
			break
		}
		if k := evalKind(t, err); k != "overloaded" {
			t.Fatalf("Kind = %q while draining, want overloaded or success", k)
		}
		if time.Now().After(deadline) {
			t.Fatal("still overloaded after the abandoned evaluations were released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvalError_ErrorCarriesNoInputValue(t *testing.T) {
	const marker = "MARKER-PROMPT-TEXT-77c1"
	e := timeoutEngine(t, `deny contains {"leak": input.system} if { input.model == "m" }`, time.Second)
	_, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","system":"`+marker+`","messages":[]}`))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("Error() = %q contains an input value", err.Error())
	}
	var ee *EvalError
	if errors.As(err, &ee) && strings.Contains(ee.Kind, marker) {
		t.Fatalf("Kind = %q contains an input value", ee.Kind)
	}
}

func TestEvaluate_RuleIDNormalization(t *testing.T) {
	const marker = "MARKER-TEXT-with spaces-31ab"
	tests := []struct {
		name  string
		rules string
		want  []string
	}{
		{"valid ids are kept", `deny contains "a.b_c:d-e" if { true }
deny contains "0start" if { true }`, []string{"0start", "a.b_c:d-e"}},
		{"id with a newline is replaced", `deny contains "line1\nline2" if { true }`, []string{"invalid-rule-id"}},
		{"id with spaces is replaced", `deny contains sprintf("%s", ["` + marker + `"]) if { true }`, []string{"invalid-rule-id"}},
		{"id starting with a dash is replaced", `deny contains "-bad" if { true }`, []string{"invalid-rule-id"}},
		{"empty id is replaced", `deny contains "" if { true }`, []string{"invalid-rule-id"}},
		{"id of 64 characters is kept", `deny contains "` + strings.Repeat("a", 64) + `" if { true }`, []string{strings.Repeat("a", 64)}},
		{"id of 65 characters is replaced", `deny contains "` + strings.Repeat("a", 65) + `" if { true }`, []string{"invalid-rule-id"}},
		{"several invalid ids collapse to one, still sorted with the valid ones", `deny contains "bad one" if { true }
deny contains "bad\ntwo" if { true }
deny contains "ok" if { true }`, []string{"invalid-rule-id", "ok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newEngine(t, tt.rules)
			res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if !slices.Equal(res.Rules, tt.want) {
				t.Fatalf("Rules = %q, want %q (the call is denied either way)", res.Rules, tt.want)
			}
			for _, r := range res.Rules {
				if strings.Contains(r, marker) {
					t.Fatalf("Rules carries free text: %q", r)
				}
			}
		})
	}
}

func TestEvaluate_ManyFiredRulesAreAllRecorded(t *testing.T) {
	var b strings.Builder
	var want []string
	for i := range 40 {
		id := fmt.Sprintf("rule-%02d", i)
		fmt.Fprintf(&b, "deny contains %q if { true }\n", id)
		want = append(want, id)
	}
	e, _ := newEngine(t, b.String())
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !slices.Equal(res.Rules, want) {
		t.Fatalf("Rules = %v, want all %d ids (no cap)", res.Rules, len(want))
	}
}

// --- cached ------------------------------------------------------------------------

func TestEvaluate_DecidesFromMemoryUntilReload(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+`deny contains "blocked" if { input.model in data.blocked }`)
	data := writeFile(t, dir, "data.json", `{"blocked":["m-bad"]}`)
	e, err := New(cfgFor(path, data), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rev := e.Revision()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "data.json", `{"blocked":[]}`)

	for i := range 1000 {
		res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m-bad","messages":[]}`))
		if err != nil {
			t.Fatalf("Evaluate #%d: %v", i, err)
		}
		if !slices.Equal(res.Rules, []string{"blocked"}) || res.Revision != rev {
			t.Fatalf("Evaluate #%d = %+v, want the loaded policy (blocked, %s) whatever is on disk", i, res, rev)
		}
	}

	if err := e.Reload("signal"); err == nil {
		t.Fatal("Reload with the policy file deleted = nil, want an error")
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m-bad","messages":[]}`))
	if err != nil || !slices.Equal(res.Rules, []string{"blocked"}) || res.Revision != rev {
		t.Fatalf("after a failed reload: %+v err=%v, want the old policy still in force", res, err)
	}
}

// --- reload ------------------------------------------------------------------------

func TestReload_ValidEditSwapsAndChangesRevision(t *testing.T) {
	logger, logs := observedLogger()
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+`deny contains "old" if { input.model == "m" }`)
	e, err := New(cfgFor(path), "anthropic", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	oldRev := e.Revision()

	writeFile(t, filepath.Dir(path), "policy.rego", regoHeader+`deny contains "new" if { input.model == "m" }`)
	if err := e.Reload("signal"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	newRev := e.Revision()
	if newRev == oldRev || !revisionPattern.MatchString(newRev) {
		t.Fatalf("revision old=%q new=%q, want a different 12-hex revision", oldRev, newRev)
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || !slices.Equal(res.Rules, []string{"new"}) || res.Revision != newRev {
		t.Fatalf("after reload: %+v err=%v, want [new] under %s", res, err, newRev)
	}

	var found bool
	for _, le := range logs.FilterLevelExact(zapcore.InfoLevel).All() {
		text := entryText(le)
		if strings.Contains(text, oldRev) && strings.Contains(text, newRev) && strings.Contains(text, "signal") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no info log names the old revision, the new revision and the trigger; logs:\n%s", allLogText(logs))
	}
}

func TestReload_InvalidEditKeepsOldPolicyAndReturnsError(t *testing.T) {
	logger, logs := observedLogger()
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+`deny contains "old" if { input.model == "m" }`)
	e, err := New(cfgFor(path), "anthropic", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rev := e.Revision()

	tests := []struct {
		name string
		src  string
	}{
		{"syntax error", regoHeader + "\ndeny contains \"x\" if { input.a == }\n"},
		{"forbidden built-in", regoHeader + `deny contains "x" if { http.send({"method": "get", "url": "http://127.0.0.1:1"}) }`},
		{"typo in the package name", "package cardea.polcy\n\ndeny contains \"x\" if { true }\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeFile(t, filepath.Dir(path), "policy.rego", tt.src)
			err := e.Reload("signal")
			if err == nil {
				t.Fatal("Reload of an invalid policy = nil, want an error")
			}
			if e.Revision() != rev {
				t.Fatalf("Revision = %q after a failed reload, want %q", e.Revision(), rev)
			}
			res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
			if err != nil || !slices.Equal(res.Rules, []string{"old"}) || res.Revision != rev {
				t.Fatalf("after failed reload: %+v err=%v, want the old policy", res, err)
			}
		})
	}

	var errLogged bool
	for _, le := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		text := entryText(le)
		if strings.Contains(text, "signal") && strings.Contains(text, rev) {
			errLogged = true
		}
	}
	if !errLogged {
		t.Fatalf("no error log names the trigger and the revision in force; logs:\n%s", allLogText(logs))
	}
}

func TestReload_UnchangedFilesAreANoOp(t *testing.T) {
	logger, logs := observedLogger()
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+`deny contains "r" if { input.model == "m" }`)
	e, err := New(cfgFor(path), "anthropic", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rev := e.Revision()
	logs.TakeAll()

	if err := e.Reload("signal"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if e.Revision() != rev {
		t.Fatalf("Revision = %q, want unchanged %q", e.Revision(), rev)
	}
	for _, le := range logs.All() {
		if le.Level > zapcore.DebugLevel {
			t.Fatalf("a no-change reload logged at %s: %s; want debug", le.Level, entryText(le))
		}
	}
	if logs.Len() == 0 {
		t.Fatal("a no-change reload logged nothing; want a debug line")
	}
}

func TestReload_DeletedPolicyFailsAndDoesNotEmptyThePolicy(t *testing.T) {
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+`deny contains "r" if { input.model == "m" }`)
	e, err := New(cfgFor(path), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rev := e.Revision()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := e.Reload("watch"); err == nil {
		t.Fatal("Reload with the policy deleted = nil, want an error")
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || !slices.Equal(res.Rules, []string{"r"}) || res.Revision != rev {
		t.Fatalf("after deleting the policy: %+v err=%v, want enforcement unchanged", res, err)
	}
}

func TestReload_DataEditChangesDecision(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+`deny contains "blocked" if { input.model in data.blocked }`)
	data := writeFile(t, dir, "data.json", `{"blocked":[]}`)
	e, err := New(cfgFor(path, data), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if res, _ := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`)); len(res.Rules) != 0 {
		t.Fatalf("before edit: %v, want allow", res.Rules)
	}
	writeFile(t, dir, "data.json", `{"blocked":["m"]}`)
	if err := e.Reload("signal"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil || !slices.Equal(res.Rules, []string{"blocked"}) {
		t.Fatalf("after data edit and reload: %v err=%v, want [blocked]", res.Rules, err)
	}
}

func TestReload_EvaluationInFlightFinishesUnderTheOldSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+`deny contains "old" if { true }`)
	cfg := cfgFor(path)
	cfg.EvalTimeout = 60 * time.Second
	e, err := New(cfg, "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	oldRev := e.Revision()

	// Hold the evaluation inside the engine, after it has picked its
	// snapshot, until the reload is done.
	inFlight := make(chan struct{})
	release := make(chan struct{})
	e.eval = func(ctx context.Context, q rego.PreparedEvalQuery, input map[string]any) (rego.ResultSet, error) {
		close(inFlight)
		<-release
		return defaultEval(ctx, q, input)
	}

	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
		done <- out{res, err}
	}()
	<-inFlight

	writeFile(t, dir, "policy.rego", regoHeader+`deny contains "new" if { true }`)
	if err := e.Reload("signal"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if e.Revision() == oldRev {
		t.Fatal("revision did not change after reload")
	}
	close(release)
	o := <-done
	if o.err != nil {
		t.Fatalf("in-flight Evaluate: %v", o.err)
	}
	if !slices.Equal(o.res.Rules, []string{"old"}) || o.res.Revision != oldRev {
		t.Fatalf("in-flight Evaluate = %+v, want [old] under %s: it must finish under the policy it started with", o.res, oldRev)
	}
}

func TestReload_ConcurrentWithEvaluate(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "policy.rego", regoHeader+`deny contains "v0" if { input.model == "m" }`)
	e, err := New(cfgFor(path), "anthropic", zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
				if err != nil {
					t.Errorf("Evaluate during reloads: %v", err)
					return
				}
				// Never a mixture: exactly one rule, from one version.
				if len(res.Rules) != 1 || !strings.HasPrefix(res.Rules[0], "v") {
					t.Errorf("Rules = %v, want exactly one versioned id", res.Rules)
					return
				}
			}
		}()
	}
	for i := 1; i <= 30; i++ {
		writeFile(t, dir, "policy.rego", regoHeader+fmt.Sprintf(`deny contains "v%d" if { input.model == "m" }`, i))
		if err := e.Reload("signal"); err != nil {
			t.Errorf("Reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// --- panic containment -------------------------------------------------------------

const panicMarker = "SECRET-PANIC-MARKER-7f3a"

func TestEvaluate_PanicInEvaluationIsRecoveredAsInternal(t *testing.T) {
	path := writeFile(t, t.TempDir(), "policy.rego", regoHeader+`deny contains "r" if { input.model == "m" }`)
	logger, logs := observedLogger()
	e, err := New(cfgFor(path), "anthropic", logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	realEval := e.eval
	before := e.abandoned.Load()

	e.eval = func(context.Context, rego.PreparedEvalQuery, map[string]any) (rego.ResultSet, error) {
		panic("boom " + panicMarker)
	}
	_, err = e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err == nil {
		t.Fatal("Evaluate = nil error, want *EvalError{internal}")
	}
	if k := evalKind(t, err); k != "internal" {
		t.Fatalf("Kind = %q, want internal", k)
	}
	if strings.Contains(err.Error(), panicMarker) {
		t.Fatalf("error text %q contains the panic value", err.Error())
	}
	if strings.Contains(allLogText(logs), panicMarker) {
		t.Fatalf("log output contains the panic value:\n%s", allLogText(logs))
	}
	if got := e.abandoned.Load(); got != before {
		t.Fatalf("abandoned = %d, want %d unchanged", got, before)
	}

	e.eval = realEval
	res, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("Evaluate after panic: %v", err)
	}
	if !slices.Equal(res.Rules, []string{"r"}) {
		t.Fatalf("Rules = %v, want [r]", res.Rules)
	}
	if strings.Contains(allLogText(logs), panicMarker) {
		t.Fatalf("log output contains the panic value:\n%s", allLogText(logs))
	}
}

func TestEvaluate_PanicAfterDeadlineDoesNotCrashAndReleasesTheCount(t *testing.T) {
	e := timeoutEngine(t, `deny contains "r" if { input.model == "m" }`, 20*time.Millisecond)
	before := e.abandoned.Load()

	release := make(chan struct{})
	e.eval = func(context.Context, rego.PreparedEvalQuery, map[string]any) (rego.ResultSet, error) {
		<-release // ignores the context: outlives the deadline
		panic("late " + panicMarker)
	}
	_, err := e.Evaluate(context.Background(), msgReq(`{"model":"m","messages":[]}`))
	if err == nil {
		t.Fatal("Evaluate = nil error, want timeout")
	}
	if k := evalKind(t, err); k != "timeout" {
		t.Fatalf("Kind = %q, want timeout", k)
	}
	if strings.Contains(err.Error(), panicMarker) {
		t.Fatalf("error text %q contains the panic value", err.Error())
	}
	if got := e.abandoned.Load(); got != before+1 {
		t.Fatalf("abandoned = %d while the evaluation is blocked, want %d", got, before+1)
	}

	close(release) // the abandoned goroutine now panics; the process must survive
	deadline := time.Now().Add(5 * time.Second)
	for e.abandoned.Load() != before {
		if time.Now().After(deadline) {
			t.Fatalf("abandoned = %d five seconds after the panic, want %d", e.abandoned.Load(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

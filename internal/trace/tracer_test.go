package trace

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

const testKeyEnv = "AK_TRACE_TEST_FINGERPRINT_KEY"

var testKey = strings.Repeat("k", 32)

var fingerprintPattern = regexp.MustCompile(`^fp_[0-9a-f]{16}$`)

func testTracingConfig() config.TracingConfig {
	return config.TracingConfig{
		FingerprintKeyEnv: testKeyEnv,
		MaxReadBytes:      1 << 20,
		Sink:              config.TraceSinkConfig{Type: "logging"},
	}
}

// newTestTracer builds a Tracer keyed with testKey. mutate, if non-nil,
// adjusts the config before construction.
func newTestTracer(t *testing.T, mutate func(*config.TracingConfig)) *Tracer {
	t.Helper()
	t.Setenv(testKeyEnv, testKey)
	cfg := testTracingConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	tr, err := NewTracer(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	return tr
}

func expectedFingerprint(key, canonical string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(canonical))
	return "fp_" + hex.EncodeToString(m.Sum(nil))[:16]
}

func entryText(e observer.LoggedEntry) string {
	return e.Message + " " + fmt.Sprint(e.ContextMap())
}

func newRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
}

// toolResultBody is a Messages request whose last message carries one
// tool_result with the given raw JSON content.
func toolResultBody(content string) string {
	return `{"model":"claude-sonnet-5","messages":[` +
		`{"role":"user","content":"run it"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"bash","input":{"command":"ls"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":` + content + `}]}]}`
}

func fingerprintOf(t *testing.T, tr *Tracer, content string) string {
	t.Helper()
	body := toolResultBody(content)
	info, err := tr.Request(newRequest(t, body), []byte(body), "")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(info.ToolResults) != 1 {
		t.Fatalf("len(ToolResults) = %d, want 1", len(info.ToolResults))
	}
	fp := info.ToolResults[0].Fingerprint
	if fp == nil {
		t.Fatal("ToolResults[0].Fingerprint = nil, want a fingerprint")
	}
	return *fp
}

func TestNewTracer_UnsetOrEmptyKeyVarGeneratesAKeyAndWarnsOnce(t *testing.T) {
	const keyVar = "AK_TRACE_TEST_KEY_NOT_SET"
	tests := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"unset", func(t *testing.T) { t.Setenv(keyVar, ""); _ = os.Unsetenv(keyVar) }},
		{"empty", func(t *testing.T) { t.Setenv(keyVar, "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			core, logs := observer.New(zapcore.DebugLevel)
			cfg := testTracingConfig()
			cfg.FingerprintKeyEnv = keyVar

			tr, err := NewTracer(cfg, zap.New(core))
			if err != nil {
				t.Fatalf("NewTracer with %s key var = %v, want success: a missing key must not stop startup", tt.name, err)
			}
			if tr == nil {
				t.Fatal("NewTracer returned a nil *Tracer")
			}

			warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
			if len(warns) != 1 {
				t.Fatalf("got %d warn-level entries, want exactly 1: %v", len(warns), logs.All())
			}
			if !strings.Contains(entryText(warns[0]), keyVar) {
				t.Fatalf("warning %q does not name the key env var %s", entryText(warns[0]), keyVar)
			}

			fp := fingerprintOf(t, tr, `"hello"`)
			if !fingerprintPattern.MatchString(fp) {
				t.Fatalf("fingerprint %q with a generated key, want to match %s", fp, fingerprintPattern)
			}
		})
	}
}

func TestNewTracer_ConfiguredKeyIsNeverLogged(t *testing.T) {
	const key = "configured-key-value-that-must-never-be-logged-0123456789"
	t.Setenv(testKeyEnv, key)
	core, logs := observer.New(zapcore.DebugLevel)

	tr, err := NewTracer(testTracingConfig(), zap.New(core))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	_ = fingerprintOf(t, tr, `"hello"`)

	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("got %d warn-level entries with a key configured, want 0: %v", n, logs.All())
	}
	for _, e := range logs.All() {
		if strings.Contains(entryText(e), key) {
			t.Fatalf("log entry leaked the fingerprint key: %s", entryText(e))
		}
	}
}

func TestNewTracer_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		mutate func(*config.TracingConfig)
	}{
		{
			name: "key set but shorter than 32 bytes",
			key:  strings.Repeat("k", 31),
		},
		{
			name: "pattern that does not compile",
			key:  testKey,
			mutate: func(c *config.TracingConfig) {
				c.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{Header: "X-Session-Id", Pattern: `(`}}}}
			},
		},
		{
			name: "pattern with no capture group",
			key:  testKey,
			mutate: func(c *config.TracingConfig) {
				c.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{Header: "X-Session-Id", Pattern: `^s-[a-z]+$`}}}}
			},
		},
		{
			name: "pattern with two capture groups",
			key:  testKey,
			mutate: func(c *config.TracingConfig) {
				c.Sessions = []config.SessionSourceList{
					{Agents: []string{"agent-x"}, Sources: []config.SessionSource{{Header: "X-Ok", Pattern: `^(.+)$`}}},
					{Default: true, Sources: []config.SessionSource{{Header: "X-Ok"}, {BodyField: "metadata.user_id", Pattern: `^(a)(b)$`}}},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(testKeyEnv, tt.key)
			cfg := testTracingConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			tr, err := NewTracer(cfg, zaptest.NewLogger(t))
			if err == nil {
				t.Fatalf("NewTracer = %v, nil error; want an error", tr)
			}
		})
	}
}

func TestNewTracer_Accepts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.TracingConfig)
	}{
		{"key of exactly 32 bytes and no sessions", nil},
		{
			name: "pattern with one capture group besides non-capturing groups",
			mutate: func(c *config.TracingConfig) {
				c.Sessions = []config.SessionSourceList{{Default: true, Sources: []config.SessionSource{{BodyField: "metadata.user_id", Pattern: `(?:user_[a-z]+_)?session_([0-9a-f-]{36})$`}}}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = newTestTracer(t, tt.mutate)
		})
	}
}

func TestFingerprint_FormatAndKey(t *testing.T) {
	tr := newTestTracer(t, nil)
	fp := fingerprintOf(t, tr, `{"b": 1, "a": "x"}`)

	if !fingerprintPattern.MatchString(fp) {
		t.Fatalf("fingerprint = %q, want to match %s", fp, fingerprintPattern)
	}
	if want := expectedFingerprint(testKey, `{"a":"x","b":1}`); fp != want {
		t.Fatalf("fingerprint = %q, want %q (first 16 hex chars of HMAC-SHA256 over the canonical form)", fp, want)
	}
}

func TestFingerprint_EqualCanonicalInputGivesEqualFingerprints(t *testing.T) {
	tr := newTestTracer(t, nil)
	a := fingerprintOf(t, tr, `{"b": 1, "a": "x"}`)
	b := fingerprintOf(t, tr, "{\n\"a\":\"x\",\"b\":1}")
	if a != b {
		t.Fatalf("fingerprints differ for equal canonical input: %q vs %q", a, b)
	}
	if c := fingerprintOf(t, tr, `{"a":"y","b":1}`); c == a {
		t.Fatalf("different content gave the same fingerprint %q", c)
	}
}

func TestFingerprint_DependsOnKey(t *testing.T) {
	const content = `"same content"`

	t.Run("another key gives another fingerprint", func(t *testing.T) {
		a := fingerprintOf(t, newTestTracer(t, nil), content)
		t.Setenv("AK_TRACE_TEST_OTHER_KEY", strings.Repeat("z", 32))
		other := newTestTracer(t, func(c *config.TracingConfig) { c.FingerprintKeyEnv = "AK_TRACE_TEST_OTHER_KEY" })
		if b := fingerprintOf(t, other, content); a == b {
			t.Fatalf("two different keys gave the same fingerprint %q", a)
		}
	})

	t.Run("two tracers with the same key agree", func(t *testing.T) {
		a := fingerprintOf(t, newTestTracer(t, nil), content)
		b := fingerprintOf(t, newTestTracer(t, nil), content)
		if a != b {
			t.Fatalf("same key, different fingerprints: %q vs %q", a, b)
		}
	})

	t.Run("two tracers without a key disagree", func(t *testing.T) {
		const keyVar = "AK_TRACE_TEST_KEY_NOT_SET_2"
		t.Setenv(keyVar, "")
		build := func() *Tracer {
			cfg := testTracingConfig()
			cfg.FingerprintKeyEnv = keyVar
			tr, err := NewTracer(cfg, zaptest.NewLogger(t))
			if err != nil {
				t.Fatalf("NewTracer: %v", err)
			}
			return tr
		}
		a := fingerprintOf(t, build(), content)
		b := fingerprintOf(t, build(), content)
		if a == b {
			t.Fatalf("two tracers with generated keys gave the same fingerprint %q: each must generate its own key", a)
		}
	})
}

func TestRequest_Metadata(t *testing.T) {
	tr := newTestTracer(t, nil)
	tests := []struct {
		name       string
		body       string
		wantModel  *string
		wantStream bool
	}{
		{"model and stream read", `{"model":"claude-sonnet-5","stream":true,"messages":[]}`, ptr("claude-sonnet-5"), true},
		{"stream defaults to false", `{"model":"claude-sonnet-5","messages":[]}`, ptr("claude-sonnet-5"), false},
		{"model absent is nil", `{"stream":true,"messages":[]}`, nil, true},
		{"non-JSON body gives nil model and no error", `this is not json`, nil, false},
		{"JSON that is not an object gives nil model and no error", `["a","b"]`, nil, false},
		{"empty body gives nil model and no error", ``, nil, false},
		{"object without messages is not an error", `{"model":"m"}`, ptr("m"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := tr.Request(newRequest(t, tt.body), []byte(tt.body), "")
			if err != nil {
				t.Fatalf("Request: %v, want nil: a body the provider will judge is not tracing's error", err)
			}
			if !equalPtr(info.Model, tt.wantModel) {
				t.Fatalf("Model = %v, want %v", deref(info.Model), deref(tt.wantModel))
			}
			if info.Stream != tt.wantStream {
				t.Fatalf("Stream = %v, want %v", info.Stream, tt.wantStream)
			}
			if len(info.ToolResults) != 0 {
				t.Fatalf("ToolResults = %+v, want none", info.ToolResults)
			}
		})
	}
}

func TestRequest_ReadsOnlyNewToolResults(t *testing.T) {
	tr := newTestTracer(t, nil)
	tests := []struct {
		name    string
		body    string
		wantIDs []string
	}{
		{
			name: "only the last message's tool results are new",
			body: `{"model":"m","messages":[` +
				`{"role":"user","content":"go"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_old","name":"bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"old"}]},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_new1","name":"bash","input":{}},{"type":"tool_use","id":"toolu_new2","name":"bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"text","text":"here"},{"type":"tool_result","tool_use_id":"toolu_new1","content":"a"},{"type":"tool_result","tool_use_id":"toolu_new2","content":"b"}]}]}`,
			wantIDs: []string{"toolu_new1", "toolu_new2"},
		},
		{
			name: "earlier tool results resent with no tool result in the last message are not new",
			body: `{"model":"m","messages":[` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_T","name":"bash","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_T","content":"x"}]},` +
				`{"role":"assistant","content":"done"},` +
				`{"role":"user","content":[{"type":"text","text":"thanks, next"}]}]}`,
		},
		{
			name: "last message with string content has no tool results",
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_T","content":"x"}]},{"role":"user","content":"plain"}]}`,
		},
		{
			name: "last message not from the user has no tool results",
			body: `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"toolu_T","content":"x"}]}]}`,
		},
		{
			name: "empty messages has no tool results",
			body: `{"model":"m","messages":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := tr.Request(newRequest(t, tt.body), []byte(tt.body), "")
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			var got []string
			for _, r := range info.ToolResults {
				got = append(got, r.ToolCallID)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("tool result IDs = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

func TestRequest_ToolResultFields(t *testing.T) {
	tr := newTestTracer(t, nil)
	body := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_str","content":"hello"},` +
		`{"type":"tool_result","tool_use_id":"toolu_err","is_error":true,"content":[ {"type":"text", "text":"boom"} ]},` +
		`{"type":"tool_result","tool_use_id":"toolu_none"}` +
		`]}]}`

	info, err := tr.Request(newRequest(t, body), []byte(body), "")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(info.ToolResults) != 3 {
		t.Fatalf("len(ToolResults) = %d, want 3: %+v", len(info.ToolResults), info.ToolResults)
	}

	const strCanonical = `"hello"`
	const errCanonical = `[{"text":"boom","type":"text"}]`
	tests := []struct {
		name            string
		got             ToolResult
		wantID          string
		wantIsError     bool
		wantSize        int64
		wantFingerprint *string
	}{
		{"string content; is_error defaults to false", info.ToolResults[0], "toolu_str", false, int64(len(strCanonical)), ptr(expectedFingerprint(testKey, strCanonical))},
		{"block content, is_error true, size of the canonical form", info.ToolResults[1], "toolu_err", true, int64(len(errCanonical)), ptr(expectedFingerprint(testKey, errCanonical))},
		{"absent content gives size 0 and a nil fingerprint", info.ToolResults[2], "toolu_none", false, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got.ToolCallID != tt.wantID {
				t.Fatalf("ToolCallID = %q, want %q", tt.got.ToolCallID, tt.wantID)
			}
			if tt.got.IsError != tt.wantIsError {
				t.Fatalf("IsError = %v, want %v", tt.got.IsError, tt.wantIsError)
			}
			if tt.got.SizeBytes != tt.wantSize {
				t.Fatalf("SizeBytes = %d, want %d", tt.got.SizeBytes, tt.wantSize)
			}
			if !equalPtr(tt.got.Fingerprint, tt.wantFingerprint) {
				t.Fatalf("Fingerprint = %v, want %v", deref(tt.got.Fingerprint), deref(tt.wantFingerprint))
			}
		})
	}
}

func TestRequest_UnreadableToolResultIsAnError(t *testing.T) {
	const marker = "SECRET_TOOL_OUTPUT_MARKER"
	tr := newTestTracer(t, nil)
	tests := []struct {
		name  string
		block string
	}{
		{"missing tool_use_id", `{"type":"tool_result","content":"` + marker + `"}`},
		{"empty tool_use_id", `{"type":"tool_result","tool_use_id":"","content":"` + marker + `"}`},
		{"non-string tool_use_id", `{"type":"tool_result","tool_use_id":42,"content":"` + marker + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":[` + tt.block + `]}]}`
			_, err := tr.Request(newRequest(t, body), []byte(body), "")
			if err == nil {
				t.Fatal("Request = nil error, want an error for an unreadable tool_result")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error %q leaks tool result content", err)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

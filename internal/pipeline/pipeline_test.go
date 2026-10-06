package pipeline

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap/zaptest"
)

var requestIDPattern = regexp.MustCompile(`^ak_[0-9a-f]{16}$`)

type errorEnvelope struct {
	Error struct {
		Type      string `json:"type"`
		Stage     string `json:"stage"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// eventLog records the order stage methods run in, across goroutines, so
// tests can assert on request/response ordering.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) record(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

func requestEvents(l *eventLog) []string  { return eventsWithSuffix(l, ".Request") }
func responseEvents(l *eventLog) []string { return eventsWithSuffix(l, ".Response") }

func eventsWithSuffix(l *eventLog, suffix string) []string {
	var out []string
	for _, e := range l.snapshot() {
		if strings.HasSuffix(e, suffix) {
			out = append(out, e)
		}
	}
	return out
}

// fakeStage is a minimal Stage used to pin down pipeline behavior without a
// real governance stage. It implements Stage only; fakeObserverStage adds
// the optional ResponseObserver half, so tests can tell the two apart.
type fakeStage struct {
	name         string
	decision     Decision
	err          error
	panicRequest bool
	panicMsg     string
	log          *eventLog

	requestCalls atomic.Int32

	requestMu sync.Mutex
	lastCall  *Call
}

func (s *fakeStage) Name() string { return s.name }

func (s *fakeStage) Request(_ context.Context, c *Call) (Decision, error) {
	s.requestCalls.Add(1)
	if s.log != nil {
		s.log.record(s.name + ".Request")
	}
	cp := *c
	s.requestMu.Lock()
	s.lastCall = &cp
	s.requestMu.Unlock()

	if s.panicRequest {
		msg := s.panicMsg
		if msg == "" {
			msg = s.name + " request panicked"
		}
		panic(msg)
	}
	if s.err != nil {
		return Decision{}, s.err
	}
	return s.decision, nil
}

// fakeObserverStage adds ResponseObserver to a fakeStage. Kept as a
// separate type, rather than always implementing Response on fakeStage, so
// tests can prove the pipeline tolerates stages that don't implement it.
type fakeObserverStage struct {
	*fakeStage
	panicResponse bool

	responseCalls atomic.Int32

	responseMu   sync.Mutex
	lastResponse *Call
}

func (s *fakeObserverStage) Response(_ context.Context, c *Call) {
	s.responseCalls.Add(1)
	if s.log != nil {
		s.log.record(s.name + ".Response")
	}
	cp := *c
	s.responseMu.Lock()
	s.lastResponse = &cp
	s.responseMu.Unlock()

	if s.panicResponse {
		panic(s.name + " response panicked")
	}
}

func allowStage(name string, log *eventLog) *fakeObserverStage {
	return &fakeObserverStage{fakeStage: &fakeStage{name: name, decision: Allow(), log: log}}
}

func denyStage(name string, status int, message, reason string, log *eventLog) *fakeObserverStage {
	return &fakeObserverStage{fakeStage: &fakeStage{name: name, decision: Deny(status, message, reason), log: log}}
}

func errStage(name string, err error, log *eventLog) *fakeObserverStage {
	return &fakeObserverStage{fakeStage: &fakeStage{name: name, err: err, log: log}}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("simulated body read failure") }

func doRequest(t *testing.T, p *Pipeline, next http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)
	return rec
}

func TestPipeline_RequestOrder_DenialShortCircuits(t *testing.T) {
	log := &eventLog{}
	s1 := allowStage("s1", log)
	s2 := denyStage("s2", http.StatusForbidden, "no", "policy violated", log)
	s3 := allowStage("s3", log)

	var nextCalled atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	p := New(zaptest.NewLogger(t), 1<<20, nil, s1, s2, s3)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("hi"))
	rec := doRequest(t, p, next, req)

	if s1.requestCalls.Load() != 1 {
		t.Fatalf("s1.requestCalls = %d, want 1", s1.requestCalls.Load())
	}
	if s2.requestCalls.Load() != 1 {
		t.Fatalf("s2.requestCalls = %d, want 1", s2.requestCalls.Load())
	}
	if s3.requestCalls.Load() != 0 {
		t.Fatalf("s3.requestCalls = %d, want 0 (a stage after a denial must not run)", s3.requestCalls.Load())
	}
	if nextCalled.Load() {
		t.Fatal("next must not be called after a denial")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}

	want := []string{"s1.Request", "s2.Request"}
	if got := requestEvents(log); !reflect.DeepEqual(got, want) {
		t.Fatalf("request order = %v, want %v", got, want)
	}
}

func TestPipeline_ObserverOrderAndCoverage(t *testing.T) {
	tests := []struct {
		name         string
		buildStages  func(log *eventLog) []*fakeObserverStage
		wantOrder    []string
		wantDeniedBy string // "" means no denial
	}{
		{
			name: "all allow: every observer fires once, in reverse request order",
			buildStages: func(log *eventLog) []*fakeObserverStage {
				return []*fakeObserverStage{allowStage("s1", log), allowStage("s2", log), allowStage("s3", log)}
			},
			wantOrder: []string{"s3.Response", "s2.Response", "s1.Response"},
		},
		{
			name: "middle stage denies: stages after it still observe, with Call.Denial populated",
			buildStages: func(log *eventLog) []*fakeObserverStage {
				return []*fakeObserverStage{
					allowStage("s1", log),
					denyStage("s2", http.StatusTeapot, "no", "policy", log),
					allowStage("s3", log),
				}
			},
			wantOrder:    []string{"s3.Response", "s2.Response", "s1.Response"},
			wantDeniedBy: "s2",
		},
		{
			name: "a stage error still lets every observer run, with Call.Denial naming the failed stage",
			buildStages: func(log *eventLog) []*fakeObserverStage {
				return []*fakeObserverStage{
					allowStage("s1", log),
					errStage("s2", errors.New("boom"), log),
					allowStage("s3", log),
				}
			},
			wantOrder:    []string{"s3.Response", "s2.Response", "s1.Response"},
			wantDeniedBy: "s2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &eventLog{}
			stages := tt.buildStages(log)

			pstages := make([]Stage, len(stages))
			for i, s := range stages {
				pstages[i] = s
			}
			p := New(zaptest.NewLogger(t), 1<<20, nil, pstages...)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

			if got := responseEvents(log); !reflect.DeepEqual(got, tt.wantOrder) {
				t.Fatalf("response order = %v, want %v", got, tt.wantOrder)
			}

			for _, s := range stages {
				if s.responseCalls.Load() != 1 {
					t.Fatalf("stage %s observed %d times, want exactly 1", s.name, s.responseCalls.Load())
				}
				if s.lastResponse == nil {
					t.Fatalf("stage %s never received a Call in Response", s.name)
				}
				if tt.wantDeniedBy == "" {
					if s.lastResponse.Denial != nil {
						t.Fatalf("stage %s saw Denial = %+v, want nil", s.name, s.lastResponse.Denial)
					}
					continue
				}
				if s.lastResponse.Denial == nil || s.lastResponse.Denial.Stage != tt.wantDeniedBy {
					t.Fatalf("stage %s saw Denial = %+v, want Stage %q", s.name, s.lastResponse.Denial, tt.wantDeniedBy)
				}
			}
		})
	}
}

func TestPipeline_NonObserverStagesAreSkippedWithoutError(t *testing.T) {
	log := &eventLog{}
	plain := &fakeStage{name: "plain", decision: Allow(), log: log} // implements Stage only, not ResponseObserver
	obs := allowStage("obs", log)

	p := New(zaptest.NewLogger(t), 1<<20, nil, plain, obs)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if plain.requestCalls.Load() != 1 {
		t.Fatalf("plain.requestCalls = %d, want 1", plain.requestCalls.Load())
	}
	if obs.responseCalls.Load() != 1 {
		t.Fatalf("obs.responseCalls = %d, want 1", obs.responseCalls.Load())
	}
}

func TestPipeline_DenialRendering(t *testing.T) {
	const reason = "agent ak-42 exceeded 10 req/min"
	stage := denyStage("rate_limiting", http.StatusTooManyRequests, "request quota exceeded", reason, nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next must not be called on denial")
	})

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	reqID := rec.Header().Get("X-Agentkeeper-Request-Id")
	if !requestIDPattern.MatchString(reqID) {
		t.Fatalf("X-Agentkeeper-Request-Id = %q, want to match %s", reqID, requestIDPattern)
	}

	raw := rec.Body.Bytes()
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal denial body: %v; body = %s", err, raw)
	}
	if env.Error.Type != "agentkeeper_denied" {
		t.Fatalf("type = %q, want agentkeeper_denied", env.Error.Type)
	}
	if env.Error.Stage != "rate_limiting" {
		t.Fatalf("stage = %q, want rate_limiting", env.Error.Stage)
	}
	if env.Error.Message != "request quota exceeded" {
		t.Fatalf("message = %q, want %q", env.Error.Message, "request quota exceeded")
	}
	if env.Error.RequestID != reqID {
		t.Fatalf("body request_id = %q, want it to match header %q", env.Error.RequestID, reqID)
	}
	if bytes.Contains(raw, []byte(reason)) {
		t.Fatalf("denial body leaked Denial.Reason, which must stay audit-only: %s", raw)
	}
}

func TestPipeline_UnavailableRendering_OnStageError(t *testing.T) {
	const secret = "upstream-internal-detail-should-not-leak"
	stage := errStage("policies", fmt.Errorf("dial tcp failed: %s", secret), nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next must not be called when a stage errors")
	})

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	raw := rec.Body.Bytes()
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal body: %v; body = %s", err, raw)
	}
	if env.Error.Type != "agentkeeper_unavailable" {
		t.Fatalf("type = %q, want agentkeeper_unavailable", env.Error.Type)
	}
	if env.Error.Stage != "policies" {
		t.Fatalf("stage = %q, want policies", env.Error.Stage)
	}
	if reqID := rec.Header().Get("X-Agentkeeper-Request-Id"); !requestIDPattern.MatchString(reqID) {
		t.Fatalf("X-Agentkeeper-Request-Id = %q, want to match %s", reqID, requestIDPattern)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("unavailable body leaked internal error detail: %s", raw)
	}
}

func TestPipeline_UnavailableRendering_OnStagePanic(t *testing.T) {
	const secret = "panic-internal-detail-should-not-leak"
	stage := &fakeObserverStage{fakeStage: &fakeStage{name: "monitoring", panicRequest: true, panicMsg: secret}}
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next must not be called when a stage panics")
	})

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	raw := rec.Body.Bytes()
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal body: %v; body = %s", err, raw)
	}
	if env.Error.Type != "agentkeeper_unavailable" {
		t.Fatalf("type = %q, want agentkeeper_unavailable", env.Error.Type)
	}
	if env.Error.Stage != "monitoring" {
		t.Fatalf("stage = %q, want monitoring", env.Error.Stage)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("unavailable body leaked panic detail: %s", raw)
	}
	if stage.responseCalls.Load() != 1 {
		t.Fatalf("observer must still fire exactly once after a panic, got %d", stage.responseCalls.Load())
	}
	if stage.lastResponse == nil || stage.lastResponse.Denial == nil || stage.lastResponse.Denial.Stage != "monitoring" {
		t.Fatalf("Call.Denial = %+v, want Stage \"monitoring\"", stage.lastResponse.Denial)
	}

	// The gateway process must keep serving subsequent requests after a
	// stage panic; a second call through the same pipeline must behave the
	// same way rather than crashing the process.
	rec2 := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request status = %d, want %d", rec2.Code, http.StatusServiceUnavailable)
	}
}

func TestPipeline_ResponsePanicDoesNotAffectAlreadySentResponse(t *testing.T) {
	stage := &fakeObserverStage{fakeStage: &fakeStage{name: "audit", decision: Allow()}, panicResponse: true}
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream-ok"))
	})

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (a panicking observer must not retroactively change a response already sent)", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "upstream-ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "upstream-ok")
	}
	if stage.responseCalls.Load() != 1 {
		t.Fatalf("responseCalls = %d, want 1", stage.responseCalls.Load())
	}

	rec2 := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec2.Code != http.StatusOK || rec2.Body.String() != "upstream-ok" {
		t.Fatalf("second request must succeed normally, got status=%d body=%q", rec2.Code, rec2.Body.String())
	}
}

func TestPipeline_RequestID_GeneratedAndCallerCannotControlIt(t *testing.T) {
	stage := denyStage("identification", http.StatusForbidden, "denied", "reason", nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next must not be called")
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Agentkeeper-Request-Id", "attacker-controlled-id")
	rec := doRequest(t, p, next, req)

	respID := rec.Header().Get("X-Agentkeeper-Request-Id")
	if respID == "attacker-controlled-id" {
		t.Fatal("caller-supplied request id must not be echoed back")
	}
	if !requestIDPattern.MatchString(respID) {
		t.Fatalf("response request id = %q, want to match %s", respID, requestIDPattern)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if env.Error.RequestID != respID {
		t.Fatalf("body request_id = %q, want %q", env.Error.RequestID, respID)
	}

	if stage.lastCall == nil {
		t.Fatal("stage never ran")
	}
	if stage.lastCall.ID == "attacker-controlled-id" {
		t.Fatal("Call.ID must not be derived from the caller-supplied header")
	}
	if !requestIDPattern.MatchString(stage.lastCall.ID) {
		t.Fatalf("Call.ID = %q, want to match %s", stage.lastCall.ID, requestIDPattern)
	}
}

func TestPipeline_RequestID_UniquePerCall(t *testing.T) {
	stage := allowStage("audit", nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	seen := make(map[string]bool)
	for range 5 {
		doRequest(t, p, next, httptest.NewRequest(http.MethodGet, "/", nil))
		id := stage.lastCall.ID
		if !requestIDPattern.MatchString(id) {
			t.Fatalf("Call.ID = %q, want to match %s", id, requestIDPattern)
		}
		if seen[id] {
			t.Fatalf("Call.ID %q reused across calls", id)
		}
		seen[id] = true
	}
}

func TestPipeline_BodyReadOnceAndSharedWithNext(t *testing.T) {
	const bodyText = `{"hello":"world"}`
	stage := allowStage("filtering", nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)

	var nextBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("next: reading r.Body: %v", err)
		}
		nextBody = string(b)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(bodyText))
	doRequest(t, p, next, req)

	if nextBody != bodyText {
		t.Fatalf("next saw body %q, want %q (r.Body must still be fully and identically readable downstream)", nextBody, bodyText)
	}
	if stage.lastCall == nil {
		t.Fatal("stage never ran")
	}
	if string(stage.lastCall.Body) != bodyText {
		t.Fatalf("stage saw Call.Body %q, want %q", stage.lastCall.Body, bodyText)
	}
}

func TestPipeline_BodyOverLimitDeniedBeforeAnyStageRuns(t *testing.T) {
	stage := allowStage("identification", nil)
	p := New(zaptest.NewLogger(t), 10, nil, stage) // 10-byte limit

	var nextCalled atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nextCalled.Store(true) })

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 11)))
	rec := doRequest(t, p, next, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if nextCalled.Load() {
		t.Fatal("next must not be called for an over-limit body")
	}
	if stage.requestCalls.Load() != 0 {
		t.Fatalf("stage.Request called %d times, want 0: no stage can evaluate bytes it never received", stage.requestCalls.Load())
	}
	if stage.responseCalls.Load() != 1 {
		t.Fatalf("observer must still fire exactly once, got %d", stage.responseCalls.Load())
	}
	if stage.lastResponse == nil || stage.lastResponse.Denial == nil || stage.lastResponse.Denial.Stage != "pipeline" {
		t.Fatalf("Call.Denial = %+v, want Stage \"pipeline\"", stage.lastResponse.Denial)
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if env.Error.Type != "agentkeeper_denied" {
		t.Fatalf("type = %q, want agentkeeper_denied", env.Error.Type)
	}
}

func TestPipeline_BodyReadFailureDenied(t *testing.T) {
	stage := allowStage("identification", nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)

	var nextCalled atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nextCalled.Store(true) })

	req := httptest.NewRequest(http.MethodPost, "/", errorReader{})
	rec := doRequest(t, p, next, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if nextCalled.Load() {
		t.Fatal("next must not be called when the body cannot be read")
	}
	if stage.requestCalls.Load() != 0 {
		t.Fatalf("stage.Request called %d times, want 0", stage.requestCalls.Load())
	}
	if stage.responseCalls.Load() != 1 {
		t.Fatalf("observer must still fire exactly once, got %d", stage.responseCalls.Load())
	}

	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if env.Error.Type != "agentkeeper_denied" {
		t.Fatalf("type = %q, want agentkeeper_denied", env.Error.Type)
	}
}

func TestPipeline_ZeroValueDecisionDenies(t *testing.T) {
	stage := &fakeObserverStage{fakeStage: &fakeStage{name: "policies", decision: Decision{}}}
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)

	var nextCalled atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { nextCalled.Store(true) })

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodGet, "/", nil))

	if nextCalled.Load() {
		t.Fatal("a zero-value Decision must deny, not allow")
	}
	if rec.Code < 400 {
		t.Fatalf("status = %d, want an error status for a denied call", rec.Code)
	}
	if stage.lastResponse == nil || stage.lastResponse.Denial == nil || stage.lastResponse.Denial.Stage != "policies" {
		t.Fatalf("Call.Denial = %+v, want Stage \"policies\"", stage.lastResponse.Denial)
	}
}

func TestPipeline_AllowedCallPassesThroughUnchanged(t *testing.T) {
	stages := []Stage{allowStage("identification", nil), allowStage("policies", nil), allowStage("audit", nil)}
	p := New(zaptest.NewLogger(t), 1<<20, nil, stages...)

	const upstreamBody = `{"ok":true}`
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(upstreamBody))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	rec := doRequest(t, p, next, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body = %q, want %q", rec.Body.String(), upstreamBody)
	}
	if got := rec.Header().Get("X-Upstream"); got != "yes" {
		t.Fatalf("X-Upstream header = %q, want %q (upstream headers must reach the caller unchanged)", got, "yes")
	}
	if got := rec.Header().Get("X-Agentkeeper-Request-Id"); got != "" {
		t.Fatalf("X-Agentkeeper-Request-Id = %q, want empty on an allowed call", got)
	}
}

func TestPipeline_DecodesGzipResponseBodyForObservers(t *testing.T) {
	const plain = `{"usage":{"input_tokens":10,"output_tokens":5}}`
	var gzipped bytes.Buffer
	gw := gzip.NewWriter(&gzipped)
	_, _ = gw.Write([]byte(plain))
	_ = gw.Close()
	wireBytes := append([]byte(nil), gzipped.Bytes()...)

	stage := allowStage("token_counting", nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wireBytes)
	})

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

	if !bytes.Equal(rec.Body.Bytes(), wireBytes) {
		t.Fatal("caller must receive the gzip-encoded bytes exactly as the upstream sent them")
	}
	if stage.lastResponse == nil {
		t.Fatal("observer never ran")
	}
	if string(stage.lastResponse.ResponseBody) != plain {
		t.Fatalf("Call.ResponseBody = %q, want the gunzipped body %q", stage.lastResponse.ResponseBody, plain)
	}
}

func TestPipeline_StreamingResponseFlushesBeforeObserving(t *testing.T) {
	chunks := []string{"chunk-1\n", "chunk-2\n", "chunk-3\n"}
	release := make([]chan struct{}, len(chunks))
	for i := range release {
		release[i] = make(chan struct{})
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter passed to next must support http.Flusher for streaming")
		}
		w.WriteHeader(http.StatusOK)
		for i, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			<-release[i]
		}
	})

	stage := allowStage("audit", nil)
	p := New(zaptest.NewLogger(t), 1<<20, nil, stage)
	srv := httptest.NewServer(p.Handler(next))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for i, want := range chunks {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading chunk %d: %v", i, err)
		}
		if line != want {
			t.Fatalf("chunk %d = %q, want %q", i, line, want)
		}
		if got := stage.responseCalls.Load(); got != 0 {
			t.Fatalf("observer ran before streaming finished (after chunk %d): responseCalls = %d", i, got)
		}
		close(release[i])
	}

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("draining response: %v", err)
	}

	if got := stage.responseCalls.Load(); got != 1 {
		t.Fatalf("observer ran %d times after the stream completed, want 1", got)
	}
}

func TestPipeline_ResponseOverLimitStreamsFullyButObservationIsTruncated(t *testing.T) {
	const maxBody = 16

	tests := []struct {
		name        string
		writeChunks func(w http.ResponseWriter) []byte // returns the full body written, for the assertion
	}{
		{
			name: "single write straddles the cap",
			writeChunks: func(w http.ResponseWriter) []byte {
				body := []byte(strings.Repeat("a", maxBody-4) + strings.Repeat("b", 20)) // len 32, cap crossed mid-write
				_, _ = w.Write(body)
				return body
			},
		},
		{
			name: "writes continue after the cap is already hit",
			writeChunks: func(w http.ResponseWriter) []byte {
				var full bytes.Buffer
				for i := 0; i < 4; i++ {
					chunk := []byte(strings.Repeat(string(rune('a'+i)), 10)) // 4 * 10 = 40 bytes total, cap is 16
					_, _ = w.Write(chunk)
					full.Write(chunk)
				}
				return full.Bytes()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stage := allowStage("audit", nil)
			p := New(zaptest.NewLogger(t), maxBody, nil, stage)

			var fullBody []byte
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				fullBody = tt.writeChunks(w)
			})

			rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if !bytes.Equal(rec.Body.Bytes(), fullBody) {
				t.Fatalf("caller received %d bytes, want the full %d-byte response: a body over max_body_bytes must still be streamed to the caller in full", rec.Body.Len(), len(fullBody))
			}

			if stage.lastResponse == nil {
				t.Fatal("observer never ran")
			}
			if !stage.lastResponse.ResponseTruncated {
				t.Fatal("Call.ResponseTruncated = false, want true for a response over max_body_bytes")
			}
			if len(stage.lastResponse.ResponseBody) != maxBody {
				t.Fatalf("len(Call.ResponseBody) = %d, want %d (capped at max_body_bytes)", len(stage.lastResponse.ResponseBody), maxBody)
			}
			if !bytes.Equal(stage.lastResponse.ResponseBody, fullBody[:maxBody]) {
				t.Fatalf("Call.ResponseBody = %q, want the first %d bytes of the full response %q", stage.lastResponse.ResponseBody, maxBody, fullBody)
			}
		})
	}
}

func TestPipeline_ResponseUnderLimitIsNotMarkedTruncated(t *testing.T) {
	const maxBody = 1 << 20
	const upstreamBody = `{"ok":true}`

	stage := allowStage("audit", nil)
	p := New(zaptest.NewLogger(t), maxBody, nil, stage)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	})

	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Body.String() != upstreamBody {
		t.Fatalf("body = %q, want %q", rec.Body.String(), upstreamBody)
	}
	if stage.lastResponse == nil {
		t.Fatal("observer never ran")
	}
	if stage.lastResponse.ResponseTruncated {
		t.Fatal("Call.ResponseTruncated = true, want false: a response under max_body_bytes must not be flagged as truncated")
	}
	if string(stage.lastResponse.ResponseBody) != upstreamBody {
		t.Fatalf("Call.ResponseBody = %q, want %q", stage.lastResponse.ResponseBody, upstreamBody)
	}
}

func TestPipeline_AllStagesDisabled_PassesThroughByteIdenticalWithNoStageInvoked(t *testing.T) {
	const upstreamBody = `{"ok":true}`
	p := New(zaptest.NewLogger(t), 1<<20, nil) // no stages at all, as if every pipeline.stages.* was disabled

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(upstreamBody))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	rec := doRequest(t, p, next, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body = %q, want %q", rec.Body.String(), upstreamBody)
	}
	if got := rec.Header().Get("X-Upstream"); got != "yes" {
		t.Fatalf("X-Upstream header = %q, want %q", got, "yes")
	}
}

// --- X-Should-Retry (specs/0001, as amended by 0004) ---------------------------

func TestPipeline_ShouldRetryHeader(t *testing.T) {
	tests := []struct {
		name       string
		stage      func() Stage
		body       io.Reader
		maxBody    int64
		wantStatus int
		wantHeader []string // nil: the header must be absent
	}{
		{
			name: "a deliberate denial says do not retry",
			stage: func() Stage {
				return denyStage("policies", http.StatusForbidden, "request denied by policy", "rules=r", nil)
			},
			wantStatus: http.StatusForbidden,
			wantHeader: []string{"false"},
		},
		{
			name: "identification's 401 says do not retry",
			stage: func() Stage {
				return denyStage("identification", http.StatusUnauthorized, "unauthorized", "no key", nil)
			},
			wantStatus: http.StatusUnauthorized,
			wantHeader: []string{"false"},
		},
		{
			name:       "the pipeline's own 413 says do not retry",
			stage:      func() Stage { return allowStage("policies", nil) },
			body:       strings.NewReader(strings.Repeat("x", 11)),
			maxBody:    10,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantHeader: []string{"false"},
		},
		{
			name:       "the pipeline's own 400 says do not retry",
			stage:      func() Stage { return allowStage("policies", nil) },
			body:       errorReader{},
			wantStatus: http.StatusBadRequest,
			wantHeader: []string{"false"},
		},
		{
			name:       "a stage failure carries no X-Should-Retry",
			stage:      func() Stage { return errStage("policies", errors.New("evaluation failed"), nil) },
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "a stage panic carries no X-Should-Retry",
			stage: func() Stage {
				return &fakeStage{name: "policies", panicRequest: true}
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			max := tt.maxBody
			if max == 0 {
				max = 1 << 20
			}
			p := New(zaptest.NewLogger(t), max, nil, tt.stage())
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Fatal("next must not be called")
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", tt.body)
			rec := doRequest(t, p, next, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			got, present := rec.Header()["X-Should-Retry"]
			if tt.wantHeader == nil {
				if present {
					t.Fatalf("X-Should-Retry = %v, want the header absent on a failure", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tt.wantHeader[0] {
				t.Fatalf("X-Should-Retry = %v, want %v", got, tt.wantHeader)
			}
		})
	}
}

func TestPipeline_AllowedCallHasNoShouldRetryHeaderFromTheGateway(t *testing.T) {
	p := New(zaptest.NewLogger(t), 1<<20, nil, allowStage("policies", nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := doRequest(t, p, next, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if v, ok := rec.Header()["X-Should-Retry"]; ok {
		t.Fatalf("X-Should-Retry = %v on a forwarded call, want none: the provider's headers are relayed untouched", v)
	}
}

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/identification"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
)

// identificationStage must satisfy pipeline.Stage; it never observes
// responses, so it deliberately does not implement pipeline.ResponseObserver.
var _ pipeline.Stage = (*identificationStage)(nil)

var _ = func() bool {
	var s pipeline.Stage = &identificationStage{}
	if _, isObserver := s.(pipeline.ResponseObserver); isObserver {
		panic("identificationStage implements ResponseObserver; identification never observes a response")
	}
	return true
}()

// Fixture shared by this file and authorization_test.go: one agent type,
// "test-agent", recognized by one exact-value header rule.
const (
	testAgentTypeHeader = "X-Test-Agent-Type"
	testAgentTypeValue  = "test-agent-marker"
	testAgentTypeID     = "test-agent"
)

func writeAgentTypeFile(t *testing.T, dir, filename, agentID, header, value string) {
	t.Helper()
	contents := "agent_id: \"" + agentID + "\"\n" +
		"match:\n" +
		"  - header: \"" + header + "\"\n" +
		"    value: \"" + value + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing agent-type file %s: %v", filename, err)
	}
}

// newOneTypeRegistry builds a real identification.Registry with one agent
// type, "test-agent", matched by testAgentTypeHeader == testAgentTypeValue.
func newOneTypeRegistry(t *testing.T) *identification.Registry {
	t.Helper()
	dir := t.TempDir()
	writeAgentTypeFile(t, dir, "test-agent.yaml", testAgentTypeID, testAgentTypeHeader, testAgentTypeValue)
	reg, err := identification.NewRegistry(dir)
	if err != nil {
		t.Fatalf("identification.NewRegistry: %v", err)
	}
	return reg
}

// capturingStage is a minimal pipeline.Stage that always allows and records
// the last Call it saw, so a test can observe what the pipeline itself
// attached to Call.AgentID by the time a later stage runs — the only way to
// observe it, since it is never echoed to the caller.
type capturingStage struct {
	mu   sync.Mutex
	last *pipeline.Call
}

func (s *capturingStage) Name() string { return "capturing" }

func (s *capturingStage) Request(_ context.Context, c *pipeline.Call) (pipeline.Decision, error) {
	cp := *c
	s.mu.Lock()
	s.last = &cp
	s.mu.Unlock()
	return pipeline.Allow(), nil
}

func (s *capturingStage) lastCall() *pipeline.Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func TestIdentificationStage_Name(t *testing.T) {
	s := &identificationStage{registry: newOneTypeRegistry(t)}
	if got := s.Name(); got != "identification" {
		t.Fatalf("Name() = %q, want %q", got, "identification")
	}
}

func TestIdentificationStage_MatchingRequestAttachesTheMatchedType(t *testing.T) {
	s := &identificationStage{registry: newOneTypeRegistry(t)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set(testAgentTypeHeader, testAgentTypeValue)
	call := &pipeline.Call{Request: req}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if decision != pipeline.AllowAs(testAgentTypeID) {
		t.Fatalf("decision = %+v, want pipeline.AllowAs(%q)", decision, testAgentTypeID)
	}
}

func TestIdentificationStage_NonMatchingRequestAttachesUnknown(t *testing.T) {
	s := &identificationStage{registry: newOneTypeRegistry(t)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil) // no matching header
	call := &pipeline.Call{Request: req}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if decision != pipeline.AllowAs(identification.Unknown) {
		t.Fatalf("decision = %+v, want pipeline.AllowAs(%q)", decision, identification.Unknown)
	}
}

func TestIdentificationStage_NeverDenies(t *testing.T) {
	s := &identificationStage{registry: newOneTypeRegistry(t)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	call := &pipeline.Call{Request: req}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if decision != pipeline.AllowAs(identification.Unknown) {
		t.Fatalf("decision = %+v, an unmatched call must still be allowed, as %q", decision, identification.Unknown)
	}
}

// --- buildStages -----------------------------------------------------------

func identCfg(t *testing.T, enabled bool, agentTypesDir string) config.Config {
	t.Helper()
	return config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Identification: enabled},
		},
		Identification: config.IdentificationConfig{AgentTypesDir: agentTypesDir},
	}
}

func TestBuildStages_OmitsIdentificationWhenDisabled(t *testing.T) {
	cfg := identCfg(t, false, "")
	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if sink != nil {
		t.Cleanup(func() { _ = sink.Close() })
	}
	for _, s := range stages {
		if s.Name() == "identification" {
			t.Fatal("identificationStage present in buildStages output despite identification being disabled")
		}
	}
}

func TestBuildStages_IdentificationEnabledWithEmptyDirStillBuilds(t *testing.T) {
	cfg := identCfg(t, true, "")
	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if sink != nil {
		t.Cleanup(func() { _ = sink.Close() })
	}
	if len(stages) != 1 || stages[0].Name() != "identification" {
		t.Fatalf("stages = %v, want exactly one identification stage", stages)
	}
}

func TestBuildStages_IdentificationNonexistentDirFailsConstruction(t *testing.T) {
	cfg := identCfg(t, true, filepath.Join(t.TempDir(), "does-not-exist"))
	stages, _, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err == nil {
		t.Fatal("buildStages with a nonexistent agent_types_dir = nil error, want error")
	}
	if stages != nil {
		t.Fatalf("stages = %v, want nil on error", stages)
	}
}

// --- tracing.sessions[].agents cross-validation -----------------------------

func TestBuildStages_RejectsUnknownTracingSessionAgentWhenIdentificationEnabled(t *testing.T) {
	dir := t.TempDir()
	writeAgentTypeFile(t, dir, "test-agent.yaml", testAgentTypeID, testAgentTypeHeader, testAgentTypeValue)

	cfg := identCfg(t, true, dir)
	cfg.Pipeline.Stages.Tracing = true
	cfg.Tracing = fileSinkTracing(t)
	cfg.Tracing.Sessions = []config.SessionSourceList{{
		Agents:  []string{"no-such-agent-type"},
		Sources: []config.SessionSource{{Header: "X-Session-Id"}},
	}}

	stages, _, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err == nil {
		t.Fatal("buildStages with an unrecognized tracing.sessions[].agents entry = nil error, want error")
	}
	if stages != nil {
		t.Fatalf("stages = %v, want nil on error", stages)
	}
}

func TestBuildStages_AcceptsUnknownReservedNameInTracingSessionAgents(t *testing.T) {
	dir := t.TempDir()
	writeAgentTypeFile(t, dir, "test-agent.yaml", testAgentTypeID, testAgentTypeHeader, testAgentTypeValue)

	cfg := identCfg(t, true, dir)
	cfg.Pipeline.Stages.Tracing = true
	cfg.Tracing = fileSinkTracing(t)
	cfg.Tracing.Sessions = []config.SessionSourceList{{
		Agents:  []string{identification.Unknown},
		Sources: []config.SessionSource{{Header: "X-Session-Id"}},
	}}

	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages with %q in tracing.sessions[].agents: %v, want success: it is a reserved, always-known name", identification.Unknown, err)
	}
	if sink != nil {
		t.Cleanup(func() { _ = sink.Close() })
	}
	if len(stages) == 0 {
		t.Fatal("buildStages returned no stages")
	}
}

func TestBuildStages_DoesNotValidateTracingSessionAgentsWhenIdentificationDisabled(t *testing.T) {
	cfg := identCfg(t, false, "")
	cfg.Pipeline.Stages.Tracing = true
	cfg.Tracing = fileSinkTracing(t)
	cfg.Tracing.Sessions = []config.SessionSourceList{{
		Agents:  []string{"this-agent-type-is-never-validated"},
		Sources: []config.SessionSource{{Header: "X-Session-Id"}},
	}}

	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v, want success: with identification disabled, a stray tracing.sessions[].agents entry is inert, not validated", err)
	}
	if sink != nil {
		t.Cleanup(func() { _ = sink.Close() })
	}
	if len(stages) == 0 {
		t.Fatal("buildStages returned no stages")
	}
}

// --- full-pipeline integration: identification disabled leaves AgentID "" ---

func TestIdentificationStage_FullPipeline_DisabledMeansAgentIDStaysEmpty(t *testing.T) {
	capture := &capturingStage{}
	// No identificationStage at all: identification disabled.
	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, capture)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set(testAgentTypeHeader, testAgentTypeValue)
	rec := httptest.NewRecorder()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	p.Handler(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := capture.lastCall().AgentID; got != "" {
		t.Fatalf("Call.AgentID = %q, want empty with identification disabled", got)
	}
}

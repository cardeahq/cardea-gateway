package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap/zaptest"

	"github.com/agentkeeper/agentkeeper/internal/config"
	"github.com/agentkeeper/agentkeeper/internal/identity"
	"github.com/agentkeeper/agentkeeper/internal/pipeline"
)

// identityStage must satisfy pipeline.Stage; unlike tokenStage it never
// observes responses, so it deliberately does not implement
// pipeline.ResponseObserver.
var _ pipeline.Stage = (*identityStage)(nil)

const alphaAPIKey = "alpha-opaque-api-key-value"

var betaSecret = bytes.Repeat([]byte("B"), 32) // >= 32 bytes, HS256's RFC 7518 §3.2 floor

// newAlphaBetaRegistry builds a real two-agent identity.Registry: agent-alpha
// via an API key, agent-beta via a JWT. Shared by every identityStage test
// below so denial-category tests run against one fixed, known registry.
func newAlphaBetaRegistry(t *testing.T) *identity.Registry {
	t.Helper()
	t.Setenv("AK_GW_TEST_ALPHA_API_KEY", alphaAPIKey)
	t.Setenv("AK_GW_TEST_BETA_JWT_SECRET", string(betaSecret))

	reg, err := identity.NewRegistry([]config.AgentConfig{
		{AgentID: "agent-alpha", APIKeyEnv: "AK_GW_TEST_ALPHA_API_KEY"},
		{AgentID: "agent-beta", JWTSecretEnv: "AK_GW_TEST_BETA_JWT_SECRET"},
	})
	if err != nil {
		t.Fatalf("identity.NewRegistry: %v", err)
	}
	return reg
}

func mintBetaJWT(t *testing.T, secret []byte, exp time.Time) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "agent-beta",
		"exp": jwt.NewNumericDate(exp),
	})
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("signing test JWT: %v", err)
	}
	return s
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

func TestIdentityStage_Name(t *testing.T) {
	s := &identityStage{registry: newAlphaBetaRegistry(t)}
	if got := s.Name(); got != "identification" {
		t.Fatalf("Name() = %q, want %q", got, "identification")
	}
}

func TestIdentityStage_ValidCredentialAllowsAndAttachesAgentID(t *testing.T) {
	s := &identityStage{registry: newAlphaBetaRegistry(t)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+alphaAPIKey)
	call := &pipeline.Call{Request: req}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if decision != pipeline.AllowAs("agent-alpha") {
		t.Fatalf("decision = %+v, want pipeline.AllowAs(%q)", decision, "agent-alpha")
	}
}

func TestIdentityStage_DenialCategories(t *testing.T) {
	s := &identityStage{registry: newAlphaBetaRegistry(t)}
	past := time.Now().Add(-time.Hour)

	tests := []struct {
		name       string
		header     string
		wantReason string
	}{
		{
			name:       "no credential presented",
			header:     "",
			wantReason: string(identity.ReasonNoCredential),
		},
		{
			name:       "credential in an unrecognized shape",
			header:     "Basic dXNlcjpwYXNz",
			wantReason: string(identity.ReasonUnrecognizedShape),
		},
		{
			name:       "API key matches no registered agent",
			header:     "Bearer no-such-key",
			wantReason: string(identity.ReasonNoMatch),
		},
		{
			name:       "JWT outside its validity window",
			header:     "Bearer " + mintBetaJWT(t, betaSecret, past),
			wantReason: string(identity.ReasonOutsideValidity) + ": claimed agent_id=agent-beta",
		},
	}

	seenReasons := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			call := &pipeline.Call{Request: req}

			decision, err := s.Request(context.Background(), call)
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			want := pipeline.Deny(http.StatusUnauthorized, identityDenialMessage, tt.wantReason)
			if decision != want {
				t.Fatalf("decision = %+v, want %+v", decision, want)
			}
			seenReasons[tt.wantReason] = true
		})
	}

	if len(seenReasons) != len(tests) {
		t.Fatalf("only %d distinct reasons across %d denial categories, want all four distinct", len(seenReasons), len(tests))
	}
}

func TestIdentityStage_DenialsShareStatusAndMessage(t *testing.T) {
	s := &identityStage{registry: newAlphaBetaRegistry(t)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil) // no Authorization header
	call := &pipeline.Call{Request: req}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	want := pipeline.Deny(http.StatusUnauthorized, identityDenialMessage, string(identity.ReasonNoCredential))
	if decision != want {
		t.Fatalf("decision = %+v, want %+v", decision, want)
	}
}

func TestBuildStages_PrependsIdentificationAheadOfTracing(t *testing.T) {
	t.Setenv("AK_GW_TEST_BUILD_ALPHA_KEY", alphaAPIKey)
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Tracing: true, Identification: true},
		},
		Agents:  []config.AgentConfig{{AgentID: "agent-alpha", APIKeyEnv: "AK_GW_TEST_BUILD_ALPHA_KEY"}},
		Tracing: fileSinkTracing(t),
	}
	stages, sink, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2", len(stages))
	}
	if stages[0].Name() != "identification" {
		t.Fatalf("stages[0].Name() = %q, want %q", stages[0].Name(), "identification")
	}
	if stages[1].Name() != "tracing" {
		t.Fatalf("stages[1].Name() = %q, want %q", stages[1].Name(), "tracing")
	}
}

func TestBuildStages_OmitsIdentificationWhenDisabled(t *testing.T) {
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Tracing: true, Identification: false},
		},
		Tracing: fileSinkTracing(t),
	}
	stages, sink, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	if len(stages) != 1 {
		t.Fatalf("len(stages) = %d, want 1", len(stages))
	}
	if stages[0].Name() != "tracing" {
		t.Fatalf("stages[0].Name() = %q, want %q", stages[0].Name(), "tracing")
	}
	for _, s := range stages {
		if s.Name() == "identification" {
			t.Fatal("identityStage present in buildStages output despite identification being disabled")
		}
	}
}

func TestBuildStages_ReturnsErrorNotPartialStagesOnRegistryFailure(t *testing.T) {
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Tracing: true, Identification: true},
		},
		Agents:  []config.AgentConfig{{AgentID: "agent-a", APIKeyEnv: "AK_GW_TEST_UNSET_ENV_VAR_ABC123"}},
		Tracing: fileSinkTracing(t),
	}
	stages, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err == nil {
		t.Fatal("buildStages with a registry construction failure = nil error, want error")
	}
	if stages != nil {
		t.Fatalf("stages = %v, want nil on error", stages)
	}
}

func TestGatewayNew_ReturnsErrorNotPartialGatewayOnRegistryFailure(t *testing.T) {
	prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	cfg := config.Config{
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Identification: true},
		},
		Agents: []config.AgentConfig{{AgentID: "agent-a", APIKeyEnv: "AK_GW_TEST_UNSET_ENV_VAR_XYZ789"}},
	}
	g, err := New(cfg, zaptest.NewLogger(t), prov)
	if err == nil {
		t.Fatal("New with a registry construction failure = nil error, want error")
	}
	if g != nil {
		t.Fatalf("New returned a non-nil *Gateway alongside an error: %+v", g)
	}
}

// --- full-pipeline integration ----------------------------------------------
//
// These compose the real pipeline.Pipeline with the real identityStage and a
// real identity.Registry, driven over HTTP — the same "full pipeline"
// pattern internal/pipeline's own tests use, since Call.AgentID is never
// echoed back to the caller and so cannot be observed through gateway.New's
// HTTP surface alone (see functional spec's open questions).

func TestIdentityStage_FullPipeline_NoAuthorizationHeaderDeniedBeforeProvider(t *testing.T) {
	stage := &identityStage{registry: newAlphaBetaRegistry(t)}
	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, stage)

	var providerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	if providerCalled {
		t.Fatal("provider was invoked despite a missing Authorization header")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestIdentityStage_FullPipeline_TwoAgentsDifferentShapesNeverCrossAttributed(t *testing.T) {
	stage := &identityStage{registry: newAlphaBetaRegistry(t)}
	capture := &capturingStage{}
	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, stage, capture)

	var providerCalls int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.WriteHeader(http.StatusOK)
	})

	alphaReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	alphaReq.Header.Set("Authorization", "Bearer "+alphaAPIKey)
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, alphaReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("alpha call status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := capture.lastCall().AgentID; got != "agent-alpha" {
		t.Fatalf("alpha call Call.AgentID = %q, want %q", got, "agent-alpha")
	}

	betaToken := mintBetaJWT(t, betaSecret, time.Now().Add(time.Hour))
	betaReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	betaReq.Header.Set("Authorization", "Bearer "+betaToken)
	rec2 := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec2, betaReq)
	if rec2.Code != http.StatusOK {
		t.Fatalf("beta call status = %d, want %d", rec2.Code, http.StatusOK)
	}
	if got := capture.lastCall().AgentID; got != "agent-beta" {
		t.Fatalf("beta call Call.AgentID = %q, want %q", got, "agent-beta")
	}

	if providerCalls != 2 {
		t.Fatalf("provider invoked %d times, want 2 (both credentialed calls must reach it)", providerCalls)
	}
}

func TestIdentityStage_FullPipeline_ZeroAgentsRegisteredDeniesEveryProxiedRequest(t *testing.T) {
	reg, err := identity.NewRegistry(nil)
	if err != nil {
		t.Fatalf("identity.NewRegistry(nil): %v", err)
	}
	stage := &identityStage{registry: reg}

	tests := []struct {
		name   string
		header string
	}{
		{"without a credential", ""},
		{"with a plausible-looking credential", "Bearer some-plausible-credential"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, stage)
			var providerCalled bool
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				providerCalled = true
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			p.Handler(next).ServeHTTP(rec, req)

			if providerCalled {
				t.Fatal("provider was invoked despite zero agents being registered")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestGateway_HealthzBypassesPipelineWithIdentificationEnabledAndZeroAgents(t *testing.T) {
	prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}
	cfg := config.Config{
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Identification: true},
		},
		Agents: nil, // zero agents registered
	}
	g, err := New(cfg, zaptest.NewLogger(t), prov)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want %d: it must bypass the pipeline even with identification enabled and zero agents registered", rec.Code, http.StatusOK)
	}
}

func TestGateway_ProxiedRequestDeniedWithIdentificationEnabledAndZeroAgents(t *testing.T) {
	var providerCalled bool
	prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusOK)
	})}
	cfg := config.Config{
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Identification: true},
		},
		Agents: nil,
	}
	g, err := New(cfg, zaptest.NewLogger(t), prov)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(rec, req)

	if providerCalled {
		t.Fatal("provider was invoked for a proxied request despite zero agents being registered")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

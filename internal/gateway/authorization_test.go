package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/cardeahq/cardea-gateway/internal/authorization"
	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
)

// authorizationStage must satisfy pipeline.Stage; it never observes
// responses, so it deliberately does not implement pipeline.ResponseObserver.
var _ pipeline.Stage = (*authorizationStage)(nil)

var _ = func() bool {
	var s pipeline.Stage = &authorizationStage{}
	if _, isObserver := s.(pipeline.ResponseObserver); isObserver {
		panic("authorizationStage implements ResponseObserver; authorization never observes a response")
	}
	return true
}()

const (
	authzSecretOneEnv = "AK_GW_TEST_AUTHZ_SECRET_1"
	authzSecretTwoEnv = "AK_GW_TEST_AUTHZ_SECRET_2"
	authzSecretOne    = "gateway-test-shared-secret-one"
	authzSecretTwo    = "gateway-test-shared-secret-two"
)

// newTwoSecretGate builds a real two-secret authorization.Gate, shared by
// every authorizationStage test below so denial-category tests run against
// one fixed, known gate.
func newTwoSecretGate(t *testing.T) *authorization.Gate {
	t.Helper()
	t.Setenv(authzSecretOneEnv, authzSecretOne)
	t.Setenv(authzSecretTwoEnv, authzSecretTwo)
	g, err := authorization.NewGate([]string{authzSecretOneEnv, authzSecretTwoEnv})
	if err != nil {
		t.Fatalf("authorization.NewGate: %v", err)
	}
	return g
}

func TestAuthorizationStage_Name(t *testing.T) {
	s := &authorizationStage{gate: newTwoSecretGate(t)}
	if got := s.Name(); got != "authorization" {
		t.Fatalf("Name() = %q, want %q", got, "authorization")
	}
}

func TestAuthorizationStage_ValidSecretAllows(t *testing.T) {
	s := &authorizationStage{gate: newTwoSecretGate(t)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+authzSecretOne)
	call := &pipeline.Call{Request: req}

	decision, err := s.Request(context.Background(), call)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if decision != pipeline.Allow() {
		t.Fatalf("decision = %+v, want pipeline.Allow()", decision)
	}
}

func TestAuthorizationStage_EitherOfTwoConfiguredSecretsAllows(t *testing.T) {
	s := &authorizationStage{gate: newTwoSecretGate(t)}
	for _, secret := range []string{authzSecretOne, authzSecretTwo} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		call := &pipeline.Call{Request: req}

		decision, err := s.Request(context.Background(), call)
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		if decision != pipeline.Allow() {
			t.Fatalf("decision for secret %q = %+v, want pipeline.Allow()", secret, decision)
		}
	}
}

func TestAuthorizationStage_DenialCategoriesShareStatusAndMessageButDistinctReasons(t *testing.T) {
	s := &authorizationStage{gate: newTwoSecretGate(t)}

	tests := []struct {
		name       string
		header     string
		wantReason string
	}{
		{
			name:       "no credential presented",
			header:     "",
			wantReason: string(authorization.ReasonNoCredential),
		},
		{
			name:       "credential in an unrecognized shape",
			header:     "Basic dXNlcjpwYXNz",
			wantReason: string(authorization.ReasonUnrecognizedShape),
		},
		{
			name:       "bearer value matches no configured secret",
			header:     "Bearer no-such-secret",
			wantReason: string(authorization.ReasonNoMatch),
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
			want := pipeline.Deny(http.StatusUnauthorized, authorizationDenialMessage, tt.wantReason)
			if decision != want {
				t.Fatalf("decision = %+v, want %+v", decision, want)
			}
			seenReasons[tt.wantReason] = true
		})
	}
	if len(seenReasons) != len(tests) {
		t.Fatalf("only %d distinct reasons across %d denial categories, want all three distinct", len(seenReasons), len(tests))
	}
}

func TestAuthorizationStage_ZeroSecretsDeniesEveryCall(t *testing.T) {
	g, err := authorization.NewGate(nil)
	if err != nil {
		t.Fatalf("authorization.NewGate(nil): %v", err)
	}
	s := &authorizationStage{gate: g}

	tests := []struct {
		name       string
		header     string
		wantReason authorization.DenialReason
	}{
		{"without a credential", "", authorization.ReasonNoCredential},
		{"with a plausible-looking credential", "Bearer some-plausible-credential", authorization.ReasonNoMatch},
	}
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
			want := pipeline.Deny(http.StatusUnauthorized, authorizationDenialMessage, string(tt.wantReason))
			if decision != want {
				t.Fatalf("decision = %+v, want %+v (zero secrets configured must still deny)", decision, want)
			}
		})
	}
}

// --- buildStages ---------------------------------------------------------------

func authzCfg(t *testing.T, authzEnabled bool, secretEnv string) config.Config {
	t.Helper()
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Authorization: authzEnabled, Identification: true},
		},
	}
	if authzEnabled {
		t.Setenv(secretEnv, "build-stages-test-secret")
		cfg.Authorization = config.AuthorizationConfig{APIKeyEnvs: []string{secretEnv}}
	}
	return cfg
}

func TestBuildStages_PrependsAuthorizationAheadOfIdentificationWhenBothEnabled(t *testing.T) {
	cfg := authzCfg(t, true, "AK_GW_TEST_BUILD_AUTHZ_SECRET")
	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if sink != nil {
		t.Cleanup(func() { _ = sink.Close() })
	}
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2", len(stages))
	}
	if stages[0].Name() != "authorization" {
		t.Fatalf("stages[0].Name() = %q, want %q", stages[0].Name(), "authorization")
	}
	if stages[1].Name() != "identification" {
		t.Fatalf("stages[1].Name() = %q, want %q", stages[1].Name(), "identification")
	}
}

func TestBuildStages_OmitsAuthorizationWhenDisabled(t *testing.T) {
	cfg := authzCfg(t, false, "")
	stages, sink, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if sink != nil {
		t.Cleanup(func() { _ = sink.Close() })
	}
	for _, s := range stages {
		if s.Name() == "authorization" {
			t.Fatal("authorizationStage present in buildStages output despite authorization being disabled")
		}
	}
}

func TestBuildStages_ReturnsErrorNotPartialStagesOnGateFailure(t *testing.T) {
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Authorization: true},
		},
		Authorization: config.AuthorizationConfig{APIKeyEnvs: []string{"AK_GW_TEST_UNSET_AUTHZ_ENV_VAR_ABC123"}},
	}
	stages, _, _, _, err := buildStages(cfg, zaptest.NewLogger(t))
	if err == nil {
		t.Fatal("buildStages with a gate construction failure = nil error, want error")
	}
	if stages != nil {
		t.Fatalf("stages = %v, want nil on error", stages)
	}
}

func TestGatewayNew_ReturnsErrorNotPartialGatewayOnGateFailure(t *testing.T) {
	prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	cfg := config.Config{
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{Authorization: true},
		},
		Authorization: config.AuthorizationConfig{APIKeyEnvs: []string{"AK_GW_TEST_UNSET_AUTHZ_ENV_VAR_XYZ789"}},
	}
	g, err := New(cfg, zaptest.NewLogger(t), prov)
	if err == nil {
		t.Fatal("New with a gate construction failure = nil error, want error")
	}
	if g != nil {
		t.Fatalf("New returned a non-nil *Gateway alongside an error: %+v", g)
	}
}

// --- full-pipeline integration ---------------------------------------------
//
// authorization enabled and failing denies the call before identification
// runs: Call.AgentID stays "".

func TestAuthorizationStage_FullPipeline_DeniedBeforeIdentificationRuns(t *testing.T) {
	authzStage := &authorizationStage{gate: newTwoSecretGate(t)}
	identStage := &identificationStage{registry: newOneTypeRegistry(t)}
	capture := &capturingStage{}
	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, authzStage, identStage, capture)

	var providerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil) // no Authorization header
	req.Header.Set(testAgentTypeHeader, testAgentTypeValue)          // would match identification, if it ran
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if providerCalled {
		t.Fatal("provider was invoked despite a failed authorization")
	}
	if capture.lastCall() != nil {
		t.Fatal("a later stage ran despite authorization denying the call")
	}
}

func TestAuthorizationStage_FullPipeline_PassingAuthorizationLetsIdentificationRun(t *testing.T) {
	authzStage := &authorizationStage{gate: newTwoSecretGate(t)}
	identStage := &identificationStage{registry: newOneTypeRegistry(t)}
	capture := &capturingStage{}
	p := pipeline.New(zaptest.NewLogger(t), 1<<20, nil, authzStage, identStage, capture)

	var providerCalled bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+authzSecretOne)
	req.Header.Set(testAgentTypeHeader, testAgentTypeValue)
	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !providerCalled {
		t.Fatal("provider was never invoked despite a passing authorization")
	}
	if got := capture.lastCall().AgentID; got != testAgentTypeID {
		t.Fatalf("Call.AgentID = %q, want %q (identification ran after authorization passed)", got, testAgentTypeID)
	}
}

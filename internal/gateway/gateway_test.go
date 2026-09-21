package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

// stubProvider is a minimal provider.Provider for tests that need to
// observe whether the provider was reached.
type stubProvider struct {
	handler http.Handler
}

func (stubProvider) Name() string            { return "stub" }
func (p stubProvider) Handler() http.Handler { return p.handler }

func TestGateway_HealthzBypassesPipelineRegardlessOfStageConfig(t *testing.T) {
	tests := []struct {
		name          string
		tokenCounting bool
	}{
		{"pipeline stage enabled", true},
		{"pipeline stage disabled", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			})}
			cfg := config.Config{
				Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
				Pipeline: config.PipelineConfig{
					MaxBodyBytes: 1 << 20,
					Stages:       config.StagesConfig{TokenCounting: tt.tokenCounting},
				},
			}
			g, err := New(cfg, zaptest.NewLogger(t), prov)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			g.server.Handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("/healthz status = %d, want %d: it must bypass the pipeline and the provider entirely", rec.Code, http.StatusOK)
			}
		})
	}
}

func TestGateway_ForwardsAllowedCallsThroughPipelineToProvider(t *testing.T) {
	var providerCalled bool
	prov := stubProvider{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalled = true
		w.WriteHeader(http.StatusOK)
	})}

	cfg := config.Config{
		Server: config.ServerConfig{ListenAddr: "127.0.0.1:0"},
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{TokenCounting: true},
		},
	}
	g, err := New(cfg, zaptest.NewLogger(t), prov)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(rec, req)

	if !providerCalled {
		t.Fatal("provider was never invoked for an allowed call routed through the pipeline")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestBuildStages_RespectsConfigBooleans(t *testing.T) {
	tests := []struct {
		name          string
		tokenCounting bool
		wantLen       int
	}{
		{"token counting enabled", true, 1},
		{"token counting disabled", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Pipeline: config.PipelineConfig{
					MaxBodyBytes: 1 << 20,
					Stages:       config.StagesConfig{TokenCounting: tt.tokenCounting},
				},
			}
			stages, err := buildStages(cfg, zaptest.NewLogger(t))
			if err != nil {
				t.Fatalf("buildStages: %v", err)
			}
			if len(stages) != tt.wantLen {
				t.Fatalf("len(buildStages(...)) = %d, want %d", len(stages), tt.wantLen)
			}
		})
	}
}

func TestBuildStages_TokenCountingStageIsNamedAfterItsConfigKey(t *testing.T) {
	cfg := config.Config{
		Pipeline: config.PipelineConfig{
			MaxBodyBytes: 1 << 20,
			Stages:       config.StagesConfig{TokenCounting: true},
		},
	}
	stages, err := buildStages(cfg, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("len(buildStages(...)) = %d, want 1", len(stages))
	}
	if got := stages[0].Name(); got != "token_counting" {
		t.Fatalf("stages[0].Name() = %q, want %q (matching its pipeline.stages config key)", got, "token_counting")
	}
}

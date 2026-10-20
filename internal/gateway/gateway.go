// Package gateway implements the Gateway's HTTP server.
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"go.uber.org/zap"

	"github.com/cardeahq/cardea-gateway/internal/authorization"
	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/filtering"
	"github.com/cardeahq/cardea-gateway/internal/identification"
	"github.com/cardeahq/cardea-gateway/internal/pipeline"
	"github.com/cardeahq/cardea-gateway/internal/policy"
	"github.com/cardeahq/cardea-gateway/internal/provider"
	"github.com/cardeahq/cardea-gateway/internal/trace"
)

type Gateway struct {
	cfg          config.Config
	logger       *zap.Logger
	provider     provider.Provider
	sink         trace.CloseableSink
	engine       *policy.Engine
	filterEngine *filtering.Engine
	server       *http.Server
}

func New(cfg config.Config, logger *zap.Logger, prov provider.Provider) (*Gateway, error) {
	mux := http.NewServeMux()
	g := &Gateway{
		cfg:      cfg,
		logger:   logger,
		provider: prov,
	}

	stages, sink, engine, filterEngine, err := buildStages(cfg, logger)
	if err != nil {
		return nil, err
	}
	g.sink = sink
	g.engine = engine
	g.filterEngine = filterEngine
	var pipelineSink trace.Sink
	if sink != nil {
		pipelineSink = sink
	}
	pl := pipeline.New(logger, cfg.Pipeline.MaxBodyBytes, pipelineSink, stages...)

	mux.HandleFunc("/healthz", g.handleHealthz)
	mux.Handle("/", pl.Handler(prov.Handler()))

	g.server = &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: mux,
	}
	return g, nil
}

// ReloadPolicy reloads the policy from disk (the SIGHUP path). With no
// policy configured it logs that and returns nil.
func (g *Gateway) ReloadPolicy() error {
	if g.engine == nil || g.cfg.Policies.Path == "" {
		g.logger.Info("no policy configured; nothing to reload")
		return nil
	}
	return g.engine.Reload("signal")
}

// Run blocks until ctx is cancelled or the server fails, then closes the
// trace sink.
func (g *Gateway) Run(ctx context.Context) error {
	if g.sink != nil {
		defer func() { _ = g.sink.Close() }()
	}
	if g.engine != nil {
		go func() {
			if err := g.engine.Watch(ctx); err != nil {
				g.logger.Error("policy file watching stopped; signal reload still works", zap.Error(err))
			}
		}()
	}
	ln, err := net.Listen("tcp", g.server.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", g.server.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		g.logger.Info("gateway listening", zap.String("addr", g.server.Addr))
		errCh <- g.server.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		return g.server.Shutdown(context.Background())
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

func (g *Gateway) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// buildStages walks the RFC's fixed stage order and appends the
// constructor for each stage whose config boolean is true. A new stage
// spec adds its constructor at its position here. The returned sink is
// nil exactly when tracing is disabled, and the engine exactly when the
// policies stage is.
func buildStages(cfg config.Config, logger *zap.Logger) ([]pipeline.Stage, trace.CloseableSink, *policy.Engine, *filtering.Engine, error) {
	var stages []pipeline.Stage
	var sink trace.CloseableSink

	if cfg.Pipeline.Stages.Authorization {
		gate, err := authorization.NewGate(cfg.Authorization.APIKeyEnvs)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("building authorization gate: %w", err)
		}
		stages = append(stages, &authorizationStage{gate: gate})
	}

	var identRegistry *identification.Registry
	if cfg.Pipeline.Stages.Identification {
		var err error
		identRegistry, err = identification.NewRegistry(cfg.Identification.AgentTypesDir)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("building identification registry: %w", err)
		}
		stages = append(stages, &identificationStage{registry: identRegistry})
	}

	if cfg.Pipeline.Stages.Tracing {
		var agentSources map[string][]config.SessionSource
		if identRegistry != nil {
			agentSources = identRegistry.SessionSources()
		}
		tracer, err := trace.NewTracer(cfg.Tracing, agentSources, logger)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("building tracer: %w", err)
		}
		sink, err = buildSink(cfg.Tracing, logger)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		stages = append(stages, &tracingStage{tracer: tracer})
	}
	var engine *policy.Engine
	if cfg.Pipeline.Stages.Policies {
		var err error
		engine, err = policy.New(cfg.Policies, cfg.Provider.Name, logger)
		if err != nil {
			if sink != nil {
				_ = sink.Close()
			}
			return nil, nil, nil, nil, fmt.Errorf("building policy engine: %w", err)
		}
		if cfg.Policies.Path != "" && !cfg.Pipeline.Stages.Identification {
			logger.Warn("a policy is configured but identification is off: rules cannot tell agents apart")
		}
		stages = append(stages, &policiesStage{engine: engine, logger: logger})
	}
	var filterEngine *filtering.Engine
	if cfg.Pipeline.Stages.Filtering {
		var err error
		filterEngine, err = filtering.New(cfg.Filtering, cfg.Provider.Name, logger)
		if err != nil {
			if sink != nil {
				_ = sink.Close()
			}
			return nil, nil, nil, nil, fmt.Errorf("building filtering engine: %w", err)
		}
		stages = append(stages, &filteringStage{engine: filterEngine, logger: logger})
	}
	return stages, sink, engine, filterEngine, nil
}

// buildSink returns the sink cfg.Sink selects: the local log, the Cardea
// sink, or both in a MultiSink. On error nothing it opened stays open.
func buildSink(cfg config.TracingConfig, logger *zap.Logger) (trace.CloseableSink, error) {
	var sinks []trace.CloseableSink
	if cfg.Sink.UsesCardea() {
		cs, err := trace.NewCardeaSink(cfg.Sink.Cardea, logger)
		if err != nil {
			return nil, fmt.Errorf("building cardea sink: %w", err)
		}
		sinks = append(sinks, cs)
	}
	if cfg.Sink.UsesLogging() {
		ls, err := trace.NewLogSink(cfg.Sink.Path)
		if err != nil {
			for _, s := range sinks {
				_ = s.Close()
			}
			return nil, err
		}
		sinks = append(sinks, ls)
	}
	if len(sinks) == 1 {
		return sinks[0], nil
	}
	return trace.NewMultiSink(sinks...), nil
}

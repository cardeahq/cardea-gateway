// Package gateway implements agentkeeper's HTTP gateway server.
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"go.uber.org/zap"

	"github.com/agentkeeper/agentkeeper/internal/config"
	"github.com/agentkeeper/agentkeeper/internal/identity"
	"github.com/agentkeeper/agentkeeper/internal/pipeline"
	"github.com/agentkeeper/agentkeeper/internal/provider"
)

type Gateway struct {
	cfg      config.Config
	logger   *zap.Logger
	provider provider.Provider
	server   *http.Server
}

func New(cfg config.Config, logger *zap.Logger, prov provider.Provider) (*Gateway, error) {
	mux := http.NewServeMux()
	g := &Gateway{
		cfg:      cfg,
		logger:   logger,
		provider: prov,
	}

	stages, err := buildStages(cfg, logger)
	if err != nil {
		return nil, err
	}
	pl := pipeline.New(logger, cfg.Pipeline.MaxBodyBytes, stages...)

	mux.HandleFunc("/healthz", g.handleHealthz)
	mux.Handle("/", pl.Handler(prov.Handler()))

	g.server = &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: mux,
	}
	return g, nil
}

// Run blocks until ctx is cancelled or the server fails.
func (g *Gateway) Run(ctx context.Context) error {
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
// spec adds its constructor at its position here.
func buildStages(cfg config.Config, logger *zap.Logger) ([]pipeline.Stage, error) {
	var stages []pipeline.Stage
	if cfg.Pipeline.Stages.Identification {
		registry, err := identity.NewRegistry(cfg.Agents)
		if err != nil {
			return nil, fmt.Errorf("building agent registry: %w", err)
		}
		stages = append(stages, &identityStage{registry: registry})
	}
	if cfg.Pipeline.Stages.TokenCounting {
		stages = append(stages, &tokenStage{logger: logger, counter: &tokenCounter{}})
	}
	return stages, nil
}

// Package gateway implements agentkeeper's HTTP gateway server.
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"go.uber.org/zap"

	"github.com/agentkeeper/agentkeeper/internal/config"
	"github.com/agentkeeper/agentkeeper/internal/provider"
)

type Gateway struct {
	cfg      config.Config
	logger   *zap.Logger
	provider provider.Provider
	server   *http.Server
}

func New(cfg config.Config, logger *zap.Logger, prov provider.Provider) *Gateway {
	mux := http.NewServeMux()
	g := &Gateway{
		cfg:      cfg,
		logger:   logger,
		provider: prov,
	}
	tokens := &tokenCounter{}

	mux.HandleFunc("/healthz", g.handleHealthz)
	mux.Handle("/", chain(prov.Handler(), stages(logger, tokens)...))

	g.server = &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: mux,
	}
	return g
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

// chain wraps final with mw in order, so mw[0] runs first and final runs
// last.
func chain(final http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	h := final
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

func logRequests(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Info("request", zap.String("method", r.Method), zap.String("path", r.URL.Path))
			next.ServeHTTP(w, r)
		})
	}
}

func displayTokenUsage(logger *zap.Logger, tokens *tokenCounter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			logger.Info(
				"token usage",
				zap.Int64("total_sent", tokens.sent.Load()),
				zap.Int64("total_received", tokens.received.Load()),
			)
		})
	}
}

func stages(logger *zap.Logger, tokens *tokenCounter) []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		displayTokenUsage(logger, tokens),
		countTokensStage(logger, tokens),
		//logRequests(logger),
	}
}

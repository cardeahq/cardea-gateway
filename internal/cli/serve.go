package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cardeahq/cardea-gateway/internal/gateway"
	"github.com/cardeahq/cardea-gateway/internal/logging"
	"github.com/cardeahq/cardea-gateway/internal/provider"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the Cardea Gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			logger, err := logging.New(cfg.Logging)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()

			var prov provider.Provider
			if cfg.Provider.Name == "anthropic" {
				prov, err = provider.NewAnthropicProvider(cfg.Provider.BaseURL, cfg.Provider.APIKeyEnv)
			} else {
				prov, err = provider.NewHTTPProxy(cfg.Provider.Name, cfg.Provider.BaseURL)
			}
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			gw, err := gateway.New(cfg, logger, prov)
			if err != nil {
				return err
			}

			// Registered even with no policy: SIGHUP's default action ends the process.
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-hup:
						_ = gw.ReloadPolicy() // a failure is already logged by the engine
					}
				}
			}()
			return gw.Run(ctx)
		},
	}
}

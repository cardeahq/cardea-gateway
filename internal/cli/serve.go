package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cardeahq/cardea-gateway/internal/config"
	"github.com/cardeahq/cardea-gateway/internal/gateway"
	"github.com/cardeahq/cardea-gateway/internal/logging"
	"github.com/cardeahq/cardea-gateway/internal/provider"
	"github.com/cardeahq/cardea-gateway/internal/routing"
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

			set, table, err := buildProviders(cfg)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			gw, err := gateway.New(cfg, logger, set, table)
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

// buildProviders builds the provider.Set and routing.Table gateway.New
// needs, from either the provider list (list mode) or the single
// provider block (single mode) — see specs/0009-technical, Design ->
// internal/cli.
func buildProviders(cfg config.Config) (*provider.Set, *routing.Table, error) {
	if cfg.Routing.ListMode() {
		set, err := provider.NewSet(cfg.Routing.Providers)
		if err != nil {
			return nil, nil, err
		}
		routes := make([]routing.Route, 0, len(cfg.Routing.Providers))
		for _, e := range cfg.Routing.Providers {
			routes = append(routes, routing.Route{
				Provider: e.Name,
				Kind:     e.Kind,
				Formats:  []routing.Format{routing.FormatOf(e.Kind)},
				Patterns: e.Models,
			})
		}
		defaults := make(map[routing.Format]string, len(cfg.Routing.Defaults))
		for format, name := range cfg.Routing.Defaults {
			defaults[routing.Format(format)] = name
		}
		table, err := routing.NewTable(routes, defaults)
		if err != nil {
			return nil, nil, err
		}
		return set, table, nil
	}

	var prov provider.Provider
	var err error
	kind := ""
	switch cfg.Provider.Name {
	case "anthropic":
		kind = "anthropic"
		prov, err = provider.NewAnthropicProvider(cfg.Provider.BaseURL, cfg.Provider.APIKeyEnv)
	case "mistral":
		kind = "mistral"
		prov, err = provider.NewMistralProvider(cfg.Provider.BaseURL, cfg.Provider.APIKeyEnv)
	case "gemini":
		kind = "gemini"
		prov, err = provider.NewGeminiProvider(cfg.Provider.BaseURL, cfg.Provider.APIKeyEnv)
	default:
		prov, err = provider.NewHTTPProxy(cfg.Provider.Name, cfg.Provider.BaseURL)
	}
	if err != nil {
		return nil, nil, err
	}

	route := routing.Route{Provider: cfg.Provider.Name, Kind: kind, Formats: []routing.Format{routing.FormatOf(kind)}, Single: true}
	return provider.NewSingleSet(cfg.Provider.Name, prov), routing.NewSingleTable(route), nil
}

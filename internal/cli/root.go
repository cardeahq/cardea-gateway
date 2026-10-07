// Package cli implements the Gateway's Cobra command tree.
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	"github.com/cardeahq/cardea-gateway/internal/config"
)

var cfgFile, envFile string

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gateway",
		Short: "Cardea Gateway is an LLM gateway for security, privacy, and observability",
		Long: `Cardea Gateway shields and contains LLM usage behind a single gateway.

It provides security, privacy, and observability controls for LLM traffic,
lets you switch between LLM providers without changing application code,
and enforces user-defined rules such as circuit breakers and token
compaction policies.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./cardea-gateway.yaml)")

	root.PersistentFlags().StringVar(&envFile, "env-file", ".env", "dotenv file loaded before the config (missing file is ignored; real environment variables win)")

	root.AddCommand(newServeCmd())
	root.AddCommand(newVersionCmd())

	return root
}

func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	if err := godotenv.Load(envFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return config.Config{}, fmt.Errorf("loading %s: %w", envFile, err)
	}
	return config.Load(cfgFile)
}

// Package cli implements agentkeeper's Cobra command tree.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/agentkeeper/agentkeeper/internal/config"
)

var cfgFile string

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "agentkeeper",
		Short: "agentkeeper is an LLM gateway for security, privacy, and observability",
		Long: `agentkeeper shields and contains LLM usage behind a single gateway.

It provides security, privacy, and observability controls for LLM traffic,
lets you switch between LLM providers without changing application code,
and enforces user-defined rules such as circuit breakers and token
compaction policies.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./agentkeeper.yaml)")

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
	return config.Load(cfgFile)
}

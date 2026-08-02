package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version, commit, and date are set at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/agentkeeper/agentkeeper/internal/cli.version=$(VERSION)"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the agentkeeper version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "agentkeeper %s (commit %s, built %s)\n", version, commit, date)
			return nil
		},
	}
}

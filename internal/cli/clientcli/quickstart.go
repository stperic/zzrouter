package clientcli

import (
	"github.com/spf13/cobra"
)

// NewQuickstartCmd creates the quickstart command
func NewQuickstartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quickstart",
		Short: "Get up and running with zzRouter",
		Long: `Start your zzRouter journey. Open the interactive dashboard.

The server should already be running (started by the install script).

Examples:
  zzrouter quickstart`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(withQuickstart())
		},
	}
	return cmd
}

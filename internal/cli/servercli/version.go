package servercli

import (
	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/version"
)

// NewVersionCmd creates the version subcommand.
func NewVersionCmd() *cobra.Command {
	var jsonOut, detailed bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long: `Display comprehensive version and build information for zzrouter-node.

Shows version number, build information, git details, and capabilities.
Use --json flag for machine-readable output.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return version.Print(cmd.OutOrStdout(), version.PrintSpec{
				BinaryName:   "zzrouter-node",
				ServiceType:  version.ServiceTypeNode,
				Capabilities: version.NodeCapabilities,
			}, versionMode(jsonOut, detailed))
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output version information in JSON format")
	cmd.Flags().BoolVar(&detailed, "detailed", false, "Show detailed version information")

	return cmd
}

func versionMode(jsonOut, detailed bool) version.PrintMode {
	switch {
	case jsonOut:
		return version.PrintJSON
	case detailed:
		return version.PrintDetailed
	default:
		return version.PrintSimple
	}
}

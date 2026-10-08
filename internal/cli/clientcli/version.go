package clientcli

import (
	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/version"
)

// NewVersionCmd creates the client version subcommand.
func NewVersionCmd() *cobra.Command {
	var jsonOut, detailed bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long: `Display comprehensive version and build information for zzrouter.

Shows version number, build information, git details, and capabilities.
Use --json flag for machine-readable output.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return version.Print(cmd.OutOrStdout(), version.PrintSpec{
				BinaryName:        "zzRouter Client",
				ServiceType:       version.ServiceTypeClient,
				Capabilities:      version.ClientCapabilities,
				WithCompatibility: true,
			}, versionMode(jsonOut, detailed))
		},
	}

	cmd.Flags().BoolVarP(&jsonOut, "json", "j", false, "Output in JSON format")
	cmd.Flags().BoolVarP(&detailed, "detailed", "d", false, "Show detailed version and build information")

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

package servercli

import (
	"encoding/json"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
)

func newNodeConfigIdentityCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{Use: "identity", Short: "Read the installation's non-secret connection settings", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if dir == "" {
			dir = config.Paths().GetConfigDir()
		}
		identity, err := config.ReadInstallationIdentity(cmd.Context(), dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(identity)
	}}
	cmd.Flags().StringVarP(&dir, "dir", "C", "", "Installed configuration directory")
	cmd.Flags().Bool("json", true, "Return safe identity fields as JSON")
	return cmd
}

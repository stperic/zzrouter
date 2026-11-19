package servercli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/service"
)

func newNodeConfigCredentialsCmd() *cobra.Command {
	var dir, legacy string
	var legacyActive, checkOnly bool
	cmd := &cobra.Command{
		Use: "credentials", Short: "Prepare installation credentials without displaying them", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				dir = config.Paths().GetConfigDir()
			}
			if err := service.CheckInstallationCredentialOwner(dir); err != nil {
				return err
			}
			if checkOnly {
				cmd.Println("Installation credential ownership is compatible.")
				return nil
			}
			if legacyActive && legacy == "" {
				return fmt.Errorf("--legacy-active requires --legacy-file")
			}
			_, err := config.EnsureInstallationCredentials(cmd.Context(), dir, config.InstallationCredentialOptions{LegacyFile: legacy, LegacyOverrides: legacyActive})
			if err != nil {
				return err
			}
			cmd.Printf("Installation credentials ready in %s\n", filepath.Join(dir, ".env"))
			return nil
		},
	}
	cmd.Flags().StringVarP(&dir, "dir", "C", "", "Installation configuration directory")
	cmd.Flags().StringVar(&legacy, "legacy-file", "", "Legacy environment file to migrate")
	cmd.Flags().BoolVar(&legacyActive, "legacy-active", false, "The installed service currently imports the legacy file")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Check service account compatibility without changing files")
	return cmd
}

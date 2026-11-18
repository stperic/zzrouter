package servercli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/recovery"
)

// NewRecoveryCmd exposes local offline inventory, backup, checking and staging.
func NewRecoveryCmd() *cobra.Command {
	var dir string
	command := &cobra.Command{Use: "recovery", Short: "Inspect, back up and stage an installation recovery"}
	command.PersistentFlags().StringVarP(&dir, "dir", "C", config.Paths().GetConfigDir(), "Installation configuration directory")
	command.AddCommand(&cobra.Command{Use: "inventory", Short: "Show included state and separate recovery dependencies", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		inventory, err := recovery.Read(cmd.Context(), dir)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(inventory)
	}})
	var output, keyOutput string
	backup := &cobra.Command{Use: "backup", Short: "Back up a stopped node; export its protection key separately", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if output == "" {
			return fmt.Errorf("--output is required")
		}
		manifest, err := recovery.Backup(cmd.Context(), dir, output, keyOutput)
		if err != nil {
			return err
		}
		if _, err := recovery.Check(cmd.Context(), output, keyOutput); err != nil {
			return fmt.Errorf("backup check failed; do not use this archive: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Saved and checked %d files for %s. The archive contains secrets; keep it in encrypted backup storage.\n", len(manifest.Files), manifest.Inventory.Node)
		if len(manifest.MissingOptional) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%d optional state paths were absent; inspect missing_optional in the archive manifest for the list. Previously recorded state must be available.\n", len(manifest.MissingOptional))
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Model weights, runtime executables, service installation and listed external dependencies need separate recovery. Run recovery inventory for the scope.")
		return nil
	}}
	backup.Flags().StringVar(&output, "output", "", "New archive path in secure backup storage")
	backup.Flags().StringVar(&keyOutput, "key-output", "", "New protection-key path in separate secure storage")
	command.AddCommand(backup)
	for _, operation := range []string{"check", "restore"} {
		var keyFile, destination string
		child := &cobra.Command{Use: operation + " ARCHIVE", Short: map[string]string{"check": "Check archive integrity, authority state and the matching key", "restore": "Validate and restore into a new staging directory; never activate it"}[operation], Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var manifest recovery.Manifest
			var err error
			if operation == "check" {
				manifest, err = recovery.Check(cmd.Context(), args[0], keyFile)
			} else {
				manifest, err = recovery.Restore(cmd.Context(), args[0], keyFile, destination)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Validated %d files for %s (%s).\n", len(manifest.Files), manifest.Inventory.Node, manifest.Inventory.Platform)
			if operation == "restore" {
				fmt.Fprintln(cmd.OutOrStdout(), "Staged only. Keep the node stopped, review manifest.json and restore paths, ownership and separate dependencies before activation. See docs/recovery.md.")
			}
			return nil
		}}
		child.Flags().StringVar(&keyFile, "key-file", "", "Separate matching protection-key backup")
		if operation == "restore" {
			child.Flags().StringVar(&destination, "to", "", "New, absent staging directory")
		}
		command.AddCommand(child)
	}
	return command
}

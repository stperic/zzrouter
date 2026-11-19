package servercli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/recovery"
)

// NewRecoveryCmd exposes local offline inventory, backup, checking and staging.
func NewRecoveryCmd() *cobra.Command {
	var dir string
	command := &cobra.Command{Use: "recovery", Short: "Inspect, back up and stage an installation recovery"}
	command.PersistentFlags().StringVarP(&dir, "dir", "C", config.Paths().GetConfigDir(), "Installation configuration directory")
	command.AddCommand(&cobra.Command{Use: "inventory", Short: "Show included state, separate recovery dependencies and the access store", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		inventory, err := recovery.Read(cmd.Context(), dir)
		if err != nil {
			return err
		}
		store, err := recovery.InspectStore(cmd.Context(), dir)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(struct {
			Inventory recovery.Inventory    `json:"inventory"`
			Store     recovery.StoreSection `json:"store"`
		}{inventory, store})
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
		verb, note := "Saved and checked", ""
		if _, err := recovery.Check(cmd.Context(), output, keyOutput); err != nil {
			// A newer release's database is captured for safety; only that release reads it.
			var refusal *sqlstore.Refusal
			if !errors.As(err, &refusal) || refusal.Reason != sqlstore.SchemaNewer {
				return fmt.Errorf("backup check failed; do not use this archive: %w", err)
			}
			verb, note = "Saved", "A newer release wrote the access database, so this release checked everything else in the archive: check and restore it with that release.\n"
		}
		describeBundle(cmd.OutOrStdout(), verb, manifest)
		fmt.Fprint(cmd.OutOrStdout(), note+"The archive contains secrets; keep it in encrypted backup storage.\n")
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
			describeBundle(cmd.OutOrStdout(), "Validated", manifest)
			if staged := manifest.Staged; staged != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Prepared the staged access database as generation %s: once it is activated, sessions, delegated tokens and app-key references issued before this restore do not resolve.\n", staged.Generation)
			}
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
	// An interrupt or a service stop cancels the operation, which removes its
	// temporaries and leaves no output.
	for _, child := range command.Commands() {
		run := child.RunE
		child.RunE = func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			cmd.SetContext(ctx)
			return run(cmd, args)
		}
	}
	return command
}

// describeBundle names a bundle's profile, installation and access store.
func describeBundle(w io.Writer, verb string, m recovery.Manifest) {
	fmt.Fprintf(w, "%s %d files, profile %s", verb, len(m.Files), m.Profile)
	if m.Inventory != nil {
		fmt.Fprintf(w, ", for %s (%s)", m.Inventory.Node, m.Inventory.Platform)
	}
	fmt.Fprintln(w, ".")
	if s := m.Store; s != nil {
		fmt.Fprintf(w, "Access store %s: generation %s, schema version %d, revision %d, sequence %d.\n", s.StoreID, s.Generation, s.SchemaVersion, s.Revision, s.Sequence)
	} else {
		fmt.Fprintln(w, "No access store: a worker, or a node that never ran as a coordinator.")
	}
}

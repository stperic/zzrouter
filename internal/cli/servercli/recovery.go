package servercli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/recovery"
)

// NewRecoveryCmd exposes local offline inventory, backup, checking, staging
// and activation.
func NewRecoveryCmd() *cobra.Command {
	var dir string
	command := &cobra.Command{Use: "recovery", Short: "Inspect, back up, stage and activate an installation recovery"}
	command.PersistentFlags().StringVarP(&dir, "dir", "C", config.Paths().GetConfigDir(), "Installation configuration directory")
	command.AddCommand(&cobra.Command{Use: "inventory", Short: "Show included state, separate recovery dependencies and the access store", Args: cobra.NoArgs, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		// The store is inspected even without a layout: a store refused at
		// the first start stops the node before it records one.
		inventory, readErr := recovery.Read(cmd.Context(), dir)
		store, inspectErr := recovery.InspectStore(cmd.Context(), dir)
		printed := struct {
			Inventory *recovery.Inventory   `json:"inventory"`
			Store     recovery.StoreSection `json:"store"`
		}{Store: store}
		if readErr == nil {
			printed.Inventory = &inventory
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(printed); err != nil {
			return err
		}
		return errors.Join(readErr, inspectErr)
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
			if cmd.Context().Err() != nil {
				command := "zzrouter-node recovery check " + output
				if keyOutput != "" {
					command += " --key-file " + keyOutput
				}
				return fmt.Errorf("the backup was written, but its check was interrupted: run %s before relying on it: %w", command, err)
			}
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
			switch {
			case operation == "restore" && manifest.Profile == recovery.ProfileDatabase:
				staged, installation := absolute(destination), absolute(dir)
				fmt.Fprintf(cmd.OutOrStdout(), "Staged only. Keep the node stopped and publish it with zzrouter-node recovery activate %s --dir %s, after a --dry-run. See docs/recovery.md.\n", staged, installation)
			case operation == "restore":
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
	var activation recovery.ActivateOptions
	activate := &cobra.Command{Use: "activate STAGING", Short: "Publish a staged database bundle as this stopped installation's access store", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, err := recovery.Activate(cmd.Context(), dir, args[0], activation)
		if err != nil {
			return err
		}
		describeActivation(cmd.OutOrStdout(), a, activation)
		return nil
	}}
	activate.Flags().BoolVar(&activation.DryRun, "dry-run", false, "Run every check and report what would change, publishing and replacing nothing")
	activate.Flags().BoolVar(&activation.Rollback, "rollback", false, "Undo an activation that did not finish")
	command.AddCommand(activate)
	for _, child := range command.Commands() {
		child.RunE = interruptible(child.RunE)
	}
	return command
}

// interruptible runs run under a context an interrupt or a service stop
// cancels, so the operation removes its temporaries and leaves no output. The
// first signal releases the handler: a second one ends the process at once.
func interruptible(run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		context.AfterFunc(ctx, stop)
		cmd.SetContext(ctx)
		return run(cmd, args)
	}
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

// describeActivation names what an activation replaced, its generation, its
// rollback directory and the next step, or what a dry run or a rollback did.
func describeActivation(w io.Writer, a recovery.Activation, o recovery.ActivateOptions) {
	fresh := len(a.Replaced) == 0
	switch {
	case o.Rollback && o.DryRun && fresh:
		fmt.Fprintf(w, "Dry run: rolling back activation %s would remove the access store it published, since this installation had none before. A dry run changes nothing.\n", a.RollbackID)
	case o.Rollback && o.DryRun:
		fmt.Fprintf(w, "Dry run: rolling back activation %s would put back the access store's database and store marker kept in %s. A dry run changes nothing.\n", a.RollbackID, a.RollbackDir)
	case o.Rollback && fresh:
		fmt.Fprintf(w, "Rolled back activation %s: the access store it published is removed, since this installation had none before. The staging directory is kept.\n", a.RollbackID)
	case o.Rollback:
		fmt.Fprintf(w, "Rolled back activation %s: the access store's database and store marker are as they were before. The staging directory is kept.\n", a.RollbackID)
	case o.DryRun:
		fmt.Fprintf(w, "Dry run: activating would publish generation %s of access store %s.\n", a.Store.Generation, a.Store.StoreID)
		if fresh {
			fmt.Fprintln(w, "This installation has no access store, so nothing would be replaced.")
		} else {
			fmt.Fprintf(w, "It would replace this installation's access store, keeping its database and store marker in %s.\n", a.RollbackDir)
		}
		fmt.Fprintln(w, "A dry run publishes nothing, replaces nothing and creates no rollback directory; at most, SQLite finished recovering a journal a crash left beside the database, as the next start would.")
	default:
		s := a.Store
		fmt.Fprintf(w, "Activated generation %s of access store %s (schema version %d, revision %d, sequence %d).\n", s.Generation, s.StoreID, s.SchemaVersion, s.Revision, s.Sequence)
		if fresh {
			fmt.Fprintf(w, "This installation had no access store, so nothing was replaced; %s records the activation.\n", a.RollbackDir)
		} else {
			fmt.Fprintf(w, "The previous database and store marker, credential hashes included, are kept in %s until you delete it.\n", a.RollbackDir)
		}
		fmt.Fprintln(w, "The staging directory is removed. Next: start the node. Every session and delegated access issued before this restore is invalid: people, the owner included, sign in again with their passwords, and app keys keep working.")
	}
}

// absolute is path made absolute, or path as given when that fails.
func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

package servercli

import (
	"context"
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

// NewRecoveryCmd exposes local offline inventory, backup, checking, staging
// and activation.
func NewRecoveryCmd() *cobra.Command {
	var dir string
	command := &cobra.Command{Use: "recovery", Short: "Inspect, back up, stage and activate an installation recovery"}
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
				fmt.Fprintf(cmd.OutOrStdout(), "Staged only. Keep the node stopped and publish it with zzrouter-node recovery activate %s, after a --dry-run. See docs/recovery.md.\n", destination)
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
	activate.Flags().BoolVar(&activation.DryRun, "dry-run", false, "Run every check and report what would change, changing nothing")
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
// rollback and the next step, or what a dry run or a rollback did.
func describeActivation(w io.Writer, a recovery.Activation, o recovery.ActivateOptions) {
	replaced := "nothing: this installation had no access store"
	if len(a.Replaced) > 0 {
		replaced = "the access store (its database and store marker)"
	}
	switch {
	case o.Rollback && o.DryRun:
		fmt.Fprintf(w, "Dry run: rolling back activation %s would restore %s. Nothing was changed.\n", a.RollbackID, replaced)
	case o.Rollback:
		fmt.Fprintf(w, "Rolled back activation %s: restored %s as it was before. The staging directory is kept.\n", a.RollbackID, replaced)
	case o.DryRun:
		fmt.Fprintf(w, "Dry run: activating would publish generation %s of access store %s and replace %s, kept in rollback %s. Nothing was changed.\n", a.Store.Generation, a.Store.StoreID, replaced, a.RollbackID)
	default:
		s := a.Store
		fmt.Fprintf(w, "Activated generation %s of access store %s (schema version %d, revision %d, sequence %d), replacing %s.\n", s.Generation, s.StoreID, s.SchemaVersion, s.Revision, s.Sequence, replaced)
		fmt.Fprintf(w, "Rollback %s keeps what was replaced, credential hashes included, under rollback/%s in the configuration directory until you remove it. The staging directory is removed.\n", a.RollbackID, a.RollbackID)
		fmt.Fprintln(w, "Next: start the node. Every session and delegated access issued before this restore is invalid: people, the owner included, sign in again with their passwords, and app keys keep working.")
	}
}

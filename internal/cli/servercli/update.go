package servercli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// NewUpdateCmd creates the update command group
func NewUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Manage auto-updates",
		Long: `Manage the zzrouter-node auto-update system.

The auto-update system can check for new releases, download and verify updates,
and apply them with automatic service restart.

Commands:
  status    - Show current update status
  check     - Check for available updates
  apply     - Apply a pending update
  rollback  - Rollback to the previous version
  history   - Show update history
  enable    - Move an existing service install into the managed layout
  run       - Perform a privileged update run (started by systemd)`,
	}

	cmd.AddCommand(
		newUpdateStatusCmd(),
		newUpdateCheckCmd(),
		newUpdateApplyCmd(),
		newUpdateRollbackCmd(),
		newUpdateHistoryCmd(),
		newUpdateRunCmd(),
		newUpdateEnableCmd(),
	)

	return cmd
}

// newUpdateStatusCmd creates the update status command
func newUpdateStatusCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show update status",
		Long:  "Display the current status of the auto-update system.",
		RunE: func(cmd *cobra.Command, args []string) error {
			scheduler, err := getScheduler()
			if err != nil {
				return err
			}
			status := scheduler.GetStatus()

			if jsonOutput {
				data, err := json.MarshalIndent(status, "", "  ")
				if err != nil {
					return fmt.Errorf("format status as JSON: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Update Status\n")
			fmt.Printf("=============\n\n")
			fmt.Printf("State:           %s\n", status.State)
			fmt.Printf("Current Version: %s\n", status.CurrentVersion.String())
			fmt.Printf("Channel:         %s\n", status.Channel)

			if status.LatestVersion != nil {
				fmt.Printf("Latest Version:  %s\n", status.LatestVersion.String())
			}

			fmt.Printf("Update Available: %v\n", status.UpdateAvailable)

			if status.LastCheckTime != nil {
				fmt.Printf("Last Check:      %s\n", status.LastCheckTime.Format(time.RFC3339))
			}
			if status.NextCheckTime != nil {
				fmt.Printf("Next Check:      %s\n", status.NextCheckTime.Format(time.RFC3339))
			}
			if status.LastUpdateTime != nil {
				fmt.Printf("Last Update:     %s\n", status.LastUpdateTime.Format(time.RFC3339))
			}
			// RestartRequired is deliberately not printed here. It lives
			// in the applying process's memory, and this command builds
			// a fresh scheduler, so the flag is always false by the time
			// a separate `update status` reads it. The durable version
			// of the same question is the pending record below.
			if status.Blocked != "" {
				fmt.Printf("\nCannot install updates: %s\n", status.Blocked)
			}
			if p := status.PendingConfirm; p != nil {
				// "installed", not "running": the record is written when
				// the binary lands, which is before any restart, and a
				// node with auto_restart off may still be on the old one.
				fmt.Printf("\nAwaiting confirmation: %s is installed but not yet proven healthy\n", p.ToVersion)
				fmt.Printf("Attempt:         %d of %d, then it rolls back to %s\n",
					p.Attempts, update.MaxConfirmAttempts, p.FromVersion)
			}
			if status.Error != "" {
				fmt.Printf("\nError: %s\n", status.Error)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output in JSON format")

	return cmd
}

// newUpdateCheckCmd creates the update check command
func newUpdateCheckCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check for updates",
		Long:  "Check if a new version is available from GitHub Releases.",
		RunE: func(cmd *cobra.Command, args []string) error {
			scheduler, err := getScheduler()
			if err != nil {
				return err
			}

			fmt.Println("Checking for updates...")

			ctx, cancel := context.WithTimeout(context.Background(), constants.ClusterActionTimeout)
			defer cancel()

			result, err := scheduler.CheckNow(ctx)
			if err != nil {
				return fmt.Errorf("check for updates: %w", err)
			}

			if jsonOutput {
				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return fmt.Errorf("format check result as JSON: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("\nCurrent version: %s\n", result.CurrentVersion.String())

			if !result.UpdateAvailable {
				fmt.Println("You are running the latest version.")
				if result.SkippedReason != "" {
					fmt.Printf("Note: %s\n", result.SkippedReason)
				}
				return nil
			}

			fmt.Printf("\nUpdate available!\n")
			fmt.Printf("  New version: %s\n", result.LatestRelease.Version.String())
			fmt.Printf("  Published:   %s\n", result.LatestRelease.PublishedAt.Format("2006-01-02 15:04:05"))
			fmt.Printf("  URL:         %s\n", result.LatestRelease.HTMLURL)

			if result.LatestRelease.Name != "" {
				fmt.Printf("  Title:       %s\n", result.LatestRelease.Name)
			}

			fmt.Printf("\nRun 'zzrouter-node update apply' to install this update.\n")
			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output in JSON format")

	return cmd
}

// newUpdateApplyCmd creates the update apply command
func newUpdateApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply pending update",
		Long: `Apply a pending update immediately.

This will:
1. Check for a newer release
2. Download the new version
3. Verify checksums and signatures
4. Create a backup of the current binary
5. Replace the binary with the new version
6. Restart the service (if auto_restart is enabled)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			scheduler, err := getScheduler()
			if err != nil {
				return err
			}

			fmt.Println("Applying update...")
			fmt.Println()

			// No pre-check against the in-memory status: this process
			// was started by this command, so that status is always
			// empty and gating on it made apply a no-op no matter what
			// `update check` had just reported. ApplyNow resolves the
			// pending release itself, checking the feed when it has to.
			err = scheduler.ApplyNow(context.Background())
			switch {
			case errors.Is(err, update.ErrNoUpdateAvailable):
				fmt.Println("No update available: this node is already running the latest version.")
				return nil
			case err != nil:
				return fmt.Errorf("apply update: %w", err)
			}

			fmt.Println("Update applied successfully!")
			// Report what happened rather than what is configured: the
			// apply knows whether it handed the process to a service
			// manager or left the new binary waiting for a restart.
			if status := scheduler.GetStatus(); status.RestartRequired {
				fmt.Println("The new version runs on the next restart of the node.")
				if status.RestartRequiredReason != "" {
					fmt.Printf("Reason: %s\n", status.RestartRequiredReason)
				}
			} else {
				fmt.Println("The service manager is restarting the node on the new version.")
			}
			return nil
		},
	}

	return cmd
}

// newUpdateRollbackCmd creates the update rollback command
func newUpdateRollbackCmd() *cobra.Command {
	var listBackups bool

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback to previous version",
		Long: `Rollback to the most recent backup.

This will replace the current binary with the most recent backup
created during a previous update.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			scheduler, err := getScheduler()
			if err != nil {
				return err
			}

			if listBackups {
				backupDir := scheduler.GetBackupDir()
				fmt.Printf("Backup directory: %s\n\n", backupDir)

				history, err := scheduler.GetHistory()
				if err != nil {
					return fmt.Errorf("load update history: %w", err)
				}

				if len(history.Entries) == 0 {
					fmt.Println("No update history found.")
					return nil
				}

				fmt.Println("Available backups:")
				for i, entry := range history.Entries {
					if entry.BackupPath != "" {
						fmt.Printf("  %d. %s → %s (%s)\n",
							i+1,
							entry.FromVersion.String(),
							entry.ToVersion.String(),
							entry.Timestamp.Format("2006-01-02 15:04:05"))
						fmt.Printf("     Backup: %s\n", entry.BackupPath)
					}
				}
				return nil
			}

			fmt.Println("Rolling back to previous version...")

			result, err := scheduler.Rollback(cmd.Context())
			if err != nil {
				return fmt.Errorf("rollback: %w", err)
			}
			if !result.Success {
				return fmt.Errorf("rollback failed: %s", result.Error)
			}

			fmt.Println("Rollback successful!")
			// A managed install has no backup file to name -- it moved a
			// symlink onto a version directory that was never touched --
			// so report the version. Printing an empty "restored from"
			// reads as a rollback that found nothing.
			if result.Version != "" {
				fmt.Printf("Now running: %s\n", result.Version)
			} else {
				fmt.Printf("Restored from backup: %s\n", result.BackupPath)
			}
			fmt.Println("\nManual restart required: run 'zzrouter-node start'")
			return nil
		},
	}

	cmd.Flags().BoolVarP(&listBackups, "list", "l", false, "List available backups")

	return cmd
}

// newUpdateHistoryCmd creates the update history command
func newUpdateHistoryCmd() *cobra.Command {
	var jsonOutput bool
	var limit int

	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show update history",
		Long:  "Display the history of past updates.",
		RunE: func(cmd *cobra.Command, args []string) error {
			scheduler, err := getScheduler()
			if err != nil {
				return err
			}

			history, err := scheduler.GetHistory()
			if err != nil {
				return fmt.Errorf("load update history: %w", err)
			}

			if jsonOutput {
				data, err := json.MarshalIndent(history, "", "  ")
				if err != nil {
					return fmt.Errorf("format history as JSON: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			if len(history.Entries) == 0 {
				fmt.Println("No update history.")
				return nil
			}

			fmt.Printf("Update History\n")
			fmt.Printf("==============\n\n")

			entries := history.Entries
			if limit > 0 && limit < len(entries) {
				entries = entries[:limit]
			}

			for _, entry := range entries {
				statusIcon := "SUCCESS"
				if !entry.Success {
					statusIcon = "FAILED"
				}

				fmt.Printf("[%s] %s\n", statusIcon, entry.Timestamp.Format("2006-01-02 15:04:05"))
				fmt.Printf("  %s → %s\n", entry.FromVersion.String(), entry.ToVersion.String())
				fmt.Printf("  Duration: %s\n", entry.Duration.Round(time.Second))

				if entry.Automatic {
					fmt.Printf("  Type: Automatic\n")
				} else {
					fmt.Printf("  Type: Manual\n")
				}

				if entry.Error != "" {
					fmt.Printf("  Error: %s\n", entry.Error)
				}

				fmt.Println()
			}

			if limit > 0 && len(history.Entries) > limit {
				fmt.Printf("(showing %d of %d entries)\n", limit, len(history.Entries))
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output in JSON format")
	cmd.Flags().IntVarP(&limit, "limit", "n", 10, "Number of entries to show")

	return cmd
}

// getScheduler creates a scheduler instance for CLI commands.
//
// A config that will not load is an error, not a cue to fall back to
// defaults. These commands download and install a binary: running them
// against the built-in defaults when node.yaml was meant to pin a
// version, hold a channel, or name a different release source is the
// one outcome nobody asked for, and the old fallback did it silently.
func getScheduler() (*update.Scheduler, error) {
	cm := pkgConfig.NewConfigManager("zzrouter")
	cfg, err := cm.LoadNodeConfig()
	if err != nil {
		return nil, fmt.Errorf("load node config: %w", err)
	}
	if err := cfg.Update.Validate(); err != nil {
		return nil, fmt.Errorf("invalid update config: %w", err)
	}

	return update.NewScheduler(&cfg.Update, clock.System()), nil
}

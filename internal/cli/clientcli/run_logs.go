package clientcli

// CLI `zzrouter run logs` — thin adapter over internal/client/logs and
// the standalone run logs TUI. All filter/streaming/dedup logic lives
// in the shared client package; this file handles flag parsing, TTY
// detection, output formatting, and signal handling.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/views"

	"github.com/spf13/cobra"
	logsclient "github.com/stperic/zzrouter/internal/client/logs"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"golang.org/x/term"
)

// NewRunLogsCmd builds the `zzrouter run logs` subcommand.
func NewRunLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs [RUN_ID_OR_PREFIX]",
		Short: "View or tail logs for provider runs",
		Long: `View or live-tail the stdout/stderr logs of one or more provider runs.

RUN_ID_OR_PREFIX accepts either a full run ID or a unique prefix (git-SHA
style). Combine with --provider / --model / --node / --status to narrow
the selection. With exactly one match, the logs open immediately;
otherwise an interactive picker appears (TUI) or a table is printed
(non-TTY).

Follow behavior:
  - running runs are followed live by default
  - stopped runs print a static tail
  - --no-follow forces a static tail even on running runs

Examples:
  # Follow logs for a specific run by ID prefix
  zzrouter run logs r_8f2a

  # Pick a run interactively from all MLX runs
  zzrouter run logs --provider mlx

  # Print the last 500 lines of one run's logs, then exit
  zzrouter run logs r_8f2a --no-follow --lines 500

  # Pipe live logs to grep
  zzrouter run logs r_8f2a | grep ERROR`,
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, _ := cmd.Flags().GetString("provider")
			model, _ := cmd.Flags().GetString("model")
			node, _ := cmd.Flags().GetString("node")
			status, _ := cmd.Flags().GetString("status")
			lines, _ := cmd.Flags().GetInt("lines")
			noFollow, _ := cmd.Flags().GetBool("no-follow")
			forceTUI, _ := cmd.Flags().GetBool("tui")

			filter := logsclient.RunFilter{
				Provider: provider,
				Model:    model,
				Node:     node,
				Status:   status,
			}
			if len(args) == 1 {
				filter.RunID = args[0]
			}

			client, err := EnsureConnected()
			if err != nil {
				return err
			}

			return runRunLogs(cmd.Context(), client, filter, lines, noFollow, forceTUI)
		},
	}

	cmd.Flags().String("provider", "", "Filter by provider (e.g. mlx, vllm, ollama)")
	cmd.Flags().String("model", "", "Filter by model")
	cmd.Flags().String("node", "", "Filter by node")
	cmd.Flags().String("status", "", "Filter by status (running|stopped)")
	cmd.Flags().Int("lines", logsclient.DefaultTailLines, "Tail size (non-follow mode)")
	cmd.Flags().Bool("no-follow", false, "Force static tail even on running runs")
	cmd.Flags().Bool("tui", false, "Force the interactive TUI viewer even when stdout is not a TTY")

	return cmd
}

// runRunLogs resolves the filter, decides picker vs viewer vs stdout,
// and dispatches. Contains zero HTTP or SSE code — that all lives in
// internal/client/logs.
//
// On ErrNoMatch, the user-friendly message is written to stderr and
// the error is returned. The Cobra command has SilenceErrors set, so
// Cobra does not print the error itself — it only uses the non-nil
// return to set the exit code. That keeps the RunE contract intact
// (no os.Exit from handlers) and keeps the command testable.
func runRunLogs(ctx context.Context, client *pkgClient.Client, filter logsclient.RunFilter, lines int, noFollow, forceTUI bool) error {
	runs, err := logsclient.ResolveRuns(ctx, client, filter)
	if err != nil {
		if errors.Is(err, logsclient.ErrNoMatch) {
			fmt.Fprintln(os.Stderr, "no runs match filter")
			return err
		}
		return err
	}

	isTTY := forceTUI || term.IsTerminal(int(os.Stdout.Fd()))

	// Multiple matches.
	if len(runs) > 1 {
		if isTTY {
			return views.RunRunlogsStandaloneTUI(client, filter)
		}
		return writeRunsTable(os.Stdout, runs)
	}

	// Exactly one match.
	run := runs[0]

	if isTTY {
		// Pass the original filter so the TUI can honour it (e.g.
		// show a picker title if one field was specified). With a
		// single match the picker will auto-jump to the viewer.
		return views.RunRunlogsStandaloneTUI(client, filter)
	}

	// Non-TTY stream path.
	return streamRunLogsToStdout(ctx, client, run, lines, noFollow)
}

// streamRunLogsToStdout prints logs for a single run to stdout. Follows
// live by default on running runs, static tail otherwise. Honours
// SIGINT for clean shutdown by cancelling the passed-in context. Thin
// shim around streamRunLogsTo that installs a signal handler and wires
// the standard streams.
func streamRunLogsToStdout(parentCtx context.Context, client *pkgClient.Client, run logsclient.Run, lines int, noFollow bool) error {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	return streamRunLogsTo(ctx, client, run, lines, noFollow, os.Stdout, os.Stderr)
}

// streamRunLogsTo is the testable core: it streams a single run's logs
// to the provided writers with no signal handling. Cancelling ctx is
// the only way to interrupt a follow — callers that want SIGINT
// semantics must install their own handler that cancels ctx.
func streamRunLogsTo(ctx context.Context, client *pkgClient.Client, run logsclient.Run, lines int, noFollow bool, stdout, stderr io.Writer) error {
	// Static tail path: stopped runs or --no-follow override.
	static := noFollow || !strings.EqualFold(run.Status, "running")
	if static {
		tail, err := logsclient.FetchTail(ctx, client, run.ID, lines)
		if err != nil {
			return fmt.Errorf("fetch logs: %w", err)
		}
		for _, line := range tail.Lines {
			fmt.Fprintln(stdout, line)
		}
		return nil
	}

	sess, err := logsclient.NewRunSession(ctx, client, run.ID)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	for ev := range sess.Events() {
		switch e := ev.(type) {
		case logsclient.EventEntry:
			fmt.Fprintln(stdout, e.Entry.Text)
		case logsclient.EventReset:
			fmt.Fprintln(stderr, "--- log rotated ---")
		case logsclient.EventError:
			fmt.Fprintln(stderr, "error:", e.Err)
		case logsclient.EventDone:
			return nil
		}
	}
	return nil
}

// writeRunsTable writes a human-readable table of matched runs to the
// provided writer. Used when multiple runs match and stdout is not a
// TTY — lets scripts and agents see the list without spawning a picker.
func writeRunsTable(w io.Writer, runs []logsclient.Run) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tPROVIDER\tMODEL\tNODE\tSTATUS\tSTARTED")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Provider, r.Model, r.Node, r.Status, r.StartedAt)
	}
	return tw.Flush()
}

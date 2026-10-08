package clientcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewLogsCmd creates the logs command for viewing inference request logs.
func NewLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "View inference request logs",
		Long: `View and stream inference request/response logs from the server.

Shows model, provider, tokens, latency, and routing information for each inference request.

Examples:
  zzrouter logs                        # Show recent inference logs
  zzrouter logs --follow               # Stream logs in real-time
  zzrouter logs --model llama3         # Filter by model name
  zzrouter logs --since 5m             # Show logs from last 5 minutes
  zzrouter logs --limit 100            # Show last 100 entries
  zzrouter logs -o json                # Output as JSON`,
		RunE: runLogs,
	}

	cmd.Flags().BoolP("follow", "f", false, "Stream logs in real-time (SSE)")
	cmd.Flags().StringP("model", "m", "", "Filter by model name")
	cmd.Flags().String("status", "", "Filter by status (success, error)")
	cmd.Flags().String("since", "", "Show logs since (duration like '5m' or RFC3339 timestamp)")
	cmd.Flags().IntP("limit", "n", 50, "Maximum number of entries to show")

	cmd.AddCommand(newLogsPayloadCmd())

	return cmd
}

// newLogsPayloadCmd creates the subcommand that prints an entry's retained
// request bodies. Output is the raw payload JSON so it redirects straight
// into a file.
func newLogsPayloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "payload <log-id>",
		Short: "Print the full request payload retained for a log entry",
		Long: `Print the request bodies retained for an inference log entry.

The server keeps full payloads for a shorter window than log metadata, so
an older entry may report that its payload is no longer retained. Entries
that still have one are marked in the PAYLOAD column of ` + "`zzrouter logs`" + `.

Attached media (images, audio, uploaded files) is replaced server-side by a
placeholder describing its type and size, so the prompt stays readable and
no binary comes back.

Examples:
  zzrouter logs payload abc123               # Print the payload
  zzrouter logs payload abc123 > req.json    # Save it to a file`,
		Args: cobra.ExactArgs(1),
		RunE: runLogsPayload,
	}
}

func runLogsPayload(cmd *cobra.Command, args []string) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	payload, err := client.GetInferenceLogPayload(args[0])
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format payload: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(out))
	return nil
}

func runLogs(cmd *cobra.Command, _ []string) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	follow, _ := cmd.Flags().GetBool("follow")
	model, _ := cmd.Flags().GetString("model")
	status, _ := cmd.Flags().GetString("status")
	since, _ := cmd.Flags().GetString("since")
	limit, _ := cmd.Flags().GetInt("limit")

	if follow {
		return streamLogs(client, model)
	}

	logs, err := client.GetInferenceLogs(model, status, since, limit)
	if err != nil {
		return err
	}

	if len(logs) == 0 {
		fmt.Println("No inference logs found.")
		return nil
	}

	return OutputData(cmd, logs, func() {
		printLogTable(logs)
	})
}

func streamLogs(client *pkgClient.Client, model string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Println("Streaming inference logs (Ctrl+C to stop)...")
	fmt.Println()

	return client.StreamInferenceLogs(ctx, model, func(entry pkgClient.InferenceLogEntry) {
		printLogLine(entry)
	})
}

func printLogTable(logs []pkgClient.InferenceLogEntry) {
	theme := shared.ResolveUITheme()
	renderer := ui.NewTerminalRendererWithOptions(&theme, true, 0)

	tableData := ui.TableData{
		Columns: []ui.TableColumn{
			{Header: "TIME", MaxWidth: 19},
			{Header: "MODEL", MaxWidth: 30},
			{Header: "TYPE", MaxWidth: 10},
			{Header: "TOKENS", MaxWidth: 12},
			{Header: "LATENCY", MaxWidth: 10},
			{Header: "TPS", MaxWidth: 8},
			{Header: "COST", MaxWidth: 8},
			{Header: "ROUTE", MaxWidth: 8},
			{Header: "STATUS", MaxWidth: 8},
			{Header: "PAYLOAD", MaxWidth: 8},
		},
		MinColumns: 5,
	}

	for _, log := range logs {
		tokens := fmt.Sprintf("%d/%d", log.TokensIn, log.TokensOut)
		latency := formatLatency(log.LatencyMs)
		tps := "-"
		if log.TokensPerSec > 0 {
			tps = fmt.Sprintf("%.1f", log.TokensPerSec)
		}
		route := log.RoutingDecision
		if route == "" {
			route = "-"
		}

		cost := "-"
		if log.Cost > 0 {
			cost = formatLogCostCLI(log.Cost)
		}

		tableData.Rows = append(tableData.Rows, []string{
			log.Timestamp.Local().Format("2006-01-02 15:04:05"),
			log.Model,
			log.RequestType,
			tokens,
			latency,
			tps,
			cost,
			route,
			log.Status,
			shared.FormatPayloadMark(log.PayloadAvailable),
		})
	}

	if err := renderer.RenderTable(tableData); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

func printLogLine(entry pkgClient.InferenceLogEntry) {
	ts := entry.Timestamp.Local().Format("15:04:05")
	tokens := fmt.Sprintf("%d/%d", entry.TokensIn, entry.TokensOut)
	latency := formatLatency(entry.LatencyMs)

	status := entry.Status
	if status == "error" {
		status = "ERR"
	} else {
		status = "OK"
	}

	fmt.Printf("%s  %-25s  %-6s  tok:%-10s  %s  %s\n",
		ts, truncate(entry.Model, 25), entry.RequestType, tokens, latency, status)
}

func formatLogCostCLI(cost float64) string {
	if cost < 0.001 {
		return fmt.Sprintf("$%.4f", cost)
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.3f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func formatLatency(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "."
}

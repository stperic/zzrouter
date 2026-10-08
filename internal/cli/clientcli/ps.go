package clientcli

import (
	"fmt"
	"strings"
	"time"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// NewPsCmd creates the ps command for showing running models across hosts
func NewPsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ps [model_name]",
		Short: "List running model(s)",
		Long: `List running (loaded) model(s) across all connected zzRouter nodes and inference providers.

Default behavior (no subcommand): List all running models.

Supports filtering with node and provider parameters:
  --node <name>  --provider <type>
  [model_name] (optional positional argument for model filtering)

Wildcards (*) supported for all filters.

Examples:
  zzrouter ps                                  # List all running models (default)
  zzrouter ps --provider ollama                     # All Ollama instances
  zzrouter ps --node localhost --provider ollama    # Ollama on localhost
  zzrouter ps Qwen/Qwen2.5/VL                  # Filter by model name
  zzrouter ps --provider "*llm" "Qwen*"             # Wildcard patterns

Subcommands:
  zzrouter ps list [model_name]     # List running models (explicit)
  zzrouter ps logs <model_name>     # View logs (last 50 lines)
  zzrouter ps stream <model_name>   # Stream logs (real-time)
  zzrouter ps details <model_name>  # View instance details

For stopping instances, use:
  zzrouter stop <model_spec_or_id>  # Stop running models`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, _ := cmd.Flags().GetString("provider")

			// --model is not registered on `ps`; the positional arg is
			// the model filter.
			model, err := ParsePositionalModelArg(args, "")
			if err != nil {
				return err
			}

			return runPs(nil, provider, model) // clientConfig not needed since EnsureConnected handles it
		},
	}

	AddCommonFilterFlags(cmd)

	// Add subcommands
	cmd.AddCommand(NewPsListCmd())
	cmd.AddCommand(NewPsLogsCmd())
	cmd.AddCommand(NewPsStreamCmd())
	cmd.AddCommand(NewPsDetailsCmd())

	return cmd
}

func runPs(_ *pkgConfig.ClientConfig, provider, model string) error {
	// Use centralized sign-in logic
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Fetch and filter instances using shared data layer
	filteredInstances, err := shared.FetchInstances(client, provider, model)
	if err != nil {
		return err
	}

	if len(filteredInstances) == 0 {
		if provider != "" || model != "" {
			fmt.Println("No running instances match the specified criteria")
		} else {
			fmt.Println("No running instances")
		}
		return nil
	}

	// Display instances in ps-style table
	displayPsTable(filteredInstances)
	fmt.Println()

	return nil
}

// displayPsTable displays instances in ps-style table format with NODE as first column
func displayPsTable(instances []pkgClient.Instance) {
	if len(instances) == 0 {
		return
	}

	// Calculate column widths
	maxNodeWidth := len("NODE")
	maxNameWidth := len("NAME")
	maxIDWidth := 12 // Fixed width for 12-char hex IDs (like Ollama)
	maxProviderWidth := len("PROVIDER")
	maxStatusWidth := len("STATUS")
	maxUntilWidth := len("UNTIL")

	for _, inst := range instances {
		// The run states where its weights came from.
		sourceRepo := inst.SourceRepo
		// Format model name to show only the last segment
		formattedModel := utils.FormatModelName(inst.Model)
		if formattedModel == "" {
			formattedModel = "(unknown)"
		}
		nameDisplay := shared.FormatModelWithRegistry(sourceRepo, formattedModel)

		// Calculate max widths using visual width (handles emojis properly)
		if hostWidth := len(inst.Node); hostWidth > maxNodeWidth {
			maxNodeWidth = hostWidth
		}
		if nameWidth := runewidth.StringWidth(nameDisplay); nameWidth > maxNameWidth {
			maxNameWidth = nameWidth
		}
		if providerWidth := len(inst.App); providerWidth > maxProviderWidth {
			maxProviderWidth = providerWidth
		}
		statusStr := inst.Status
		if statusWidth := len(statusStr); statusWidth > maxStatusWidth {
			maxStatusWidth = statusWidth
		}
		untilStr := formatUntilFromKeepAlive(inst.KeepAlive, inst.LastActivity, inst.StartedAt)
		if untilWidth := len(untilStr); untilWidth > maxUntilWidth {
			maxUntilWidth = untilWidth
		}
	}

	// Print header
	fmt.Printf("%-*s  %-*s  %-*s  %-*s  %-*s  %-*s\n",
		maxNodeWidth, "NODE",
		maxNameWidth, "NAME",
		maxIDWidth, "ID",
		maxProviderWidth, "PROVIDER",
		maxStatusWidth, "STATUS",
		maxUntilWidth, "UNTIL")

	// Print separator line
	fmt.Printf("%s  %s  %s  %s  %s  %s\n",
		strings.Repeat("-", maxNodeWidth),
		strings.Repeat("-", maxNameWidth),
		strings.Repeat("-", maxIDWidth),
		strings.Repeat("-", maxProviderWidth),
		strings.Repeat("-", maxStatusWidth),
		strings.Repeat("-", maxUntilWidth))

	// Print data rows
	for _, inst := range instances {
		// The run states where its weights came from.
		sourceRepo := inst.SourceRepo
		// Format model name to show only the last segment
		formattedModel := utils.FormatModelName(inst.Model)
		if formattedModel == "" {
			formattedModel = "(unknown)"
		}
		nameDisplay := shared.FormatModelWithRegistry(sourceRepo, formattedModel)

		// Format columns
		statusStr := inst.Status
		untilStr := formatUntilFromKeepAlive(inst.KeepAlive, inst.LastActivity, inst.StartedAt)

		// Truncate instance ID to 12 chars for display (full ID stored internally)
		displayID := truncateID(inst.ID, 12)

		// Calculate padding for name column to handle visual width properly
		namePadding := maxNameWidth - runewidth.StringWidth(nameDisplay)

		fmt.Printf("%-*s  %s%s  %-*s  %-*s  %-*s  %-*s\n",
			maxNodeWidth, inst.Node,
			nameDisplay, strings.Repeat(" ", namePadding),
			maxIDWidth, displayID,
			maxProviderWidth, inst.App,
			maxStatusWidth, statusStr,
			maxUntilWidth, untilStr)
	}
}

// truncateID truncates an instance ID to the specified length for display
// Full IDs are stored internally, but we display only the first N chars (like Docker/Ollama)
func truncateID(id string, maxLen int) string {
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen]
}

// viewInstanceLogs displays logs for an instance
func viewInstanceLogs(client *pkgClient.Client, instanceID, host string, lines int, follow bool) error {
	logs, err := client.GetInstanceLogs(instanceID, host, lines, follow)
	if err != nil {
		return err
	}

	if follow {
		// Streaming mode - logs will be printed as they arrive
		// This is handled by the client
		return nil
	}

	// Static mode - print all logs
	fmt.Println("\nLogs:")
	fmt.Println(strings.Repeat("-", 80))
	for _, line := range logs {
		fmt.Println(line)
	}
	fmt.Println(strings.Repeat("-", 80))

	return nil
}

// viewInstanceDetails displays detailed information about an instance
func viewInstanceDetails(inst *pkgClient.Instance) {
	fmt.Println("\n📊 Instance Details")
	fmt.Println(strings.Repeat("-", 80))

	// Basic info
	fmt.Printf("ID:              %s\n", inst.ID)
	fmt.Printf("Model:           %s\n", inst.Model)
	fmt.Printf("Provider:        %s\n", inst.App)
	fmt.Printf("Status:          %s\n", inst.Status)
	fmt.Printf("Launch Mode:     %s\n", inst.LaunchMode)
	fmt.Printf("Port:            %d\n", inst.Port)

	// Process info
	if inst.ProcessID > 0 {
		fmt.Printf("Process ID:      %d\n", inst.ProcessID)
	}

	// Model metadata
	if inst.SourceRepo != "" {
		fmt.Printf("Source Registry:     %s\n", inst.SourceRepo)
	}
	if inst.SizeBytes > 0 {
		sizeGB := float64(inst.SizeBytes) / (1024 * 1024 * 1024)
		fmt.Printf("Model Size:      %.2f GB\n", sizeGB)
	}
	if inst.ContextLength > 0 {
		fmt.Printf("Context Length:  %d\n", inst.ContextLength)
	}
	if inst.Processor != "" {
		fmt.Printf("Processor:       %s\n", inst.Processor)
	}

	// Timing info
	if inst.StartedAt != "" {
		startTime, err := time.Parse(time.RFC3339, inst.StartedAt)
		if err == nil {
			uptime := time.Since(startTime)
			hours := int(uptime.Hours())
			minutes := int(uptime.Minutes()) % 60
			fmt.Printf("Started At:      %s (%dh %dm ago)\n", startTime.Format("2006-01-02 15:04:05"), hours, minutes)
		} else {
			fmt.Printf("Started At:      %s\n", inst.StartedAt)
		}
	}

	if inst.LastActivity != "" {
		lastActivity, err := time.Parse(time.RFC3339, inst.LastActivity)
		if err == nil {
			ago := time.Since(lastActivity)
			fmt.Printf("Last Activity:   %s (%d seconds ago)\n", lastActivity.Format("2006-01-02 15:04:05"), int(ago.Seconds()))
		}
	}

	// Keep-alive info
	if inst.KeepAlive != "" {
		fmt.Printf("Keep Alive:      %s\n", inst.KeepAlive)
		untilStr := formatUntilFromKeepAlive(inst.KeepAlive, inst.LastActivity, inst.StartedAt)
		if untilStr != "-" {
			fmt.Printf("Expires In:      %s\n", untilStr)
		}
	}

	// Launch command info
	if inst.LaunchCommand != nil {
		fmt.Println("\nLaunch Command:")
		fmt.Printf("  Command:       %s\n", inst.LaunchCommand.Command)
		if len(inst.LaunchCommand.Args) > 0 {
			fmt.Println("  Arguments:")
			for i, arg := range inst.LaunchCommand.Args {
				fmt.Printf("    [%d] %s\n", i, arg)
			}
		}
		if len(inst.LaunchCommand.Environment) > 0 {
			fmt.Println("  Environment:")
			for key, value := range inst.LaunchCommand.Environment {
				fmt.Printf("    %s=%s\n", key, value)
			}
		}
		if inst.LaunchCommand.WorkingDir != "" {
			fmt.Printf("  Working Dir:   %s\n", inst.LaunchCommand.WorkingDir)
		}
	}

	fmt.Println(strings.Repeat("-", 80))
}

// findInstanceByModelOrID finds a single instance by model name OR instance ID, with disambiguation
// Supports: instance ID (exact/prefix), model name (exact/substring/wildcard)
// If multiple matches, returns the most recent instance (by StartedAt)
func findInstanceByModelOrID(client *pkgClient.Client, provider, spec string) (*pkgClient.Instance, error) {
	instances, err := client.ListInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	// First filter by provider (if specified)
	if provider != "" {
		instances = shared.FilterInstances(instances, provider, "")
	}

	// Then filter by model name or instance ID (DRY utility)
	filtered := FilterInstancesBySpec(instances, spec)

	if len(filtered) == 0 {
		return nil, fmt.Errorf("no running instances match the specified criteria")
	}

	if len(filtered) == 1 {
		return &filtered[0], nil
	}

	// Multiple matches - show disambiguation and use most recent
	fmt.Printf("Multiple instances match '%s':\n\n", spec)
	displayPsTable(filtered)
	fmt.Printf("\nTotal: %d instance(s)\n", len(filtered))

	// Find most recent instance (latest StartedAt)
	// StartedAt is a string, so we compare lexicographically (ISO 8601 format sorts correctly)
	mostRecent := &filtered[0]
	for i := 1; i < len(filtered); i++ {
		if filtered[i].StartedAt > mostRecent.StartedAt {
			mostRecent = &filtered[i]
		}
	}

	fmt.Printf("\n📋 Using most recent instance: %s (started %s)\n\n",
		mostRecent.ID, mostRecent.StartedAt)

	return mostRecent, nil
}

// NewPsListCmd creates the ps list subcommand (explicit version of default ps behavior)
func NewPsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [model_name]",
		Short: "List running models (explicit)",
		Long: `List running (loaded) model(s) across all connected zzRouter nodes and inference providers.

This is an explicit subcommand version of the default 'zzrouter ps' behavior.

Supports filtering with node and provider parameters:
  --node <name>  --provider <type>
  [model_name] (optional positional argument for model filtering)

Wildcards (*) supported for all filters.

Examples:
  zzrouter ps list                             # List all running models
  zzrouter ps list --provider ollama                # All Ollama instances
  zzrouter ps list Qwen/Qwen2.5/VL             # Filter by model name
  zzrouter ps list --provider "*llm" "Qwen*"        # Wildcard patterns`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, _ := cmd.Flags().GetString("provider")
			// --model not registered; positional arg is the model filter.

			// Handle positional argument using DRY utility
			model, err := ParsePositionalModelArg(args, "")
			if err != nil {
				return err
			}

			return runPs(nil, provider, model) // Same as default ps behavior
		},
	}

	AddCommonFilterFlags(cmd)
	return cmd
}

// NewPsLogsCmd creates the ps logs subcommand
func NewPsLogsCmd() *cobra.Command {
	var lines int

	cmd := &cobra.Command{
		Use:   "logs <model_name_or_id>",
		Short: "View logs for a running model",
		Long: `View logs for a running model instance.

Supports model name OR instance ID:
  - Instance ID (exact or prefix): "06c1097efce0" or "06c109"
  - Model name (exact/substring/wildcard): "qwen3-coder:30b" or "Qwen*"

Filtering:
  --node <name>  --provider <type>

Examples:
  zzrouter ps logs Qwen/Qwen2.5/VL         # By model name
  zzrouter ps logs 06c1097efce0            # By instance ID
  zzrouter ps logs --lines 100 "Qwen*"     # Wildcard pattern
  zzrouter ps logs --provider ollama qwen       # Filter by provider`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, _ := cmd.Flags().GetString("provider")
			// --model not registered; positional arg is the model filter.

			// Handle positional argument
			model, err := ParsePositionalModelArg(args, "")
			if err != nil {
				return err
			}

			if model == "" {
				return fmt.Errorf("model name is required")
			}

			client, err := EnsureConnected()
			if err != nil {
				return err
			}

			instance, err := findInstanceByModelOrID(client, provider, model)
			if err != nil {
				return err
			}

			return viewInstanceLogs(client, instance.ID, instance.Node, lines, false)
		},
	}

	AddCommonFilterFlags(cmd)
	cmd.Flags().IntVarP(&lines, "lines", "l", 50, "Number of log lines to display")
	return cmd
}

// NewPsStreamCmd creates the ps stream subcommand
func NewPsStreamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stream <model_name_or_id>",
		Short: "Stream logs for a running model",
		Long: `Stream logs (real-time) for a running model instance.

Supports model name OR instance ID:
  - Instance ID (exact or prefix): "06c1097efce0" or "06c109"
  - Model name (exact/substring/wildcard): "qwen3-coder:30b" or "Qwen*"

Filtering:
  --node <name>  --provider <type>

Examples:
  zzrouter ps stream Qwen/Qwen2.5/VL       # By model name
  zzrouter ps stream 06c1097efce0          # By instance ID
  zzrouter ps stream --provider ollama qwen     # Filter by provider`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, _ := cmd.Flags().GetString("provider")
			// --model not registered; positional arg is the model filter.

			// Handle positional argument
			model, err := ParsePositionalModelArg(args, "")
			if err != nil {
				return err
			}

			if model == "" {
				return fmt.Errorf("model name is required")
			}

			client, err := EnsureConnected()
			if err != nil {
				return err
			}

			instance, err := findInstanceByModelOrID(client, provider, model)
			if err != nil {
				return err
			}

			fmt.Printf("📡 Streaming logs for %s (press Ctrl+C to stop)...\n", instance.Model)
			fmt.Println(strings.Repeat("-", 80))
			err = client.StreamInstanceLogs(instance.ID, instance.Node)
			fmt.Println(strings.Repeat("-", 80))
			return err
		},
	}

	AddCommonFilterFlags(cmd)
	return cmd
}

// NewPsDetailsCmd creates the ps details subcommand
func NewPsDetailsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "details <model_name_or_id>",
		Short: "View details for a running model",
		Long: `View detailed information for a running model instance.

Supports model name OR instance ID:
  - Instance ID (exact or prefix): "06c1097efce0" or "06c109"
  - Model name (exact/substring/wildcard): "qwen3-coder:30b" or "Qwen*"

Filtering:
  --node <name>  --provider <type>

Examples:
  zzrouter ps details Qwen/Qwen2.5/VL      # By model name
  zzrouter ps details 06c1097efce0         # By instance ID
  zzrouter ps details --provider ollama --model Qwen/Qwen2.5/VL
  zzrouter ps details --model "Qwen*"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, _ := cmd.Flags().GetString("provider")
			// --model not registered; positional arg is the model filter.

			// Handle positional argument
			model, err := ParsePositionalModelArg(args, "")
			if err != nil {
				return err
			}

			if model == "" {
				return fmt.Errorf("model name is required")
			}

			client, err := EnsureConnected()
			if err != nil {
				return err
			}

			instance, err := findInstanceByModelOrID(client, provider, model)
			if err != nil {
				return err
			}

			// Fetch fresh data from server
			freshInstance, err := client.GetInstance(instance.ID, instance.Node)
			if err != nil {
				return fmt.Errorf("failed to fetch instance details: %w", err)
			}

			viewInstanceDetails(freshInstance)
			return nil
		},
	}

	AddCommonFilterFlags(cmd)
	return cmd
}

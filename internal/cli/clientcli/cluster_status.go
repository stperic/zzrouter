package clientcli

import (
	"encoding/json"
	"fmt"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
)

// =============================================================================
// STATUS COMMAND - uses same /zzrouter/nodes endpoint as 'nodes' command
// =============================================================================

// NewStatusCmd creates the status command for cluster health and info
func NewStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster status and health",
		Long: `Display cluster status including:
- Cluster health
- All nodes (coordinator and workers)
- Version information for each node
- Uptime and connectivity

This command provides an at-a-glance view of your entire zzRouter cluster.

Examples:
  zzrouter status            # Show cluster status
  zzrouter status -v         # Detailed status
  zzrouter status -j         # JSON output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			verbose, _ := cmd.Flags().GetBool("verbose")
			jsonOutput, _ := cmd.Flags().GetBool("json")
			return runStatus(cmd, verbose, jsonOutput)
		},
	}

	cmd.Flags().BoolP("verbose", "v", false, "Show detailed information")
	cmd.Flags().BoolP("json", "j", false, "Output in JSON format")

	return cmd
}

func runStatus(cmd *cobra.Command, verbose bool, jsonOutput bool) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Use the same /zzrouter/nodes endpoint as the 'nodes' command
	response, err := client.GetNodesWithRefresh("", false)
	if err != nil {
		return fmt.Errorf("failed to fetch cluster status: %w", err)
	}

	nodes, err := shared.ParseNodesFromResponse(response)
	if err != nil {
		return fmt.Errorf("failed to parse nodes: %w", err)
	}

	// Check for JSON output
	if format := GetOutputFormat(cmd); format == OutputJSON || jsonOutput {
		return displayStatusJSON(nodes)
	}

	return displayStatusFormatted(nodes, verbose)
}

func displayStatusJSON(nodes []shared.NodeInfo) error {
	// Determine cluster health from node statuses
	clusterHealth := "healthy"
	for _, n := range nodes {
		if n.HealthStatus == "down" {
			clusterHealth = "degraded"
			break
		}
	}

	output := map[string]any{
		"cluster_health": clusterHealth,
		"total_nodes":    len(nodes),
		"nodes":          nodes,
	}

	jsonBytes, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to format JSON: %w", err)
	}

	fmt.Println(string(jsonBytes))
	return nil
}

func displayStatusFormatted(nodes []shared.NodeInfo, verbose bool) error {
	// Determine cluster health
	clusterHealth := "healthy"
	for _, n := range nodes {
		if n.HealthStatus == "down" {
			clusterHealth = "degraded"
			break
		}
	}

	healthIcon := getStatusIcon(clusterHealth)
	fmt.Printf("Cluster Status: %s %s\n", healthIcon, clusterHealth)
	fmt.Printf("Total Nodes:    %d\n\n", len(nodes))

	if len(nodes) == 0 {
		fmt.Println("No nodes found.")
		return nil
	}

	// First node is the coordinator
	displayNodeStatusInfo("Coordinator", nodes[0], verbose)

	if len(nodes) > 1 {
		fmt.Println("\nWorkers:")
		for i := 1; i < len(nodes); i++ {
			displayNodeStatusInfo("", nodes[i], verbose)
		}
	}

	fmt.Println()
	return nil
}

func displayNodeStatusInfo(label string, node shared.NodeInfo, verbose bool) {
	statusIcon := getStatusIcon(node.HealthStatus)
	uptime := shared.FormatUptimeDuration(node.UptimeSeconds)

	if label != "" {
		fmt.Printf("%s:\n", label)
	}

	address := node.Address
	if address == "" {
		address = node.IPAddress
	}

	if verbose {
		fmt.Printf("  %s %s (%s) - %s\n", statusIcon, node.Name, node.Version, node.ClusterRole)
		fmt.Printf("     Address: %s\n", address)
		fmt.Printf("     Uptime:  %s\n", uptime)
		fmt.Printf("     Status:  %s\n", node.HealthStatus)
		if node.Memory != "" && node.Memory != "-" {
			fmt.Printf("     RAM:     %s\n", node.Memory)
		}
		if node.GPU != "" && node.GPU != "-" {
			fmt.Printf("     VRAM:    %s\n", node.GPU)
		}
	} else {
		fmt.Printf("  %s %s (%s) - %s - Uptime: %s\n", statusIcon, node.Name, node.Version, address, uptime)
	}
}

func getStatusIcon(status string) string {
	if status == "healthy" || status == "" {
		return "\u2713"
	}
	return "\u2717"
}

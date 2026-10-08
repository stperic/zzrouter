package clientcli

import (
	"encoding/json"
	"fmt"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/ui"
)

// =============================================================================
// NODES COMMAND (was: host list)
// =============================================================================

// NewNodesCmd creates the nodes command for listing cluster nodes
func NewNodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List nodes in the cluster",
		Long: `List all nodes in the cluster with system information.

Shows disk space, memory, GPU information for each node.

Examples:
  zzrouter nodes                # List all nodes
  zzrouter nodes -d             # Detailed view with GPU usage
  zzrouter nodes -r             # Resources only (compact)
  zzrouter nodes -j             # JSON output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			detailed, _ := cmd.Flags().GetBool("detailed")
			fields, _ := cmd.Flags().GetString("fields")
			resources, _ := cmd.Flags().GetBool("resources")
			refresh, _ := cmd.Flags().GetBool("refresh")
			jsonOutput, _ := cmd.Flags().GetBool("json")

			return runNodes(cmd, detailed, fields, resources, refresh, jsonOutput)
		},
	}

	cmd.Flags().BoolP("detailed", "d", false, "Include detailed information (GPU usage)")
	cmd.Flags().StringP("fields", "f", "", "Comma-separated fields: disk,memory,gpu,gpu_usage,all")
	cmd.Flags().BoolP("resources", "r", false, "Show only resource information")
	cmd.Flags().BoolP("refresh", "R", false, "Force refresh by re-probing all workers")
	cmd.Flags().BoolP("json", "j", false, "Output in JSON format")

	return cmd
}

func runNodes(cmd *cobra.Command, detailed bool, fields string, resources bool, refresh bool, jsonOutput bool) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	fieldsParam := fields
	if fieldsParam == "" && detailed {
		fieldsParam = "all"
	}

	// Fetch and parse nodes using shared data layer
	nodes, err := shared.FetchNodes(client, fieldsParam, refresh)
	if err != nil {
		return err
	}

	if refresh {
		fmt.Printf("%s Cluster refreshed\n", ui.GetCheckEmoji())
	}

	if len(nodes) == 0 {
		if format := GetOutputFormat(cmd); format == OutputJSON || jsonOutput {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("No nodes found")
		return nil
	}

	// Check for JSON output
	if format := GetOutputFormat(cmd); format == OutputJSON || jsonOutput {
		jsonBytes, err := json.MarshalIndent(nodes, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to format JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	displayNodesTable(nodes, resources)
	fmt.Println()

	return nil
}

func displayNodesTable(nodes []shared.NodeInfo, resourcesOnly bool) {
	var columns []ui.TableColumn
	var minColumns int

	if resourcesOnly {
		columns = []ui.TableColumn{
			{Header: "NODE", MaxWidth: 20},
			{Header: "STORAGE", MaxWidth: 18},
			{Header: "RAM", MaxWidth: 18},
			{Header: "VRAM", MaxWidth: 18},
			{Header: "GPU", MaxWidth: 25},
		}
		minColumns = 3
	} else {
		columns = []ui.TableColumn{
			{Header: "NODE", MaxWidth: 20},
			{Header: "IP", MaxWidth: 15},
			{Header: "ROLE", MaxWidth: 11},
			{Header: "STATUS", MaxWidth: 10},
			{Header: "OS", MaxWidth: 10},
			{Header: "VERSION", MaxWidth: 12},
			{Header: "STORAGE", MaxWidth: 18},
			{Header: "RAM", MaxWidth: 18},
			{Header: "VRAM", MaxWidth: 18},
		}
		minColumns = 6
	}

	var rows [][]string
	for _, node := range nodes {
		ip := node.IPAddress
		if ip == "" {
			ip = "-"
		}

		if resourcesOnly {
			rows = append(rows, []string{
				node.Name,
				node.Disk,
				node.Memory,
				node.GPU,
				node.GPUDesc,
			})
		} else {
			osName := node.OS
			if osName == "" {
				osName = "-"
			}
			version := node.Version
			if version == "" {
				version = "-"
			}
			rows = append(rows, []string{
				node.Name,
				ip,
				node.ClusterRole,
				node.HealthStatus,
				osName,
				version,
				node.Disk,
				node.Memory,
				node.GPU,
			})
		}
	}

	renderer := ui.NewTerminalRendererWithOptions(nil, true, 0)
	defer func() { _ = renderer.Close() }()

	tableData := ui.TableData{
		Columns:    columns,
		Rows:       rows,
		MinColumns: minColumns,
	}

	if err := renderer.RenderTable(tableData); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

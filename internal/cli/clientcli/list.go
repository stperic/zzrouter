package clientcli

import (
	"fmt"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Note: Display constants removed - unused after refactoring to ui package

// NewListCmd creates the list command (Ollama-compatible: lists models only)
func NewListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [model_name]",
		Short: "List available models",
		Long: `List all available models across all connected nodes.

This command is Ollama-compatible and only lists models.
For node and provider information, use 'zzrouter nodes' and 'zzrouter providers'.

Parameters (all support wildcards * and ?):
  --node <name>      Node name pattern (supports wildcards: *, gpu*, local*)
  --registry <source>   Source registry: ollama, huggingface (supports wildcards)
  --provider <provider>   Application/provider: ollama, vllm, llamacpp, mlx (supports wildcards)
  --model <name>     Model name pattern (supports wildcards: *, llama*, qwen?)
  --sort <field>     Sort by: node, registry, model, size, date (default: date, recent first)

Examples:
  zzrouter list                                    # List all models
  zzrouter list --node "gpu*"                      # Nodes starting with "gpu"
  zzrouter list --provider "vllm"                       # All vLLM models
  zzrouter list --registry "huggingface"              # All HuggingFace models
  zzrouter list --model "llama*"                   # Models starting with "llama"
  zzrouter list --sort size                        # Sort by size (largest first)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default behavior: list models
			params, err := ParseModelQueryParams(cmd, args)
			if err != nil {
				return err
			}
			return runList(cmd, params)
		},
	}

	// Add flags for model listing
	cmd.Flags().StringP("node", "n", "*", "Node name pattern (supports wildcards)")
	cmd.Flags().StringP("registry", "r", "", "Source registry: ollama, huggingface")
	cmd.Flags().StringP("provider", "a", "*", "Provider: ollama, vllm, llamacpp, mlx")
	cmd.Flags().StringP("model", "m", "", "Model name pattern (supports wildcards)")
	cmd.Flags().StringP("sort", "s", "date", "Sort by: node, repo, model, size, date")
	cmd.Flags().BoolP("refresh", "R", false, "Force refresh cache from all nodes")

	return cmd
}

func runList(cmd *cobra.Command, params *shared.ModelQueryParams) error {
	// Use centralized sign-in logic
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Check for refresh flag
	refresh := false
	if cmd != nil {
		refresh, _ = cmd.Flags().GetBool("refresh")
	}

	// Fetch models using shared data layer
	models, err := shared.FetchModels(client, params, refresh)
	if err != nil {
		return err
	}

	if refresh {
		fmt.Printf("%s Cache refreshed\n", ui.GetCheckEmoji())
	}

	// Handle empty results
	if len(models) == 0 {
		// For JSON/YAML, output empty array
		if format := GetOutputFormat(cmd); format != OutputTable {
			return OutputData(cmd, []any{}, func() {})
		}

		fmt.Printf("No models found")
		if params.Node != "" && params.Node != "*" {
			fmt.Printf(" on node '%s'", params.Node)
		}
		if params.Registry != "" && params.Registry != "*" {
			fmt.Printf(" in registry '%s'", params.Registry)
		}
		if params.Model != "" {
			fmt.Printf(" matching '%s'", params.Model)
		}
		fmt.Println()
		fmt.Println()
		return nil
	}

	// Output in requested format
	return OutputData(cmd, models, func() {
		displayModelsTable(models, params.SortBy)
	})
}

// displayModelsTable displays models using adaptive table format
func displayModelsTable(models []pkgClient.ModelMetadata, sortBy string) {
	// Sort models FIRST before building rows
	shared.SortModelRows(models, sortBy)

	// Define columns with max widths for adaptive rendering
	columns := []ui.TableColumn{
		{Header: "MODEL", MaxWidth: 50},
		{Header: "PROVIDER", MaxWidth: 12},
		{Header: "NODE", MaxWidth: 25},
		{Header: "FORMAT", MaxWidth: 15},
		{Header: "SIZE", MaxWidth: 10},
		{Header: "MODIFIED", MaxWidth: 12},
	}

	// Convert models to rows
	var rows [][]string
	for _, m := range models {
		// Determine source repo (fallback to "ollama" if not specified)
		sourceRepo := m.SourceRepo
		if sourceRepo == "" {
			sourceRepo = constants.RepoOllama // Default for Ollama-format responses
		}

		formattedModelName := utils.FormatModelName(m.Name)
		modelDisplay := shared.FormatModelWithRegistry(sourceRepo, formattedModelName)

		// Use GetModifiedTime() to handle both modified_at and modified fields
		modTime := m.GetModifiedTime()
		modifiedStr := ""
		if !modTime.IsZero() {
			modifiedStr = modTime.Format("2006-01-02")
		}

		// Get format using the GetFormat() method
		format := m.GetFormat()
		if format == "" {
			format = "-"
		}

		// Get assigned provider
		assignedProvider := m.AssignedApp
		if assignedProvider == "" {
			assignedProvider = "-"
		}

		rows = append(rows, []string{
			modelDisplay,
			assignedProvider,
			m.Node,
			format,
			shared.FormatSize(m.Size),
			modifiedStr,
		})
	}

	// Create adaptive renderer with no minimum width (use actual terminal width)
	renderer := ui.NewTerminalRendererWithOptions(nil, true, 0)
	defer func() { _ = renderer.Close() }()

	// Prepare table data with column configuration
	tableData := ui.TableData{
		Columns:    columns,
		Rows:       rows,
		MinColumns: 3, // Always show at least NODE, MODEL, FORMAT
	}

	// Render the table
	if err := renderer.RenderTable(tableData); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

// Note: rendering lives in the ui package and displayUnifiedTableFromRows in utils.go.

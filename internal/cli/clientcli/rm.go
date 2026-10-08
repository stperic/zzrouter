package clientcli

import (
	"fmt"
	"strings"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewRmCmd creates the rm command for removing models
func NewRmCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "rm [model]",
		Short: "Remove model(s)",
		Long: `Remove model(s) with wildcard support.

Parameters (all support wildcards * and ?):
  --node <name>   Node name pattern (default: * for all nodes)
                  Special values: @master (current master node), * (all nodes)
  --model <name>  Model name pattern (REQUIRED, supports wildcards)
  --force         Force deletion without confirmation

EXAMPLES:
  zzrouter rm llama3.2                              # Delete specific model
  zzrouter rm "llama*" --force                      # Delete all llama models
  zzrouter rm --node worker-1 --model "qwen*"        # Delete qwen models on worker-1 node
  zzrouter rm snowflake-arctic-embed:latest         # Delete by full name`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Use centralized parameter parsing (DRY)
			params, err := ParseModelQueryParams(cmd, args)
			if err != nil {
				return err
			}

			// Validate that model parameter is provided (required for rm)
			if params.Model == "" {
				return fmt.Errorf("--model parameter is required for rm command")
			}

			return runRm(nil, params, force) // clientConfig not needed since EnsureConnected handles it
		},
	}

	cmd.Flags().StringP("node", "n", "*", "Node name")
	cmd.Flags().StringP("model", "m", "", "Model name pattern (REQUIRED, supports wildcards)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force deletion without confirmation")

	return cmd
}

// runRm handles model deletion with unified parameters (DRY)
func runRm(_ *pkgConfig.ClientConfig, params *shared.ModelQueryParams, force bool) error {
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Query models
	models, err := client.ListModels(pkgClient.ModelFilter{Node: params.Node, Model: params.Model})
	if err != nil {
		return fmt.Errorf("failed to query models: %w", err)
	}

	if len(models) == 0 {
		fmt.Println(shared.FormatNoModelsFoundMessage(params.Node, "*", params.Model))
		return nil
	}

	// Convert for display
	matchingModels := ConvertModelsForRmCommand(models)

	count := len(matchingModels)

	// Determine if this is an exact match
	isExactMatch := false
	if count == 1 {
		// Check if this is an exact match (no wildcards used)
		modelName := matchingModels[0].Name
		// Check if the search pattern matches exactly (case-insensitive)
		// Also handle common model name formats (with/without namespace)
		searchPattern := strings.ToLower(params.Model)
		foundName := strings.ToLower(modelName)

		// Check for exact match or namespace match (e.g., "model-name" matches "org/model-name")
		isExactMatch = foundName == searchPattern ||
			strings.HasSuffix(foundName, "/"+searchPattern) ||
			strings.HasPrefix(searchPattern, foundName)
	}

	// Show what will be deleted (unless it's an exact match)
	if !isExactMatch {
		if count == 1 {
			fmt.Printf("Found 1 model matching '%s':\n", params.Model)
		} else {
			fmt.Printf("Found %d model(s) matching '%s':\n", count, params.Model)
		}
		for _, m := range matchingModels {
			fmt.Printf("  - %s\n", m.Name)
		}
		fmt.Println()
	}

	// Determine if we need confirmation
	// Skip confirmation if:
	// 1. --force flag is used, OR
	// 2. Exactly one model found AND it's an exact match
	needsConfirmation := !force && !isExactMatch

	// Ask for confirmation if needed
	if needsConfirmation {
		var prompt string
		if count == 1 {
			prompt = "Delete this model? [y/N]: "
		} else {
			prompt = "Delete these models? [y/N]: "
		}

		if !confirmYN(prompt) {
			fmt.Println("Deletion cancelled")
			return nil
		}
	}

	// Convert to original format for deletion
	var modelsToDelete []pkgClient.ModelMetadata
	for _, m := range matchingModels {
		for _, orig := range models {
			if orig.Name == m.FullID {
				modelsToDelete = append(modelsToDelete, orig)
				break
			}
		}
	}

	// Delete
	result, err := client.DeleteModelsFromRegistry(modelsToDelete)
	if err != nil {
		return fmt.Errorf("failed to delete: %w", err)
	}

	// Show results
	if result.Deleted > 0 {
		fmt.Printf("%s Deleted %d model(s)\n", ui.GetSuccessEmoji(), result.Deleted)
	}
	if len(result.Errors) > 0 {
		fmt.Printf("%s Failed %d model(s):\n", ui.GetErrorEmoji(), len(result.Errors))
		for _, errMsg := range result.Errors {
			fmt.Printf("  - %s\n", errMsg)
		}
		return fmt.Errorf("deletion completed with errors")
	}

	return nil
}

package clientcli

import (
	"context"
	"fmt"
	"maps"
	"strings"

	shared "github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewRunCmd creates the run command for loading models
func NewRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <model_spec>",
		Short: "Run a model in memory",
		Long: `Run a model by loading it into memory with pattern matching. After running, displays running instances.

Model matching requires exactly one match:
- If multiple models match, shows disambiguation table and exits
- Use --provider and --node flags to be more specific
- Use --force to automatically select the first match

Supports filtering with node and provider parameters:
  --node <name>  --provider <type>
  model_spec (positional argument, required)

Examples:
  # Basic usage (must match exactly one model)
  zzrouter run Qwen3-1.7B-MLX-4bit
  zzrouter run llama:latest

  # With provider specification
  zzrouter run Qwen/Qwen2.5-VL-3B --provider vllm
  
  # With parameters (repeatable kubectl style)
  zzrouter run Qwen/Qwen2.5-VL-3B --param gpu-memory-utilization=0.95 --param max-model-len=8192
  zzrouter run llama2 --param keep-alive=5m --param temperature=0.7
  
  # Force selection when multiple matches
  zzrouter run "Qwen*" --force
  
  # Dry-run to preview configuration
  zzrouter run Qwen/Qwen2.5-VL-3B --param gpu-memory-utilization=0.95 --dry-run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			save, _ := cmd.Flags().GetBool("save")
			stream, _ := cmd.Flags().GetBool("stream")
			watch, _ := cmd.Flags().GetBool("watch")
			provider, _ := cmd.Flags().GetString("provider")
			node, _ := cmd.Flags().GetString("node")
			model, _ := cmd.Flags().GetString("model")

			// Parse parameters from both formats (repeatable and comma-separated)
			params, err := parseParameterFlags(cmd)
			if err != nil {
				return err
			}

			// Parse environment variables from both formats
			envVars, err := parseEnvironmentFlags(cmd)
			if err != nil {
				return err
			}

			// Handle positional argument using DRY utility
			model, err = ParsePositionalModelArg(args, model)
			if err != nil {
				return err
			}

			// Validate that model is specified
			if model == "" {
				return fmt.Errorf("model must be specified either as positional argument or --model flag")
			}

			return runRun(nil, provider, node, model, force, dryRun, save, stream, watch, params, envVars) // clientConfig not needed since EnsureConnected handles it
		},
	}

	// Model Selection Flags (model is positional, only need provider and node)
	cmd.Flags().StringP("provider", "a", "", "Provider: vllm, ollama, llamacpp, mlx (defaults to * for all providers)")
	cmd.Flags().StringP("node", "n", "", "Node name (defaults to * for all nodes)")

	// Runtime Parameter Flags
	cmd.Flags().StringArray("param", nil, "Additional runtime parameter (repeatable, format: key=value)")
	cmd.Flags().StringToString("params", nil, "Additional runtime parameters (comma-separated, format: key=value,key2=value2)")

	// Environment Variable Flags
	cmd.Flags().StringArray("env", nil, "Environment variable (repeatable, format: KEY=value)")
	cmd.Flags().StringToString("envs", nil, "Environment variables (comma-separated, format: KEY=value,KEY2=value2)")

	// Control Flags
	cmd.Flags().BoolP("force", "f", false, "Force loading without confirmation")
	cmd.Flags().Bool("dry-run", false, "Preview configuration without running model")
	cmd.Flags().Bool("save", false, "Save parameters as defaults for this model")
	cmd.Flags().Bool("stream", false, "Stream logs after running model (like 'ps stream')")
	cmd.Flags().BoolP("watch", "w", false, "Stream launch progress until the instance reports ready")

	// `zzrouter run logs ...` — view/tail run stdout/stderr.
	cmd.AddCommand(NewRunLogsCmd())

	return cmd
}

// parseParameterFlags parses parameters from both --param (repeatable) and --params (comma-separated) flags
func parseParameterFlags(cmd *cobra.Command) (map[string]string, error) {
	result := make(map[string]string)

	// Parse comma-separated --params flag (backward compatibility)
	if cmd.Flags().Changed("params") {
		paramsMap, _ := cmd.Flags().GetStringToString("params")
		maps.Copy(result, paramsMap)
	}

	// Parse repeatable --param flags (kubectl style)
	if cmd.Flags().Changed("param") {
		paramArray, _ := cmd.Flags().GetStringArray("param")
		for _, param := range paramArray {
			key, value, err := splitKeyValue("--param", param)
			if err != nil {
				return nil, err
			}
			parts := []string{key, value}

			// Check if value contains comma (common mistake)
			if strings.Contains(parts[1], ",") {
				return nil, fmt.Errorf("invalid parameter value '%s': contains comma\n\n"+
					"To pass multiple parameters, use one of these formats:\n"+
					"  1. Multiple --param flags:  --param %s=%s --param <key2>=<value2>\n"+
					"  2. Use --params (plural):   --params \"%s=%s,<key2>=<value2>\"",
					param, parts[0], strings.Split(parts[1], ",")[0], parts[0], strings.Split(parts[1], ",")[0])
			}

			result[parts[0]] = parts[1]
		}
	}

	return result, nil
}

// splitKeyValue splits one key=value flag item; a missing '=' or an
// empty key is refused, naming the flag.
func splitKeyValue(flag, item string) (key, value string, err error) {
	key, value, ok := strings.Cut(item, "=")
	if !ok || key == "" {
		return "", "", fmt.Errorf("%s %q is not key=value", flag, item)
	}
	return key, value, nil
}

// parseEnvironmentFlags parses environment variables from both --env (repeatable) and --envs (comma-separated) flags
func parseEnvironmentFlags(cmd *cobra.Command) (map[string]string, error) {
	result := make(map[string]string)

	// Parse comma-separated --envs flag (backward compatibility)
	if cmd.Flags().Changed("envs") {
		envsMap, _ := cmd.Flags().GetStringToString("envs")
		maps.Copy(result, envsMap)
	}

	// Parse repeatable --env flags (kubectl style)
	if cmd.Flags().Changed("env") {
		envArray, _ := cmd.Flags().GetStringArray("env")
		for _, env := range envArray {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid environment variable format '%s', expected KEY=value", env)
			}
			result[parts[0]] = parts[1]
		}
	}

	return result, nil
}

func runRun(_ *pkgConfig.ClientConfig, provider, node, model string, force, dryRun, save, stream, watch bool, params, envVars map[string]string) error {
	// Use centralized sign-in logic
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Node defaults to "*" (any) when --node is not set. The registry
	// filter treats "*" as wildcard on the server side.
	nodeFilter := node
	if nodeFilter == "" {
		nodeFilter = "*"
	}

	models, err := runCandidates(client, nodeFilter, provider, model)
	if err != nil {
		return fmt.Errorf("failed to query models: %w", err)
	}

	if len(models) == 0 {
		fmt.Println(shared.FormatNoModelsFoundMessage(nodeFilter, provider, model))
		return nil
	}

	// Convert to ModelInfo for display using centralized utility
	matchingModels := ConvertModelsForRunCommand(models)

	// Model matching logic (similar to ps command)
	var selectedModel ModelInfo
	if len(matchingModels) == 1 {
		// Exactly one match - use it
		selectedModel = matchingModels[0]
		// Don't print success message yet - wait until API call succeeds
	} else if len(matchingModels) > 1 {
		if force {
			// Force mode: use first model
			selectedModel = matchingModels[0]
			// Don't print message yet - wait until API call succeeds
		} else {
			// Multiple matches - show disambiguation
			fmt.Printf("Multiple models match '%s':\n\n", model)
			rows := ConvertModelInfoToTableRows(matchingModels)
			displayUnifiedTableFromRows(rows)
			fmt.Printf("\nTotal: %d model(s)\n", len(matchingModels))
			return fmt.Errorf("multiple models match - please be more specific with --provider or --model flags, or use --force to select the first match")
		}
	} else {
		// This shouldn't happen since we checked len(models) == 0 above
		return fmt.Errorf("no models found")
	}

	// Use the provider from the selected model (no interactive selection)
	if selectedModel.Provider == "" || selectedModel.Provider == "-" {
		if provider != "" {
			// Use the provider specified in flags
			selectedModel.Provider = provider
		} else {
			return fmt.Errorf("model '%s' has no assigned provider. Please specify --provider flag", selectedModel.Name)
		}
	}

	// Load the selected model
	modelID := selectedModel.FullID
	if modelID == "" {
		modelID = selectedModel.Name
	}

	// Use force flag as specified by user
	forceLoad := force

	// Dry-run mode: get preview from server and display
	if dryRun {
		fmt.Printf("\n%s Fetching run preview from server...\n", ui.GetSearchEmoji())

		preview, err := client.PreviewRun(selectedModel.Node, modelID, selectedModel.Provider, params, envVars)
		if err != nil {
			return fmt.Errorf("failed to get preview: %w", err)
		}

		displayRunPreview(preview)
		return nil
	}

	loadResp, err := client.LoadModelWithProviderForceParamsAndEnv(selectedModel.Node, modelID, selectedModel.Provider, forceLoad, params, envVars)
	if err != nil {
		// Error messages from server are already user-friendly, just return them
		return err
	}

	// Validate response
	if loadResp == nil {
		return fmt.Errorf("empty response from server")
	}

	// Print success message now that API call succeeded
	if force {
		fmt.Printf("%s Running model: %s (force restart)\n", ui.GetCheckEmoji(), selectedModel.Name)
	} else {
		fmt.Printf("%s Running model: %s\n", ui.GetCheckEmoji(), selectedModel.Name)
	}

	if watch {
		if loadResp.JobID == "" {
			fmt.Printf("%s --watch: server returned no job_id (provider launched synchronously)\n", ui.GetInfoEmoji())
		} else {
			label := fmt.Sprintf("Launching %s", selectedModel.Name)
			if err := RenderJobProgress(context.Background(), client, loadResp.JobID, loadResp.Node, label); err != nil {
				return err
			}
		}
	}

	// Save parameters if --save flag is set
	if save && (len(params) > 0 || len(envVars) > 0) {
		fmt.Printf("\n💾 Saving parameters to provider config...\n")
		err = client.SaveModelDefaults(selectedModel.Node, modelID, selectedModel.Provider, params, envVars)
		if err != nil {
			fmt.Printf("%s Warning: Failed to save parameters: %v\n", ui.GetWarningEmoji(), err)
		} else {
			fmt.Printf("%s Parameters saved for model '%s'\n", ui.GetSuccessEmoji(), modelID)
			fmt.Printf("   Future runs will use these defaults automatically\n")
		}
	}

	// Handle streaming mode
	if stream {
		if loadResp.InstanceID == "" {
			// Service/external providers manage their own instances
			// and don't provide zzRouter instance IDs for log streaming
			fmt.Println()
			fmt.Printf("%s Log streaming is not available for service providers like '%s'\n",
				ui.GetWarningEmoji(), loadResp.App)
			fmt.Printf("   The model is running successfully. Use 'zzrouter ps' to see running instances.\n")
			if loadResp.App == constants.AppOllama {
				fmt.Printf("   For Ollama logs, use: ollama ps\n")
			}
			return nil // Success, just can't stream logs
		}
		fmt.Println()
		// Use instance ID directly from load response (much better UX!)
		fmt.Printf("Streaming logs for %s (press Ctrl+C to stop)...\n", modelID)
		fmt.Println(strings.Repeat("-", 80))
		err = client.StreamInstanceLogs(loadResp.InstanceID, selectedModel.Node)
		fmt.Println(strings.Repeat("-", 80))
		return err
	}

	// Non-streaming mode: show ps table
	err = runPs(nil, "", "") // Show all running instances
	if err != nil {
		fmt.Printf("Warning: Failed to display running instances: %v\n", err)
	}

	fmt.Printf("Use 'zzrouter ps' to monitor progress\n")
	fmt.Println()

	return nil
}

// displayRunPreview displays the run preview with full command details
// runModelLister is the catalog query run picks its model from.
type runModelLister interface {
	ListModels(f pkgClient.ModelFilter) ([]pkgClient.ModelMetadata, error)
}

// runCandidates lists the models run may pick from. --provider names the
// engine that would run the model, not the registry its weights came from.
func runCandidates(c runModelLister, node, provider, model string) ([]pkgClient.ModelMetadata, error) {
	return c.ListModels(pkgClient.ModelFilter{Node: node, Provider: provider, Model: model})
}

func displayRunPreview(preview *pkgClient.RunPreview) {
	// Instance Configuration
	fmt.Printf("Instance Configuration:\n")
	fmt.Printf("─────────────────────────────────────────────────────────\n")
	fmt.Printf("Model:    %s\n", preview.Model)
	fmt.Printf("Provider: %s\n", preview.App)
	if preview.Node != "" && preview.Node != "localhost" {
		fmt.Printf("Node:     %s\n", preview.Node)
	}
	fmt.Printf("Port:     %d\n", preview.Port)
	fmt.Printf("\n")

	// Full Command
	fmt.Printf("Command Line:\n")
	fmt.Printf("─────────────────────────────────────────────────────────\n")
	fmt.Printf("%s\n\n", preview.FullCommand)

	// Parameters
	if len(preview.Parameters) > 0 {
		fmt.Printf("⚙️  Parameters:\n")
		fmt.Printf("─────────────────────────────────────────────────────────\n")
		for k, v := range preview.Parameters {
			source := preview.ParameterSources[k]
			if source != "" {
				fmt.Printf("  %s: %s (%s)\n", k, v, source)
			} else {
				fmt.Printf("  %s: %s\n", k, v)
			}
		}
		fmt.Printf("\n")
	}

	// Environment Variables
	if len(preview.Environment) > 0 {
		fmt.Printf("🌍 Environment Variables:\n")
		fmt.Printf("─────────────────────────────────────────────────────────\n")
		for k, v := range preview.Environment {
			fmt.Printf("  export %s=%s\n", k, v)
		}
		fmt.Printf("\n")
	}

	// Working Directory
	if preview.WorkingDir != "" && preview.WorkingDir != "." {
		fmt.Printf("%s Working Directory:\n", ui.GetFolderEmoji())
		fmt.Printf("─────────────────────────────────────────────────────────\n")
		fmt.Printf("%s\n\n", preview.WorkingDir)
	}

	// Help text
	fmt.Printf("To start this run: remove --dry-run flag\n")
}

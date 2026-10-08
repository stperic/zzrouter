package clientcli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/constants"
)

// NewShowCmd creates the show command for displaying model information
func NewShowCmd() *cobra.Command {
	var (
		license    bool
		modelfile  bool
		parameters bool
		system     bool
		template   bool
		verbose    bool
		node       string
		provider   string
	)

	cmd := &cobra.Command{
		Use:   "show MODEL [flags]",
		Short: "Show information for a model",
		Long: `Show information for a model including architecture, parameters, capabilities, and more.

Supports cluster routing with --node flag to specify which node has the model.
If --node is not specified, automatically discovers which node has the model.

Examples:
  # Basic usage (auto-discovers node)
  zzrouter show qwen2.5-coder:1.5b-base
  zzrouter show llama3.2:1b --verbose

  # Specify node explicitly
  zzrouter show qwen3:32b --node worker-1
  zzrouter show codellama:34b --node gpu-server
  
  # Show specific information
  zzrouter show mistral --license
  zzrouter show qwen2.5-coder:1.5b-base --modelfile
  zzrouter show llama3.2:1b --parameters
  zzrouter show mistral --system
  zzrouter show qwen2.5-coder:1.5b-base --template`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			modelName := args[0]

			// Use centralized sign-in logic
			client, err := EnsureConnected()
			if err != nil {
				return err
			}

			// Query model information from server (with node/provider parameters for cluster routing)
			info, err := client.ShowModel(modelName, verbose, node, provider)
			if err != nil {
				return fmt.Errorf("failed to get model information: %w", err)
			}

			// Display based on flags (mutually exclusive)
			if license {
				displayLicense(info)
			} else if modelfile {
				displayModelfile(info)
			} else if parameters {
				displayParameters(info)
			} else if system {
				displaySystem(info)
			} else if template {
				displayTemplate(info)
			} else {
				// Default: show summary
				displaySummary(info, verbose)
			}

			return nil
		},
	}

	// Model Selection Flags (like zzrouter run)
	cmd.Flags().StringVarP(&node, "node", "n", "", "Target node name (auto-discovers if not specified)")
	cmd.Flags().StringVarP(&provider, "provider", "a", "", "Provider filter (currently not used for show)")

	// Display Flags
	cmd.Flags().BoolVarP(&license, "license", "", false, "Show license of a model")
	cmd.Flags().BoolVarP(&modelfile, "modelfile", "", false, "Show Modelfile of a model")
	cmd.Flags().BoolVarP(&parameters, "parameters", "", false, "Show parameters of a model")
	cmd.Flags().BoolVarP(&system, "system", "", false, "Show system message of a model")
	cmd.Flags().BoolVarP(&template, "template", "", false, "Show template of a model")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show detailed model information")

	return cmd
}

// displaySummary shows the default model information view
func displaySummary(info map[string]any, verbose bool) {
	provider := getStringValue(info, "_provider", "unknown")

	// Display format varies by provider
	switch provider {
	case constants.RepoOllama:
		displayOllamaSummary(info, verbose)
	case constants.RepoHuggingFace:
		displayHuggingFaceSummary(info, verbose)
	case "mlx":
		displayMLXSummary(info, verbose)
	case "openai-compatible":
		displayOpenAISummary(info, verbose)
	default:
		// Generic display
		displayGenericSummary(info, verbose)
	}
}

// displayOllamaSummary displays Ollama model information in Ollama's format
//
//nolint:gocyclo,cyclop // ollama show output has many optional fields; extracting a subfunc per field increases LOC without clarifying the flow
func displayOllamaSummary(info map[string]any, verbose bool) {
	fmt.Println("  Model")

	// Extract model details from the "details" field
	if details, ok := info["details"].(map[string]any); ok {
		// Architecture (family)
		if family, ok := details["family"].(string); ok {
			fmt.Printf("    %-20s %-10s\n", "architecture", family)
		}

		// Parameters (parameter_size)
		if paramSize, ok := details["parameter_size"].(string); ok {
			fmt.Printf("    %-20s %-10s\n", "parameters", paramSize)
		}

		// Context length from model_info
		if modelInfo, ok := info["model_info"].(map[string]any); ok {
			// Look for context_length in model_info fields (they use dotted notation)
			for key, val := range modelInfo {
				if strings.HasSuffix(key, ".context_length") {
					if ctxLen, ok := val.(float64); ok {
						fmt.Printf("    %-20s %-10.0f\n", "context length", ctxLen)
					}
				}
				if strings.HasSuffix(key, ".embedding_length") {
					if embLen, ok := val.(float64); ok {
						fmt.Printf("    %-20s %-10.0f\n", "embedding length", embLen)
					}
				}
			}
		}

		// Quantization level
		if quant, ok := details["quantization_level"].(string); ok {
			fmt.Printf("    %-20s %-10s\n", "quantization", quant)
		}
	}

	// Capabilities (if available)
	if capabilities, ok := info["capabilities"].([]any); ok && len(capabilities) > 0 {
		fmt.Println()
		fmt.Println("  Capabilities")

		for _, cap := range capabilities {
			if capStr, ok := cap.(string); ok {
				fmt.Printf("    %-20s\n", capStr)
			}
		}
	}

	// Parameters (if available) - Ollama returns this as a pre-formatted string
	if parametersStr, ok := info["parameters"].(string); ok && parametersStr != "" {
		fmt.Println()
		fmt.Println("  Parameters")

		// Parameters come pre-formatted from Ollama, just display them
		lines := strings.SplitSeq(parametersStr, "\n")
		for line := range lines {
			if strings.TrimSpace(line) != "" {
				// Ollama's format already has indentation, just print as-is
				fmt.Printf("    %s\n", line)
			}
		}
	}

	// License (if available) - only show first few lines unless verbose
	if license, ok := info["license"].(string); ok && license != "" {
		fmt.Println()
		fmt.Println("  License")

		// Display first few lines of license
		lines := strings.Split(license, "\n")
		maxLines := 3
		if !verbose {
			if len(lines) > maxLines {
				for i := range maxLines {
					trimmed := strings.TrimSpace(lines[i])
					if trimmed != "" {
						fmt.Printf("    %-28s\n", trimmed)
					}
				}
				fmt.Printf("    %-28s\n", "...")
			} else {
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					if trimmed != "" {
						fmt.Printf("    %-28s\n", trimmed)
					}
				}
			}
		} else {
			// In verbose mode, show full license
			for _, line := range lines {
				fmt.Printf("    %s\n", line)
			}
		}
	}

	// Verbose mode: show detailed model_info
	if verbose {
		if modelInfo, ok := info["model_info"].(map[string]any); ok {
			fmt.Println()
			fmt.Println("  Model Details")

			// Display all model_info fields
			for key, val := range modelInfo {
				// Skip arrays that are too large (like tokenizer.ggml.tokens)
				if arr, isArr := val.([]any); isArr {
					if len(arr) > 10 {
						fmt.Printf("    %-45s [array with %d items]\n", key, len(arr))
					} else if len(arr) == 0 {
						fmt.Printf("    %-45s []\n", key)
					} else {
						fmt.Printf("    %-45s %s\n", key, formatValue(val))
					}
				} else {
					fmt.Printf("    %-45s %s\n", key, formatValue(val))
				}
			}
		}

		// Tensors section (if available)
		if tensors, ok := info["tensors"].([]any); ok && len(tensors) > 0 {
			fmt.Println()
			fmt.Printf("  Tensors (%d total)\n", len(tensors))

			// Show all tensors in verbose mode
			for _, tensor := range tensors {
				if tensorMap, ok := tensor.(map[string]any); ok {
					name, _ := tensorMap["name"].(string)
					tensorType, _ := tensorMap["type"].(string)
					shape, _ := tensorMap["shape"].([]any)
					fmt.Printf("    %-40s %-10s %v\n", name, tensorType, shape)
				}
			}
		}
	}
}

// displayHuggingFaceSummary displays HuggingFace model information
// Uses harmonized Ollama-compatible structure (details, model_info, capabilities, license)
//
//nolint:gocyclo,cyclop // see displayOllamaSummary
func displayHuggingFaceSummary(info map[string]any, verbose bool) {
	fmt.Println("  Model")

	// Display details section (Ollama-compatible)
	if details, ok := info["details"].(map[string]any); ok {
		// Architecture (family)
		if family, ok := details["family"].(string); ok {
			fmt.Printf("    %-20s %-10s\n", "architecture", family)
		}

		// Parameters (parameter_size)
		if paramSize, ok := details["parameter_size"].(string); ok {
			fmt.Printf("    %-20s %-10s\n", "parameters", paramSize)
		}

		// Context length and embedding length from model_info
		if modelInfo, ok := info["model_info"].(map[string]any); ok {
			if maxPos, ok := modelInfo["max_position_embeddings"].(float64); ok {
				fmt.Printf("    %-20s %-10.0f\n", "context length", maxPos)
			}
			if hiddenSize, ok := modelInfo["hidden_size"].(float64); ok {
				fmt.Printf("    %-20s %-10.0f\n", "embedding length", hiddenSize)
			}
		}

		// Quantization level (dtype)
		if quant, ok := details["quantization_level"].(string); ok {
			fmt.Printf("    %-20s %-10s\n", "quantization", quant)
		}
	}

	// Capabilities (Ollama-compatible)
	if capabilities, ok := info["capabilities"].([]any); ok && len(capabilities) > 0 {
		fmt.Println()
		fmt.Println("  Capabilities")

		for _, cap := range capabilities {
			if capStr, ok := cap.(string); ok {
				fmt.Printf("    %-20s\n", capStr)
			}
		}
	}

	// License (Ollama-compatible)
	if license, ok := info["license"].(string); ok && license != "" {
		fmt.Println()
		fmt.Println("  License")

		// Display license (truncated unless verbose)
		if !verbose && len(license) > 100 {
			fmt.Printf("    %s...\n", license[:100])
		} else {
			fmt.Printf("    %s\n", license)
		}
	}

	// HuggingFace-specific metadata (if available)
	if hfMeta, ok := info["huggingface"].(map[string]any); ok {
		if downloads, ok := hfMeta["downloads"].(float64); ok {
			fmt.Println()
			fmt.Printf("    %-20s %.0f\n", "downloads", downloads)
		}
		if likes, ok := hfMeta["likes"].(float64); ok {
			fmt.Printf("    %-20s %.0f\n", "likes", likes)
		}

		// Display model card tags in non-verbose mode
		if !verbose {
			if modelCard, ok := hfMeta["model_card"].(map[string]any); ok {
				if tags, ok := modelCard["tags"].([]string); ok && len(tags) > 0 {
					fmt.Printf("    %-20s %s\n", "tags", strings.Join(tags, ", "))
				}
			}
		}
	}

	// Verbose mode: show full model_info and README
	if verbose {
		if modelInfo, ok := info["model_info"].(map[string]any); ok {
			fmt.Println()
			fmt.Println("  Model Details")

			for key, val := range modelInfo {
				fmt.Printf("    %-30s %v\n", key, formatValue(val))
			}
		}

		// Show README if available
		if hfMeta, ok := info["huggingface"].(map[string]any); ok {
			if readme, ok := hfMeta["readme"].(string); ok && readme != "" {
				fmt.Println()
				fmt.Println("  README")
				fmt.Println()
				// Display first 50 lines of README
				lines := strings.Split(readme, "\n")
				maxLines := 50
				for i, line := range lines {
					if i >= maxLines {
						fmt.Printf("    ... (%d more lines)\n", len(lines)-maxLines)
						break
					}
					fmt.Printf("    %s\n", line)
				}
			}
		}
	}
}

// displayMLXSummary displays MLX model information.
// verbose is accepted to match the dispatch signature but unused here.
func displayMLXSummary(info map[string]any, _ bool) {
	fmt.Println("  Model")

	if model, ok := info["model"].(map[string]any); ok {
		if id, ok := model["id"].(string); ok {
			fmt.Printf("    %-20s %s\n", "id", id)
		}
		if created, ok := model["created"].(float64); ok {
			fmt.Printf("    %-20s %.0f\n", "created", created)
		}
		if owned, ok := model["owned_by"].(string); ok {
			fmt.Printf("    %-20s %s\n", "owned by", owned)
		}
	}

	if note, ok := info["_note"].(string); ok {
		fmt.Println()
		fmt.Printf("  Note: %s\n", note)
	}
}

// displayOpenAISummary displays OpenAI-compatible model information
func displayOpenAISummary(info map[string]any, verbose bool) {
	fmt.Println("  Model")

	if model, ok := info["model"].(map[string]any); ok {
		if id, ok := model["id"].(string); ok {
			fmt.Printf("    %-20s %s\n", "id", id)
		}
		if created, ok := model["created"].(float64); ok {
			fmt.Printf("    %-20s %.0f\n", "created", created)
		}
		if owned, ok := model["owned_by"].(string); ok {
			fmt.Printf("    %-20s %s\n", "owned by", owned)
		}

		if verbose {
			// Display all fields
			fmt.Println()
			fmt.Println("  All Fields")
			for k, v := range model {
				fmt.Printf("    %-20s %s\n", k, formatValue(v))
			}
		}
	}

	if note, ok := info["_note"].(string); ok {
		fmt.Println()
		fmt.Printf("  Note: %s\n", note)
	}
}

// displayGenericSummary displays generic model information.
// verbose is accepted to match the dispatch signature but unused here.
func displayGenericSummary(info map[string]any, _ bool) {
	fmt.Println("  Model Information")
	fmt.Println()

	// Pretty print the JSON
	jsonBytes, err := json.MarshalIndent(info, "  ", "  ")
	if err != nil {
		fmt.Printf("  %v\n", info)
	} else {
		fmt.Println(string(jsonBytes))
	}
}

// displayLicense shows only the license information
func displayLicense(info map[string]any) {
	if license, ok := info["license"].(string); ok && license != "" {
		fmt.Println(license)
	} else {
		fmt.Println("No license information available for this model")
	}
}

// displayModelfile shows the Modelfile information
func displayModelfile(info map[string]any) {
	if modelfile, ok := info["modelfile"].(string); ok && modelfile != "" {
		fmt.Println(modelfile)
	} else {
		fmt.Println("No Modelfile information available for this model")
		fmt.Println("(Modelfile is typically only available for Ollama models)")
	}
}

// displayParameters shows model parameters (matches Ollama format)
func displayParameters(info map[string]any) {
	if params, ok := info["parameters"].(string); ok && params != "" {
		fmt.Println(params)
	} else if params, ok := info["parameters"].(map[string]any); ok {
		// Display parameters in Ollama format (without extra header when using --parameters flag)
		for k, v := range params {
			// Format value based on type
			formattedValue := formatParamValue(v)
			fmt.Printf("%-20s %s\n", k, formattedValue)
		}
	} else {
		fmt.Println("No parameter information available for this model")
	}
}

// displaySystem shows system message
func displaySystem(info map[string]any) {
	if system, ok := info["system"].(string); ok && system != "" {
		fmt.Println(system)
	} else {
		fmt.Println("No system message configured for this model")
	}
}

// displayTemplate shows the template
func displayTemplate(info map[string]any) {
	if template, ok := info["template"].(string); ok && template != "" {
		fmt.Println(template)
	} else {
		fmt.Println("No template information available for this model")
	}
}

// Helper functions

func getStringValue(info map[string]any, key, defaultValue string) string {
	if val, ok := info[key].(string); ok {
		return val
	}
	return defaultValue
}

func formatValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		// Check if it's an integer
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%.2f", val)
	case bool:
		return fmt.Sprintf("%v", val)
	case []any:
		if len(val) == 0 {
			return "[]"
		}
		return fmt.Sprintf("[%d items]", len(val))
	case map[string]any:
		return fmt.Sprintf("{%d fields}", len(val))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func formatParamValue(v any) string {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("\"%s\"", val)
	case []any:
		// For arrays like stop tokens
		var items []string
		for _, item := range val {
			if str, ok := item.(string); ok {
				items = append(items, fmt.Sprintf("\"%s\"", str))
			} else {
				items = append(items, fmt.Sprintf("%v", item))
			}
		}
		return strings.Join(items, ", ")
	case float64:
		// Check if it's an integer
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%.2f", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

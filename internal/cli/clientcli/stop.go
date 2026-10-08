package clientcli

import (
	"fmt"

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// NewStopCmd creates the stop command for unloading models
func NewStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop <model_spec|id>",
		Short: "Stop a running model",
		Long: `Stop models by unloading them from memory. Supports both instance IDs and model names.

Supports wildcards (*) and glob patterns for flexible matching.

For on-demand providers (llama.cpp, vLLM), this stops the process.
For service providers (Ollama), this unloads the model from memory.

Examples:
  zzrouter stop 06c1097efce0  # Stop by instance ID (from ps output)
  zzrouter stop llama2         # Stop specific model by name
  zzrouter stop llama2*        # Stop all models matching llama2*
  zzrouter stop Qwen*          # Stop all Qwen models`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			return runStop(args[0], force)
		},
	}

	cmd.Flags().BoolP("force", "f", false, "Force stop without confirmation")
	return cmd
}

func runStop(modelSpec string, force bool) error {
	// Use centralized sign-in logic
	client, err := EnsureConnected()
	if err != nil {
		return err
	}

	// Query running instances (same as ps command)
	instances, err := client.ListInstances()
	if err != nil {
		return fmt.Errorf("failed to query running instances: %w", err)
	}

	if len(instances) == 0 {
		fmt.Println("No running instances")
		return nil
	}

	// Filter instances by model name or instance ID (DRY utility)
	matchingInstances := FilterInstancesBySpec(instances, modelSpec)

	if len(matchingInstances) == 0 {
		return fmt.Errorf("no running instances found matching: %s", modelSpec)
	}

	// Confirmation prompt unless --force is used
	if !force {
		if !confirmStop(matchingInstances) {
			fmt.Println("Operation cancelled")
			return nil
		}
	}

	// Stop each instance
	if len(matchingInstances) > 1 {
		fmt.Println("\nStopping instances...")
	}
	successCount := 0
	failCount := 0

	for _, inst := range matchingInstances {
		err := client.StopInstance(inst.ID, inst.Node)
		if err != nil {
			fmt.Printf("  %s Failed to stop %s (%s): %v\n", ui.GetErrorEmoji(), inst.Model, inst.ID, err)
			failCount++
		} else {
			fmt.Printf("  %s Stopped %s (%s)\n", ui.GetCheckEmoji(), inst.Model, inst.App)
			successCount++
		}
	}

	// Summary
	if failCount > 0 {
		fmt.Printf(", %d failed", failCount)
	}
	fmt.Println()

	return nil
}

// confirmStop prompts user to confirm stopping instances
// Returns true immediately for single instance (no prompt needed)
// Asks for confirmation only when stopping multiple instances
func confirmStop(instances []pkgClient.Instance) bool {
	if len(instances) == 1 {
		// Single instance - no confirmation needed, just stop it
		inst := instances[0]
		fmt.Printf("Stopping %s (%s)...\n", inst.Model, inst.App)
		return true
	}

	// Multiple instances - show list and ask for confirmation.
	// Deny by default on non-TTY so piped invocations don't silently
	// stop every matched instance.
	fmt.Printf("\nFound %d running instance(s):\n", len(instances))
	displayPsTable(instances)
	prompt := fmt.Sprintf("\nStop all %d instance(s)? (y/N): ", len(instances))
	return confirmYN(prompt)
}

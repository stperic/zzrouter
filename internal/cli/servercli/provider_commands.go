package servercli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/internal/cli/servercli/confighelpers"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// NewProviderCmd creates the top-level providers command.
func NewProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "providers",
		Aliases: []string{"provider"},
		Short:   "Manage inference providers",
		Long: `Manage inference providers for LLM serving.

Enable or disable inference providers such as Ollama, vLLM, llama.cpp, and MLX.
Matches the 'providers' config section and providers/ directory layout.`,
	}

	cmd.AddCommand(
		newProviderEnableCmd(),
		newProviderDisableCmd(),
		newProviderListCmd(),
	)

	return cmd
}

func newProviderEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <provider>",
		Short: "Enable a provider",
		Long: `Enable a specific inference provider.

Available inference providers: ollama, vllm, llamacpp, mlx`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return toggleApp(args[0], true)
		},
	}
}

func newProviderDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <provider>",
		Short: "Disable a provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return toggleApp(args[0], false)
		},
	}
}

func newProviderListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all providers and their status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listApps()
		},
	}
}

// toggleApp enables or disables an app (uses DRY helper)
func toggleApp(appName string, enable bool) error {
	// Use shared helper function with context
	ctx := context.Background()
	if err := confighelpers.ToggleProviderEnabled(ctx, appName, enable); err != nil {
		return err
	}

	action := "disabled"
	if enable {
		action = "enabled"
	}
	fmt.Printf("Provider '%s' %s\n", appName, action)
	fmt.Println(" Node restart required for changes to take effect")
	fmt.Println()

	return nil
}

// listApps displays all configured inference providers and their status.
func listApps() error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	appsDir := cm.GetAppsConfigDir()

	appsConfig, err := pkgConfig.LoadAppsConfig(appsDir)
	if err != nil {
		return fmt.Errorf("failed to load apps config: %w", err)
	}

	ShowProvidersTable(appsConfig)
	return nil
}

package servercli

import (
	"fmt"

	"github.com/spf13/cobra"
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

// toggleApp records this node's choice to run a provider, through the
// store write the server's finalization uses. A worker's copy of a synced
// provider is the coordinator's, so there it refuses and names the request.
func toggleApp(appName string, enable bool) error {
	node, err := pkgConfig.NewConfigManager("zzrouter").LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load node config: %w", err)
	}
	store, err := pkgConfig.NewAppsConfigStoreFromStandardLocations(node.Node.Name)
	if err != nil {
		return fmt.Errorf("failed to load providers config: %w", err)
	}
	if sc, ok := store.Config().LookupApp(appName); ok && sc.SyncedToWorkers() && node.Cluster.Mode == pkgConfig.ClusterModeWorker {
		return fmt.Errorf("this worker's copy of %s is the coordinator's: send PATCH /zzrouter/v1/providers/%s?node=%s with {\"enabled\": %t} to the coordinator", appName, appName, node.Node.Name, enable)
	}
	if _, err := store.SetProviderEnabled(appName, node.Node.Name, enable); err != nil {
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

// listApps displays all configured inference providers and their status
// on this node.
func listApps() error {
	node, err := pkgConfig.NewConfigManager("zzrouter").LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load node config: %w", err)
	}
	store, err := pkgConfig.NewAppsConfigStoreFromStandardLocations(node.Node.Name)
	if err != nil {
		return fmt.Errorf("failed to load apps config: %w", err)
	}
	ShowProvidersTable(store.Config())
	return nil
}

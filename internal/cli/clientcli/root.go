// Package clientcli wires the zzrouter client cobra tree.
// cmd/zzrouter/main.go is a 5-line delegation point.
package clientcli

import (
	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/version"
)

const (
	groupModel   = "model"
	groupCluster = "cluster"
	groupUtility = "utility"
)

// init defers the client emoji preference to first use so `--help`,
// `version`, completion, and every error path avoid the YAML read.
func init() {
	ui.SetPreferenceLoader(func() string {
		cm := config.NewConfigManager("zzrouter")
		if cfg, err := cm.LoadClientConfig(); err == nil {
			return cfg.Preferences.Emoji
		}
		return ""
	})
}

// Execute runs the zzrouter root command. Returns the exit code; callers
// (cmd/zzrouter/main.go) should os.Exit with it.
func Execute() int {
	if err := newRootCmd().Execute(); err != nil {
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "zzrouter",
		Short: "zzrouter - AI model router client (Ollama-compatible)",
		Long: `zzrouter is a high-performance AI model router client written in Go.
It provides Ollama-compatible commands for managing AI models across distributed clusters.

Model commands work exactly like Ollama. Cluster commands provide multi-node management.

Quick Start:
  zzrouter quickstart              # Start the server and connect
  zzrouter connect <host:port>     # Connect to a remote node
  zzrouter deploy llama3.2:8b      # Deploy a model (alias: pull)
  zzrouter run llama3.2:8b         # Run a model
  zzrouter status                  # Check cluster health`,
		Version:      version.Current.String(),
		SilenceUsage: true,
	}

	root.AddGroup(
		&cobra.Group{ID: groupModel, Title: "Model Commands"},
		&cobra.Group{ID: groupCluster, Title: "Cluster Commands"},
		&cobra.Group{ID: groupUtility, Title: "Utility Commands"},
	)

	root.PersistentFlags().StringP("output", "o", "", "Output format: json, table, yaml")

	addGroup(root, groupModel,
		NewListCmd(),
		NewRunCmd(),
		NewDeployCmd(),
		NewRmCmd(),
		NewPsCmd(),
		NewStopCmd(),
		NewShowCmd(),
		NewSearchCmd(),
	)
	addGroup(root, groupCluster,
		NewConnectCmd(),
		NewDisconnectCmd(),
		NewStatusCmd(),
		NewNodesCmd(),
		NewProvidersCmd(),
		NewKeysCmd(),
	)
	addGroup(root, groupUtility,
		NewQuickstartCmd(),
		NewTUICmd(),
		NewLogsCmd(),
		NewConfigCmd(),
		NewVersionCmd(),
	)

	return root
}

func addGroup(root *cobra.Command, id string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.GroupID = id
		root.AddCommand(c)
	}
}

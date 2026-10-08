package servercli

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/security"
)

// requireConfig is a cobra PreRunE that ensures node.yaml + providers/
// exist before the command runs, offering interactive init when they
// don't. Use on subcommands that mutate config; init/reset create it
// and must not use this gate.
func requireConfig(_ *cobra.Command, _ []string) error {
	return ensureConfigExists()
}

// ensureConfigExists checks if configuration exists and offers to initialize if missing.
func ensureConfigExists() error {
	// XDG-only: Use config manager to check for existing config
	cm := pkgConfig.NewConfigManager("zzrouter")
	configPath := cm.GetNodeConfigPath()
	appsDir := cm.GetAppsConfigDir()

	configExists := false
	if _, err := os.Stat(configPath); err == nil {
		configExists = true
	}
	appsExists := false
	if info, err := os.Stat(appsDir); err == nil && info.IsDir() {
		appsExists = true
	}

	if !configExists || !appsExists {
		if !configExists && !appsExists {
			fmt.Println(" No configuration files found!")
			fmt.Println("   Missing: node.yaml and providers/ directory")
		} else if !configExists {
			fmt.Println(" Node configuration missing!")
			fmt.Println("   Missing: node.yaml")
		} else {
			fmt.Println(" Provider configuration missing!")
			fmt.Println("   Missing: providers/ directory")
		}
		fmt.Println()
		fmt.Print("Would you like to initialize configuration now? [y/N]: ")

		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			fmt.Println()
			fmt.Println("Initializing configuration...")

			// Run init with force to create fresh config
			if err := runNodeInit(true); err != nil {
				return fmt.Errorf("failed to initialize configuration: %w", err)
			}

			fmt.Println()
			fmt.Println("✓ Configuration initialized successfully!")
			return nil
		} else {
			fmt.Println()
			fmt.Println("Configuration required. Run 'zzrouter-node init' when ready.")
			return fmt.Errorf("configuration not found")
		}
	}

	return nil
}

// NewConfigCmd creates the parent config command for managing existing configuration.
func NewConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage zzRouter configuration",
		Long: `Configure and modify zzRouter settings.

Run without a subcommand for an interactive menu, or pass --edit to open
the config file directly in your preferred editor.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			edit, _ := cmd.Flags().GetBool("edit")
			if edit {
				return runNodeConfigEdit()
			}
			if err := ensureConfigExists(); err != nil {
				return err
			}
			return runNodeConfigMenu()
		},
	}

	cmd.Flags().BoolP("edit", "e", false, "Edit configuration file in your preferred editor")

	// Add subcommands
	cmd.AddCommand(
		newNodeConfigInitCmd(), // Initialize/reinitialize configuration
		newNodeConfigResetCmd(),
		newNodeConfigNodenameCmd(),
		newNodeConfigProvidersCmd(),
		newNodeConfigTLSInitCmd(),
	)

	return cmd
}

// newNodeConfigNodenameCmd creates the hostname configuration subcommand
func newNodeConfigNodenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hostname",
		Short: "Update server hostname",
		Long: `Update the hostname for this zzRouter server instance.

This modifies the 'server.name' field in node.yaml.`,
		PreRunE: requireConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeConfigNodename()
		},
	}

	return cmd
}

// newNodeConfigProvidersCmd creates the providers configuration subcommand.
func newNodeConfigProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "Update inference provider configuration",
		Long: `Configure inference providers for zzRouter.

This allows you to enable/disable and configure different LLM providers
like Ollama, vLLM, llama.cpp, and MLX.`,
		PreRunE: requireConfig,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeConfigAppsUpdate()
		},
	}

	return cmd
}

// runNodeConfigMenu shows the configuration selection menu
func runNodeConfigMenu() error {
	for {
		fmt.Println(".zzRouter Configuration Management")
		fmt.Println()
		fmt.Println("What would you like to configure?")
		fmt.Println("  1. Update hostname")
		fmt.Println("  2. Update inference provider configuration")
		fmt.Println("  3. Exit")
		fmt.Println()

		fmt.Print("Selection [1-3]: ")

		var selection string
		_, _ = fmt.Scanln(&selection)
		selection = strings.TrimSpace(selection)

		if selection == "" {
			fmt.Println("Exiting configuration menu.")
			return nil
		}

		var err error
		switch selection {
		case "1":
			err = runNodeConfigNodename()
		case "2":
			err = runNodeConfigAppsUpdate()
		case "3":
			fmt.Println("Exiting configuration menu.")
			return nil
		default:
			fmt.Println("Invalid selection. Please try again.")
			fmt.Println()
			continue
		}

		// If there was an error, show it and return
		if err != nil {
			return err
		}

		// Add spacing before showing menu again
		fmt.Println()
	}
}

// runNodeConfigNodename handles hostname configuration
func runNodeConfigNodename() error {
	fmt.Println()
	fmt.Println("Updating hostname configuration...")
	fmt.Println()

	// Check if zzrouter server is running and ask user permission to stop it
	serverWasStopped, err := checkAndAskToStopRunningNode()
	if err != nil {
		return err
	}

	// XDG-only: Use config manager paths
	cm := pkgConfig.NewConfigManager("zzrouter")
	configPath := cm.GetNodeConfigPath()

	hostname := promptForNodenameUpdate(configPath)
	if hostname != "" {
		if err := updateNodenameInConfig(configPath, hostname); err != nil {
			return fmt.Errorf("failed to update hostname: %w", err)
		}
		fmt.Println()
		fmt.Println("Nodename updated successfully!")
		if serverWasStopped {
			fmt.Println(" Node was stopped. Please restart with: zzrouter-node start")
		}
	} else {
		fmt.Println()
		fmt.Println("Nodename unchanged.")
	}

	return nil
}

// runNodeConfigAppsUpdate handles app configuration
func runNodeConfigAppsUpdate() error {
	fmt.Println()
	fmt.Println("Updating app configuration...")
	fmt.Println()

	// Check if zzrouter server is running and ask user permission to stop it
	serverWasStopped, err := checkAndAskToStopRunningNode()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()

	// XDG-only: Use config manager paths
	cm := pkgConfig.NewConfigManager("zzrouter")
	appsDir := cm.GetAppsConfigDir()

	appsConfig, err := pkgConfig.LoadAppsConfig(appsDir)
	if err != nil {
		return fmt.Errorf("failed to load providers config: %w", err)
	}

	// Run provider configurator (skip confirmation - user explicitly chose this)
	providerConfigs, err := RunProviderConfigurator(ctx, appsConfig, true)
	if err != nil {
		return fmt.Errorf("provider configurator failed: %w", err)
	}

	if providerConfigs != nil {
		if err := updateAppsYAML(appsDir, providerConfigs); err != nil {
			return fmt.Errorf("failed to update providers config: %w", err)
		}
		fmt.Println()
		fmt.Println("Provider configuration updated successfully!")
		if serverWasStopped {
			fmt.Println(" Node was stopped. Please restart with: zzrouter-node start")
		}
		fmt.Println()
	} else {
		fmt.Println("No changes made to app configuration.")
	}

	return nil
}

// newNodeConfigResetCmd creates the reset configuration subcommand
func newNodeConfigResetCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset node configuration to defaults",
		Long: `Reset all node configuration files (node.yaml, provider config) to their default values.

This overwrites existing configuration with the built-in defaults.
The server will be stopped if currently running.

Examples:
  zzrouter-node config reset        # Prompts for confirmation
  zzrouter-node config reset --yes  # Skip confirmation prompt`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeConfigReset(yes)
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// runNodeConfigReset resets node configuration to defaults
func runNodeConfigReset(yes bool) error {
	if !yes {
		fmt.Print("Reset node.yaml and provider config to defaults? [y/N]: ")

		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))

		if response != "y" && response != "yes" {
			fmt.Println("Reset cancelled.")
			return nil
		}
	}

	// Check if server is running and ask user permission to stop it
	serverWasStopped, err := checkAndAskToStopRunningNode()
	if err != nil {
		return err
	}

	// Overwrite all config files with defaults
	_, err = ensureConfigStructure(false, false)
	if err != nil {
		return fmt.Errorf("failed to reset configuration: %w", err)
	}

	fmt.Println("Configuration reset to defaults.")
	if serverWasStopped {
		fmt.Println("Node was stopped. Restart with: zzrouter-node start")
	}

	return nil
}

// newNodeConfigInitCmd creates the init configuration subcommand
func newNodeConfigInitCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize or reinitialize zzRouter configuration",
		Long: `Initialize a new zzRouter host with default configuration files.

This command creates:
  - node.yaml: Node configuration file (including cluster settings)
  - provider config: Inference providers configuration file
  - models/: Shared model repository directory
  - logs/: Shared logs directory
  - cache/: Created automatically when needed (for download lifecycle and instances)

The provider configuration includes:
  - Ollama (service)
  - vLLM (on-demand)
  - llama.cpp (on-demand)
  - MLX (Apple Silicon, on-demand)

Cluster membership is driven by pairing ('zzrouter-node cluster pair' on
the worker, 'cluster accept <code>' on the coordinator).

All services are configured to share the same model repository.

Examples:
  # Initialize configuration
  zzrouter-node config init

  # Overwrite existing configuration
  zzrouter-node config init --force`,
		SilenceUsage: true, // Don't show usage on errors
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeInit(force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Overwrite existing files")

	return cmd
}

// newNodeConfigTLSInitCmd creates the TLS certificate generation subcommand
func newNodeConfigTLSInitCmd() *cobra.Command {
	var (
		hosts   []string
		ipAddrs []string
		days    int
		outDir  string
	)

	cmd := &cobra.Command{
		Use:   "tls-init",
		Short: "Generate self-signed TLS certificates for zzRouter",
		Long: `Generate a self-signed CA and server certificate for zzRouter TLS.

This creates four files in the output directory:
  ca.pem         CA certificate (distribute to all nodes and clients)
  ca-key.pem     CA private key (keep secure, needed to sign new certs)
  server.pem     Server certificate (used in node.yaml tls_cert)
  server-key.pem Server private key (used in node.yaml tls_key)

After generation, update node.yaml:
  node:
    tls_cert: <output-dir>/server.pem
    tls_key: <output-dir>/server-key.pem
  cluster:
    tls_ca_cert: <output-dir>/ca.pem

And cli.yaml:
  node:
    secure: true
    tls_ca_cert: <output-dir>/ca.pem

Examples:
  # Generate with auto-detected hostname and IP
  zzrouter-node config tls-init

  # Specify additional hostnames and IPs for the certificate
  zzrouter-node config tls-init --host coordinator.local --host worker1 --ip 192.0.2.1

  # Custom output directory and validity
  zzrouter-node config tls-init --out /etc/zzrouter/certs --days 730`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Auto-detect hostname if none specified
			if len(hosts) == 0 {
				if h, err := os.Hostname(); err == nil {
					hosts = append(hosts, h)
				}
				hosts = append(hosts, "localhost")
			}

			// Parse IP addresses
			var ips []net.IP
			ips = append(ips, net.IPv4(127, 0, 0, 1))
			for _, addr := range ipAddrs {
				ip := net.ParseIP(addr)
				if ip == nil {
					return fmt.Errorf("invalid IP address: %s", addr)
				}
				ips = append(ips, ip)
			}

			// Auto-detect outbound IP
			if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
				if localAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
					ips = append(ips, localAddr.IP)
				}
				_ = conn.Close()
			}

			// Default output directory
			if outDir == "" {
				cm := pkgConfig.NewConfigManager("zzrouter")
				outDir = filepath.Join(filepath.Dir(cm.GetNodeConfigPath()), "certs")
			}

			fmt.Printf("Generating TLS certificates...\n")
			fmt.Printf("  Output:    %s\n", outDir)
			fmt.Printf("  Hostnames: %s\n", strings.Join(hosts, ", "))
			ipStrs := make([]string, len(ips))
			for i, ip := range ips {
				ipStrs[i] = ip.String()
			}
			fmt.Printf("  IPs:       %s\n", strings.Join(ipStrs, ", "))
			fmt.Printf("  Valid:     %d days\n\n", days)

			paths, err := security.GenerateSelfSignedCerts(outDir, hosts, ips, days)
			if err != nil {
				return fmt.Errorf("failed to generate certificates: %w", err)
			}

			fmt.Printf("Certificates generated:\n")
			fmt.Printf("  CA cert:     %s\n", paths.CACert)
			fmt.Printf("  CA key:      %s  (keep secure!)\n", paths.CAKey)
			fmt.Printf("  Server cert: %s\n", paths.ServerCert)
			fmt.Printf("  Server key:  %s\n\n", paths.ServerKey)

			fmt.Printf("Add to node.yaml:\n")
			fmt.Printf("  node:\n")
			fmt.Printf("    tls_cert: %s\n", paths.ServerCert)
			fmt.Printf("    tls_key: %s\n", paths.ServerKey)
			fmt.Printf("  cluster:\n")
			fmt.Printf("    tls_ca_cert: %s\n\n", paths.CACert)

			fmt.Printf("Add to cli.yaml:\n")
			fmt.Printf("  node:\n")
			fmt.Printf("    secure: true\n")
			fmt.Printf("    tls_ca_cert: %s\n", paths.CACert)

			return nil
		},
	}

	cmd.Flags().StringSliceVar(&hosts, "host", nil, "Hostnames for the certificate SAN (auto-detected if empty)")
	cmd.Flags().StringSliceVar(&ipAddrs, "ip", nil, "IP addresses for the certificate SAN (auto-detected if empty)")
	cmd.Flags().IntVar(&days, "days", 365, "Certificate validity in days")
	cmd.Flags().StringVar(&outDir, "out", "", "Output directory (default: config dir/certs)")

	return cmd
}

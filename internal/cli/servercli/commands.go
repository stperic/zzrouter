package servercli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/service"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Insecure dev-only fallback. The start flow refuses to use this unless
// ZZROUTER_ALLOW_DEV_KEY=1 is set, so a misconfigured prod box fails
// loud at boot instead of silently accepting a publicly-known credential.
const (
	insecureDevAdminKey = "dev-key-please-change-in-production" //nolint:gosec // gated by ZZROUTER_ALLOW_DEV_KEY

	// allowDevKeyEnv opts a process into the insecure dev-key fallback.
	// Unset (the default) = hard-fail when no admin key is configured.
	allowDevKeyEnv = "ZZROUTER_ALLOW_DEV_KEY"
)

// NewStartCmd creates the start subcommand.
func NewStartCmd() *cobra.Command {
	var configDir string
	var noSetup bool

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the zzRouter server",
		Long: `Start the zzRouter server that acts as a reverse proxy
for various LLM providers like vllm, llama.cpp, and ollama.

The server will route OpenAI-compatible API requests to the appropriate
provider based on the requested model name.

By default, automatically discovers and uses all available providers.
Use --config to use the configuration file.

Service Setup (Linux/macOS/Windows):
When run with elevated privileges (sudo/admin), first-run will:
  - Create a dedicated 'zzrouter' service user (Linux only)
  - Set up directories with proper permissions
  - Install the system service (systemd/launchd/Windows SCM)
  - Generate a secure API key

Cluster Membership:
Use 'zzrouter-node cluster pair' on a worker to join a coordinator, then
'zzrouter-node cluster accept <code>' on the coordinator.

Config Directory Priority:
1. -C, --dir flag (highest priority)
2. ZZROUTER_CONFIG_DIR environment variable
3. Current directory, then user config directory (~/.config/zzrouter/)

Examples:
  sudo zzrouter-node start               # Production: auto-setup service user
  zzrouter-node start                    # Development: run as current user
  zzrouter-node start -C /opt/zzrouter      # Use custom config directory
  zzrouter-node start --debug            # Enable debug logging
  zzrouter-node start --detach           # Run in background (clean output)
  zzrouter-node cluster pair             # Join this worker to a coordinator
  ZZROUTER_CONFIG_DIR=/opt/zzrouter zzrouter-node start  # Use environment variable`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get the service manager for the current platform
			svcMgr := service.NewServiceManager()

			// Handle service setup when running as root/admin (unless --no-setup is set)
			if svcMgr.IsPrivileged() && !noSetup {
				return runPrivilegedStart(cmd, svcMgr, configDir)
			}

			// Running as non-root: either service user or development mode
			return runNormalStart(cmd, configDir, svcMgr)
		},
	}

	// Add flags specific to serve command
	cmd.Flags().IntP("port", "P", constants.DefaultZZROUTERPort, "Port to bind the server to")
	cmd.Flags().BoolP("detach", "d", false, "Run server in background (detached mode)")
	cmd.Flags().Bool("debug", false, "Enable debug logging output")
	cmd.Flags().StringVarP(&configDir, "dir", "C", "", "Config directory (overrides ZZROUTER_CONFIG_DIR)")
	cmd.Flags().BoolVar(&noSetup, "no-setup", false, "Skip service setup (used internally when running as service)")
	// --service is a no-op marker passed by Windows SCM (see
	// pkg/service/windows.go InstallService) so the service-manager
	// dispatch in pkg/service/runtime_windows.go can identify itself
	// to operators reading event-log args. Cobra would otherwise error
	// on the unknown flag and the service would die at flag-parse time
	// with errno=1 ("Incorrect function") before runFunc ever runs.
	serviceFlag := false
	cmd.Flags().BoolVar(&serviceFlag, "service", false, "Internal: marker that this process was launched by Windows SCM")
	_ = cmd.Flags().MarkHidden("service")

	return cmd
}

// runPrivilegedStart handles the start command when running with elevated privileges.
// configDir is accepted for parity with runNormalStart; privileged mode installs
// the system service and reads config via the service manager rather than a flag.
func runPrivilegedStart(_ *cobra.Command, svcMgr service.ServiceManager, _ string) error {
	// Check if this is first run (service not installed yet)
	if svcMgr.IsFirstRun() {
		fmt.Println("First run detected...")

		// Create service user (Linux only, no-op on other platforms)
		fmt.Print("  Creating service user... ")
		if err := svcMgr.CreateServiceUser(); err != nil {
			fmt.Println("failed")
			return fmt.Errorf("failed to create service user: %w", err)
		}
		fmt.Println("done")

		// Set up directories
		fmt.Print("  Setting up directories... ")
		if err := svcMgr.SetupDirectories(); err != nil {
			fmt.Println("failed")
			return fmt.Errorf("failed to setup directories: %w", err)
		}
		fmt.Println("done")

		// Place the binaries where the service will run them from.
		// Before InstallService, because that reads where they landed to
		// write ExecStart.
		fmt.Print("  Placing binaries... ")
		nodePath, relocated, err := update.EnsureManagedLayout()
		switch {
		case errors.Is(err, update.ErrNotRelocatable):
			// Not fatal: the node runs perfectly from wherever it is.
			// What it loses is the ability to be updated in place by
			// the privileged updater, which `update status` reports.
			fmt.Println("skipped")
			fmt.Printf("    %v\n", err)
		case err != nil:
			fmt.Println("failed")
			return fmt.Errorf("failed to place binaries: %w", err)
		case relocated:
			fmt.Printf("done (%s)\n", nodePath)
		default:
			fmt.Println("already in place")
		}

		// Install the service
		fmt.Print("  Installing service... ")
		if err := svcMgr.InstallService(); err != nil {
			fmt.Println("failed")
			return fmt.Errorf("failed to install service: %w", err)
		}
		fmt.Println("done")

		// The service runs as its own account, so by default it reads
		// its own node.yaml rather than the one belonging to whoever
		// ran this command. Said here because the alternative is
		// finding out later, from a symptom that names neither file:
		// config edits that appear to do nothing, or a pair attempt
		// refused for a mode the operator never set.
		printServiceConfigLocation()

		// Generate API key
		fmt.Print("  Generating API key... ")
		key, err := svcMgr.GenerateAPIKey()
		if err != nil {
			fmt.Println("failed")
			return fmt.Errorf("failed to generate API key: %w", err)
		}
		fmt.Println("done")

		fmt.Printf("\nStarting zzrouter-node service...\n\n")

		// Start the service
		if err := svcMgr.StartService(); err != nil {
			return fmt.Errorf("failed to start service: %w", err)
		}

		// Display success info
		fmt.Printf("Server running at http://0.0.0.0:%d\n", constants.DefaultZZROUTERPort)
		fmt.Printf("Admin key saved to %s/zzrouter.env\n", svcMgr.GetConfigDir())
		fmt.Printf("\nAPI Key: %s\n", key)
		fmt.Println("\nTip: Use 'zzrouter-node service status' to check service status")
		printPlatformSpecificTip()

		return nil
	}

	// Service already installed - check if already running.
	status, err := svcMgr.GetStatus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not query service status (continuing): %v\n", err)
	}
	if status != nil && status.Running {
		fmt.Printf("Service already running (PID: %d)\n", status.PID)
		return nil
	}

	// Start the service
	fmt.Println("Starting zzrouter-node service...")
	if err := svcMgr.StartService(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}

	// Verify it started.
	status, err = svcMgr.GetStatus()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: service started but status query failed: %v\n", err)
		fmt.Println("Service started")
		return nil
	}
	if status != nil && status.Running {
		fmt.Printf("Server running (PID: %d)\n", status.PID)
	} else {
		fmt.Println("Service started")
	}

	return nil
}

// printServiceConfigLocation states where the service will read its
// config, and how to make that the same file the operator edits.
//
// It deliberately reports pkgConfig.Paths(), not ServiceManager's own
// GetConfigDir(): the latter names a directory the node never loads, so
// printing it sends operators to edit a file with no effect. When
// ZZROUTER_CONFIG_DIR is set both accounts resolve one root and there is
// nothing to warn about; when it is not, the service's directory belongs
// to an account this process cannot enumerate, so the remedy is named
// instead of a path that would be a guess.
func printServiceConfigLocation() {
	if shared := os.Getenv("ZZROUTER_CONFIG_DIR"); shared != "" {
		fmt.Printf("\n  Config directory: %s\n", shared)
		fmt.Println("  ZZROUTER_CONFIG_DIR is set, so the service and your CLI read")
		fmt.Println("  the same files. Verify with 'zzrouter-node status'.")
		return
	}
	fmt.Println("\n  The service runs as its own account and reads that account's")
	fmt.Println("  node.yaml, not yours, so edits to your copy will not reach it.")
	fmt.Println("  To give both one config, set ZZROUTER_CONFIG_DIR machine-wide")
	fmt.Println("  and restart the service. Check which file is live with")
	fmt.Println("  'zzrouter-node status'.")
}

// runNormalStart handles the start command when running as a normal user
func runNormalStart(cmd *cobra.Command, configDir string, svcMgr service.ServiceManager) error {
	// Check if running as the service user
	if svcMgr.IsServiceUser() {
		// Running as service user - normal service operation
		utils.Debugf("Running as service user")
	} else {
		// Running as regular user - development mode
		fmt.Printf("Running in development mode as '%s'...\n", os.Getenv("USER"))
		fmt.Println("Tip: For production, run: sudo zzrouter-node start")
		fmt.Println()
	}

	// Refuse to run as root unless the operator explicitly bypassed the check.
	if err := security.CheckNotRootWithBypass(); err != nil {
		return err
	}

	// Ensure config files exist before attempting to load
	// Skip in service user mode — config was created during privileged setup
	// and the config directory may be read-only
	if !svcMgr.IsServiceUser() {
		filesCreated, err := ensureConfigStructure(true, false)
		if err != nil {
			return fmt.Errorf("failed to ensure config files exist: %w", err)
		}
		if filesCreated {
			utils.Debugf("Initialized configuration files from templates")
		}
	}

	// Load host config when serve start command is executed
	cm := pkgConfig.NewConfigManager("zzrouter")
	hostCfg, err := cm.LoadNodeConfigFromDir(configDir)
	if err != nil {
		return fmt.Errorf("failed to load host config: %w", err)
	}

	// Check if server is already running
	if checkNodeRunning(hostCfg.Node.Port) {
		return fmt.Errorf("zzRouter server is already running on port %d. Use 'zzrouter-node stop' to stop it first", hostCfg.Node.Port)
	}

	// Validate server configuration
	if err := validateNodeConfig(hostCfg); err != nil {
		return err
	}

	detach, _ := cmd.Flags().GetBool("detach")
	if detach {
		return startDetachedNode(cmd)
	}
	return runServeStartCmd(hostCfg, cmd, nil)
}

// printPlatformSpecificTip prints platform-specific management tips
func printPlatformSpecificTip() {
	switch runtime.GOOS {
	case "linux":
		fmt.Println("Manage with: systemctl status/stop/restart zzrouter-node")
	case "darwin":
		fmt.Println("Manage with: launchctl stop/start com.zzrouter.node")
	case "windows":
		fmt.Println("Manage with: sc query zzrouter or Services app")
	}
}

// parseNodeFlags extracts server configuration flags from a cobra command.
// name is read but not currently used by callers; kept for symmetry with port.
//
//nolint:unparam // name returned for future symmetry, not a bug
func parseNodeFlags(cmd *cobra.Command) (port int, name string, err error) {
	port, _ = cmd.Flags().GetInt("port")
	name, _ = cmd.Flags().GetString("name")
	return port, name, nil
}

// getNodename returns the system hostname or a default value
func getNodename() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "zzrouter-host"
	}
	return hostname
}

func prepareNodeConfig(hostCfg *pkgConfig.NodeConfig, cmd *cobra.Command) (*pkgConfig.NodeConfig, error) {
	// Use the provided config directly (it should already be loaded from zzrouter.yaml)
	cfg := hostCfg

	// Extract command line flags
	port, _, err := parseNodeFlags(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to parse flags: %w", err)
	}

	// Apply flag overrides + defaults onto the authoritative cfg.Node
	// path. Before the Serve→Node schema collapse the --port flag
	// silently wrote to a mirror field the server never read; keep all
	// writes canonical by addressing cfg.Node directly.
	if port != 0 {
		cfg.Node.Port = port
	} else if cfg.Node.Port == 0 {
		cfg.Node.Port = constants.DefaultZZROUTERPort
	}

	if cfg.Node.Bind == "" {
		cfg.Node.Bind = "0.0.0.0" // Default bind - listen on all interfaces
	}

	// Cluster membership is driven by the pairing flow;
	// coordinator role is derived from cfg.Cluster.Mode in LoadNodeConfig.

	if cfg.Node.Name == "" {
		cfg.Node.Name = getNodename()
	}

	// Admin key resolution: node.yaml → ZZROUTER_ADMIN_API_KEY → insecure
	// dev fallback (only if the operator explicitly opted in via
	// ZZROUTER_ALLOW_DEV_KEY=1). Anything else is a hard failure so a
	// misconfigured prod box cannot silently boot with a public credential.
	//
	// Workers don't have admin keys. They serve only intra-cluster proxy
	// traffic, gated by mTLS + the OU=coordinator check on the cluster
	// listener, and never expose the management surface (/zzrouter/v1/*
	// is registered only on coordinators — see routes.go isWorker branch).
	// Requiring an admin key on workers blocks legitimate worker boots and
	// leaves a dead credential on the box; skip the requirement entirely.
	if cfg.Cluster.IsWorker() {
		cfg.Auth.AdminKey = ""
	} else if cfg.Auth.AdminKey == "" {
		if envKey := os.Getenv("ZZROUTER_ADMIN_API_KEY"); envKey != "" {
			cfg.Auth.AdminKey = envKey
		} else if os.Getenv(allowDevKeyEnv) == "1" {
			cfg.Auth.AdminKey = insecureDevAdminKey
			slog.Error("Using insecure dev admin key: DO NOT use in production",
				"env", allowDevKeyEnv, "hint", "set ZZROUTER_ADMIN_API_KEY to a real secret")
		} else {
			return nil, fmt.Errorf("no admin key configured: set ZZROUTER_ADMIN_API_KEY, or set %s=1 to use the insecure dev fallback", allowDevKeyEnv)
		}
	}

	// Return the prepared config without saving to preserve template structure
	return cfg, nil
}

// NewStopCmd creates the stop subcommand.
func NewStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the zzRouter server",
		Long:  `Stop any running zzRouter server processes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := killExistingServeProcesses()
			if err != nil {
				return fmt.Errorf("failed to stop server: %w", err)
			}

			return nil
		},
	}

	return cmd
}

// NewShowCmd creates the show subcommand.
func NewShowCmd() *cobra.Command {
	var showApps bool

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Display zzRouter configuration",
		Long: `Display zzRouter configuration including server settings, inference providers, and cluster configuration.

Examples:
  zzrouter-node show                    # Show host config
  zzrouter-node show --providers        # Show inference providers config`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showApps {
				return runNodeConfigApps()
			} else {
				return runNodeConfig()
			}
		},
	}

	cmd.Flags().BoolVar(&showApps, "providers", false, "Display provider configuration")

	return cmd
}

// runNodeConfig displays host configuration
func runNodeConfig() error {
	cm := pkgConfig.NewConfigManager("zzrouter")

	fmt.Println("🏠 Node Configuration")
	fmt.Println("--------------------")
	configFile := cm.GetNodeConfigPath()
	fmt.Printf("Config File: %s\n", configFile)

	// Check if .env files exist
	envLocations := pkgConfig.GetDotEnvLocations()
	fmt.Println("\n📋 Environment Files:")
	for _, loc := range envLocations {
		if _, err := os.Stat(loc); err == nil {
			fmt.Printf("  %s (exists)\n", loc)
		} else {
			fmt.Printf("  ⚪ %s (not found)\n", loc)
		}
	}

	// Read and display the raw config file content
	content, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to read host config file: %w", err)
	}

	fmt.Println("\n--- Configuration Content ---")
	fmt.Println(string(content))
	fmt.Println("-----------------------------")

	return nil
}

// runNodeConfigEdit opens the host config in an editor
func runNodeConfigEdit() error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	configFile := cm.GetNodeConfigPath()

	fmt.Printf("Editing host configuration...\n")
	fmt.Printf("File: %s\n", configFile)
	fmt.Printf("Editor: %s\n\n", pkgConfig.GetPreferredEditor())

	// Show .env file locations for reference
	envLocations := pkgConfig.GetDotEnvLocations()
	fmt.Println("💡 Tip: For API keys, you can also use .env files:")
	for _, loc := range envLocations {
		if _, err := os.Stat(loc); err == nil {
			fmt.Printf("   %s (exists)\n", loc)
		} else {
			fmt.Printf("   ⚪ %s (create if needed)\n", loc)
		}
	}
	fmt.Println()

	return pkgConfig.OpenInEditorWithFallback(configFile)
}

// runNodeConfigApps displays the inference providers configuration.
// The on-disk shape is a directory of per-provider yaml files, so this
// walks the tree and prints each file with a header.
func runNodeConfigApps() error {
	cm := pkgConfig.NewConfigManager("zzrouter")

	fmt.Println("Inference Providers Configuration")
	fmt.Println("------------------------------------")
	dir := cm.GetAppsConfigDir()
	fmt.Printf("Config Dir: %s\n", dir)

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Println("\nProvider configuration directory not found.")
		fmt.Println("💡 Run 'zzrouter host init' to create a default configuration.")
		return nil //nolint:nilerr // absence of config directory is a user-facing hint, not a CLI-error
	}

	walkErr := filepath.Walk(dir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".yaml") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		content, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", p, err)
		}
		fmt.Printf("\n--- %s ---\n", rel)
		fmt.Println(string(content))
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	fmt.Println("-----------------------------")
	return nil
}

// validateNodeConfig checks if the server configuration is valid and complete
func validateNodeConfig(cfg *pkgConfig.NodeConfig) error {
	// Workers have no admin/user keys — they auth via cluster network key
	// only. Skip the auth-key sanity check on worker mode.
	if !cfg.Cluster.IsWorker() {
		// Check for old corrupted structure indicators.
		// Old corrupted config had: admin:, 'cluster ': (with space), missing client section.
		if cfg.Auth.AdminKey == "" && cfg.Auth.UserKey == "" {
			return fmt.Errorf("server configuration has old structure (missing client authentication)")
		}
	}

	// Check for essential server fields
	if cfg.Node.Port == 0 {
		return fmt.Errorf("server configuration is missing port")
	}

	if cfg.Node.Bind == "" {
		return fmt.Errorf("server configuration is missing host")
	}

	// node.name is resolved at config load (NodeConfig.applyLoadDefaults),
	// so an empty one here means the hostname was unavailable too.
	if cfg.Node.Name == "" {
		return fmt.Errorf("server configuration is not initialized (server name is empty)\n\nPlease initialize your server configuration:\n  zzrouter-node config init\n\nThis will set up your server with proper name and settings")
	}

	return nil
}

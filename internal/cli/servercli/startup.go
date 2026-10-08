package servercli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/internal/server"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// checkClusterNodeConnectivity checks the connection status of cluster hosts
func checkClusterNodeConnectivity(endpoints []string) map[string]string {
	statuses := make(map[string]string)

	if len(endpoints) == 0 {
		return statuses
	}

	for _, endpoint := range endpoints {
		if CheckRemoteNode(endpoint, "/health", constants.ClusterHealthCheckTimeout) {
			statuses[endpoint] = "Connected"
		} else {
			statuses[endpoint] = "Failed"
		}
	}

	return statuses
}

// displayAppsSync displays apps synchronously before server starts
func displayAppsSync(cfg *pkgConfig.NodeConfig) {
	// Collect apps from all hosts
	allApps := make(map[string][]string)
	client := &http.Client{Timeout: constants.ClusterHealthCheckTimeout}

	// 1. Get local apps (master/localhost) - try once since server isn't running yet
	localApps := getAppsFromNode(client, "localhost", cfg.Node.Port)
	if len(localApps) > 0 {
		allApps[fmt.Sprintf("localhost:%d", cfg.Node.Port)] = localApps
	}

	// 2. Get apps from each cluster host
	for _, clusterEndpoint := range cfg.Cluster.Endpoints.Addresses() {
		// Parse cluster endpoint to get host and port
		host, port := parseNodePort(clusterEndpoint, cfg.Node.Port)
		clusterApps := getAppsFromNode(client, host, port)
		if len(clusterApps) > 0 {
			allApps[fmt.Sprintf("%s:%d", host, port)] = clusterApps
		}
	}

	// Display results if we found any apps
	if len(allApps) > 0 {
		fmt.Printf("\nProvider(s) available:\n")

		// Display master host first
		masterKey := fmt.Sprintf("localhost:%d", cfg.Node.Port)
		if cfg.Cluster.IsCoordinator() {
			if apps, exists := allApps[masterKey]; exists {
				fmt.Printf("Master - %s: %s\n", masterKey, strings.Join(apps, ", "))
				delete(allApps, masterKey)
			}
		}

		// Display cluster hosts
		for host, apps := range allApps {
			fmt.Printf("%s: %s\n", host, strings.Join(apps, ", "))
		}
	}
}

// getAppsFromNode fetches apps from a specific host
func getAppsFromNode(client *http.Client, host string, port int) []string {
	url := fmt.Sprintf("http://%s:%d/zzrouter/v1/providers", host, port)
	resp, err := client.Get(url)
	if err != nil {
		utils.Debugf("Failed to get apps from %s: %v", url, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		utils.Debugf("Got status %d from %s", resp.StatusCode, url)
		return nil
	}

	var result struct {
		Apps []struct {
			Type string `json:"type"`
			Node string `json:"node"`
		} `json:"providers"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		utils.Debugf("Failed to decode apps from %s: %v", url, err)
		return nil
	}

	utils.Debugf("Got %d apps from %s", len(result.Apps), url)
	for i, p := range result.Apps {
		utils.Debugf("Provider %d from %s: Type=%s, Node=%s", i, url, p.Type, p.Node)
	}

	// Extract unique provider types
	providerTypes := make(map[string]bool)
	for _, provider := range result.Apps {
		if provider.Type != "" {
			providerTypes[provider.Type] = true
		}
	}

	// Convert to slice
	var apps []string
	for providerType := range providerTypes {
		apps = append(apps, providerType)
	}

	return apps
}

// parseNodePort parses a host-or-host:port endpoint, falling back to
// defaultPort when no port is present. IPv6 literals (`[::1]:9090`) are
// handled via net.SplitHostPort.
func parseNodePort(endpoint string, defaultPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		// No port component (or malformed) — treat whole string as host.
		return endpoint, defaultPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defaultPort
	}
	return host, port
}

// runServe starts the server and blocks until it stops or a signal
// arrives. SIGINT/SIGTERM trigger a graceful Stop; a second signal forces
// immediate exit to avoid hanging on slow drains.
//
// Shutdown ordering: srv.Stop() is the authoritative teardown path — it
// pre-drains spend state, shuts down HTTP with its own timeout ctx,
// cancels shutdownCtx (which Start rooted as a child of the caller's
// ctx), then stops subsystems. We always call it, even on the self-exit
// path, so a server that exits on its own (e.g. listener error) still
// flushes spend and tears down subsystems instead of leaking goroutines.
func runServe(hostCfg *pkgConfig.NodeConfig) error {
	utils.LogInfof("Starting zzRouter server v%s...", version.Current.String())

	// Count this boot against any update awaiting proof, before building
	// anything that could fail. When the new binary has used up its
	// attempts the previous one is restored here, and exiting hands the
	// process back to the supervisor to start it.
	// ExhaustedAttempts rolls back and reports whether the process has to
	// exit for the restored binary to run; it declines to exit when
	// nothing would start the node again.
	guard := newUpdateGuard(hostCfg)
	if guard.ExhaustedAttempts() {
		return errRestartForUpdate
	}

	srv, err := server.NewServerWithOptions(hostCfg)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Start(context.Background()) }()

	// Watch for the node answering its own health endpoint, which is
	// what clears a pending update. A node that never answers rolls the
	// update back and asks for the exit that starts the old binary.
	confirmDone := make(chan struct{})
	defer close(confirmDone)
	if guard != nil {
		go func() {
			if guard.ConfirmWhenHealthy(confirmDone) {
				update.RequestExit("an update was rolled back after it failed to serve")
			}
		}()
	}

	var (
		runErr     error
		srvDrained bool
		forced     chan struct{}
		forUpdate  bool
	)

	select {
	case runErr = <-serveErr:
		// Server exited on its own (startup failure or self-initiated).
		srvDrained = true
	case <-update.RestartRequested():
		// The binary on disk changed under us, either because an update
		// installed or because one was rolled back. Drain normally, then
		// exit with a failure status so the service manager starts the
		// node again on whatever is now on disk.
		forUpdate = true
		fmt.Printf("\nShutting down so the service manager restarts the node: %s\n", update.RestartReason())
	case sig := <-sigCh:
		// Spawn the second-signal watcher BEFORE the user-visible printf,
		// so a fast double-Ctrl-C can't slip into the sigCh buffer during
		// the printf window and fire "Forced shutdown" after we've
		// already cleanly Stopped.
		forced = make(chan struct{})
		go func() {
			select {
			case <-sigCh:
				fmt.Println("\nForced shutdown (second signal received)")
				os.Exit(1)
			case <-forced:
			}
		}()
		fmt.Printf("\nReceived signal %v, shutting down server...\n", sig)
	}

	if err := srv.Stop(); err != nil {
		fmt.Printf("Error stopping server: %v\n", err)
		if runErr == nil {
			runErr = err
		}
	}
	// Release the watcher IMMEDIATELY after Stop returns, so a second
	// signal arriving after a clean shutdown doesn't force-exit us.
	if forced != nil {
		close(forced)
	}
	if !srvDrained {
		// Wait for Start() to return so deferred cleanup and final logs flush.
		if err := <-serveErr; err != nil && runErr == nil {
			runErr = err
		}
	}
	if forUpdate && runErr == nil {
		return errRestartForUpdate
	}
	return runErr
}

// errRestartForUpdate leaves the process with a non-zero exit status
// after an update-initiated shutdown, whether the binary on disk was
// just installed or just rolled back. That status is what every
// supported supervisor keys its restart on: systemd Restart=on-failure,
// launchd KeepAlive, and the SCM recovery actions. A clean exit would
// leave the node down on a binary nothing is running.
var errRestartForUpdate = errors.New("exiting so the service manager restarts the node on the binary now installed")

// runServeStartCmd handles the serve start logic
func runServeStartCmd(hostCfg *pkgConfig.NodeConfig, cmd *cobra.Command, _ []string) error {
	// CENTRALIZED CHECK: Prevent starting if ANY server instance is already running
	// This check applies to BOTH detached and interactive modes
	// No multiple instances allowed, regardless of port
	if err := checkNodeNotRunning(); err != nil {
		return err
	}

	// Load .env files before reading any environment variables
	if pkgConfig.ShouldLoadDotEnv() {
		if err := pkgConfig.AutoLoadDotEnv(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load .env files: %v\n", err)
		}
	}

	// Initialize debug mode from environment first (if set)
	utils.InitDebugFromEnv()

	// Get debug flag and override environment if explicitly set
	debug, _ := cmd.Flags().GetBool("debug")
	if debug {
		utils.SetDebugMode(true)
	}

	// Get detach flag
	detach, _ := cmd.Flags().GetBool("detach")

	// In detached mode, suppress debug output regardless of flag
	if detach {
		utils.SetDebugMode(false)
	}

	// Display config directory at startup (show actual directory being used) - only in detached mode
	configDir, _ := cmd.Flags().GetString("dir")
	if configDir == "" {
		// Check if environment variable was used
		if envDir := os.Getenv("ZZROUTER_CONFIG_DIR"); envDir != "" {
			configDir = envDir
		} else {
			// Default discovery was used
			cm := pkgConfig.NewConfigManager("zzrouter")
			configDir = cm.GetNodeConfigDir()
		}
	}

	// Only show command-line output in detached mode
	if detach {
		fmt.Printf("Config directory: %s\n", configDir)
	}

	// Load and prepare configuration
	cfg, err := prepareNodeConfig(hostCfg, cmd)
	if err != nil {
		return fmt.Errorf("failed to prepare server config: %w", err)
	}

	// Auto-seed the embedded provider tree before loading. `InstallDefaults`
	// is copy-if-missing per file, so on nodes that have already been
	// initialized (via `zzrouter-node config init` or a systemd install)
	// this is a no-op. Nodes launched directly — fresh deployments, the
	// `make bw` / SCP-a-binary-and-start-it path — get their providers/
	// tree bootstrapped automatically here, so the config store loads and
	// FinalizeOnboarding can persist `enabled: true` after an install.
	//
	// Previously this seeding ran only on explicit init or systemd service
	// install, so a bare-deployed worker booted with `s.configStore = nil`;
	// provider installs succeeded on disk but the cluster never learned
	// they were enabled ("install succeeded but finalize failed: config
	// store not initialized").
	appsDir := pkgConfig.Paths().GetAppsConfigDir()
	if seedErr := templates.InstallDefaults(appsDir); seedErr != nil {
		slog.Warn("Failed to seed provider templates", "dir", appsDir, "err", seedErr)
	}

	// Seeding is copy-if-missing; reconcile is what brings an existing tree
	// up to the release (managed keys, schemas, shipped assets).
	if updated, reconcileErr := templates.ReconcileManagedSpine(appsDir); reconcileErr != nil {
		slog.Warn("Failed to reconcile provider templates", "dir", appsDir, "err", reconcileErr)
	} else if len(updated) > 0 {
		slog.Info("Reconciled shipped provider files", "files", updated)
	}

	// Load apps configuration from XDG-compliant location
	appsConfig, appsConfigPath, err := pkgConfig.LoadAppsConfigFromStandardLocations()
	if err != nil {
		// If provider config doesn't exist, continue with default behavior
		if detach {
			fmt.Println(" No provider config found")
			fmt.Printf("   Error: %v\n", err)
			fmt.Println("   Using auto-discovery mode for providers.")
			fmt.Println("   Run 'zzrouter-node config init' to create provider configuration.")
			fmt.Println()
		}
	} else {
		// Count and display enabled apps
		enabledCount := 0
		appsConfig.RangeApps(func(_ string, provider pkgConfig.ServiceConfig) bool {
			if provider.IsEnabled() {
				enabledCount++
			}
			return true
		})

		// Warn if no apps are enabled (node can still start for cluster join)
		if enabledCount == 0 {
			slog.Warn("No providers are enabled: node will start but cannot serve inference until a provider is installed",
				"config", appsConfigPath)
		}
	}

	// Display cluster configuration - only in detached mode
	if detach && len(cfg.Cluster.Endpoints) > 0 {
		if cfg.Cluster.IsCoordinator() {
			fmt.Printf("Coordinator node connecting to cluster hosts:\n")
		} else {
			fmt.Printf("Connecting to cluster hosts:\n")
		}

		// Check connectivity for each host
		addrs := cfg.Cluster.Endpoints.Addresses()
		statuses := checkClusterNodeConnectivity(addrs)
		for _, endpoint := range addrs {
			status := statuses[endpoint]
			fmt.Printf("  %s %s\n", endpoint, status)
		}
	} else if detach && cfg.Cluster.IsCoordinator() {
		fmt.Printf("Coordinator node mode (no cluster hosts configured)\n")
	}

	// Display consolidated apps from master and cluster hosts (only in detached mode)
	if detach && (len(cfg.Cluster.Endpoints) > 0 || cfg.Cluster.IsCoordinator()) {
		displayAppsSync(cfg)
	}

	// Handle process cleanup (only in non-detached mode). Failures here are
	// non-fatal — start will still try to bind and report a clearer error if
	// the port is held — but we surface them so a stuck process is visible.
	if !detach {
		if err := cleanupExistingProcesses(); err != nil {
			slog.Warn("Pre-start process cleanup reported an error (continuing)", "error", err)
		}
	}

	// Detached mode spawns a child and returns.
	if detach {
		return handleDetachedMode(cmd, cfg)
	}

	// providers/<kind>/<name>/config.yaml is the canonical source of
	// truth. The legacy --config=false branch used to
	// auto-discover providers from a hardcoded binary list and ignore
	// providers/, which silently dropped any config-driven provider
	// (mlx, openrouter, huggingface, ollama-cloud) from the runtime.
	// The flag still exists as inert for backward compat; the
	// providers/ tree is always consulted now.
	return runServe(cfg)
}

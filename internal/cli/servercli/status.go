package servercli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// NewStatusCmd creates the status subcommand.
func NewStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check zzRouter server status",
		Long: `Check the status of the zzRouter server including:
- Node address and port
- Cluster mode
- Discovered providers and their versions
- Worker connectivity (if master)

Use this command to check the current operational state of the zzRouter server.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodeStatus()
		},
	}

	return cmd
}

// AppStatus represents an app's status for display
type AppStatus struct {
	Name    string
	Status  string
	Version string
}

func runNodeStatus() error {
	// Load configuration
	cm := config.NewConfigManager("zzrouter")
	hostCfg, err := cm.LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load host config: %w", err)
	}

	// Get the port from Node config (primary) or Serve config (legacy)
	port := hostCfg.Node.Port
	if port == 0 {
		port = hostCfg.Node.Port
	}
	if port == 0 {
		port = constants.DefaultZZROUTERPort
	}

	// Check if server is running
	serverRunning := checkNodeRunning(port)

	if !serverRunning {
		fmt.Println("Node:   not running")
		fmt.Println()
		fmt.Println("To start: zzrouter-node start")
		return nil
	}

	// Get bind address
	bindAddr := hostCfg.Node.Bind
	if bindAddr == "" {
		bindAddr = "0.0.0.0"
	}

	// Get cluster mode
	clusterMode := string(hostCfg.Cluster.Mode)
	if clusterMode == "" {
		clusterMode = string(config.ClusterModeDisabled)
	}

	// Print status in clean key-value format (values aligned at column 11)
	fmt.Printf("Node:   running on %s:%d\n", bindAddr, port)
	fmt.Printf("Mode:   %s\n", clusterMode)
	fmt.Printf("Config: %s\n", hostCfg.SourcePath)

	adminKey, _ := hostCfg.Auth.GetAdminAPIKey()

	// Say so loudly when the running server read a different file.
	reportServerIdentity(port, hostCfg)

	// Get and display apps (pass admin key for auth)
	apps := getAppsWithStatus(port, adminKey)
	if len(apps) > 0 {
		fmt.Println()
		fmt.Println("Providers:")
		displayAppsTable(apps)
	}

	// Show workers if master mode
	if hostCfg.Cluster.IsMaster() && len(hostCfg.Cluster.Endpoints) > 0 {
		fmt.Println()
		fmt.Println("Workers:")
		displayWorkersStatus(hostCfg.Cluster.Endpoints.Addresses())
	}

	fmt.Println()
	return nil
}

// checkNodeRunning checks if the zzRouter server is running on the specified port
func checkNodeRunning(port int) bool {
	result := CheckLocalNode(port, "/health/ready", constants.ClusterHealthCheckTimeout)
	return result.Reachable
}

// getAppsWithStatus fetches apps from the server API
func getAppsWithStatus(port int, adminKey string) []AppStatus {
	client := &http.Client{Timeout: constants.ClusterHealthCheckTimeout}

	// Try localhost first
	apps := fetchAppsFromAPI(client, "localhost", port, adminKey)
	if len(apps) == 0 {
		// Try 127.0.0.1 as fallback
		apps = fetchAppsFromAPI(client, "127.0.0.1", port, adminKey)
	}

	return apps
}

// fetchAppsFromAPI fetches apps from the server API and extracts status/version
func fetchAppsFromAPI(client *http.Client, host string, port int, adminKey string) []AppStatus {
	url := fmt.Sprintf("http://%s:%d/zzrouter/v1/providers", host, port)
	// TODO: Thread context from callers for proper cancellation support
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Data []struct {
			Name     string         `json:"name"`
			Type     string         `json:"type"`
			Status   string         `json:"status"`
			Metadata map[string]any `json:"metadata"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var apps []AppStatus
	for _, app := range result.Data {
		status := app.Status
		if status == "" {
			status = "available"
		}

		// Try to get version from metadata
		version := ""
		if app.Metadata != nil {
			if v, ok := app.Metadata["version"].(string); ok {
				version = v
			}
		}

		// Use type as name if name is empty
		name := app.Name
		if name == "" {
			name = app.Type
		}

		apps = append(apps, AppStatus{
			Name:    name,
			Status:  status,
			Version: version,
		})
	}

	return apps
}

// displayAppsTable displays apps in a clean table format
func displayAppsTable(apps []AppStatus) {
	// Calculate column widths
	maxNameWidth := 4    // "NAME"
	maxStatusWidth := 6  // "STATUS"
	maxVersionWidth := 7 // "VERSION"

	for _, app := range apps {
		if len(app.Name) > maxNameWidth {
			maxNameWidth = len(app.Name)
		}
		if len(app.Status) > maxStatusWidth {
			maxStatusWidth = len(app.Status)
		}
		if len(app.Version) > maxVersionWidth {
			maxVersionWidth = len(app.Version)
		}
	}

	// Print header
	fmt.Printf("  %-*s  %-*s  %-*s\n",
		maxNameWidth, "NAME",
		maxStatusWidth, "STATUS",
		maxVersionWidth, "VERSION")

	// Print separator
	fmt.Printf("  %s  %s  %s\n",
		strings.Repeat("-", maxNameWidth),
		strings.Repeat("-", maxStatusWidth),
		strings.Repeat("-", maxVersionWidth))

	// Print data rows
	for _, app := range apps {
		version := app.Version
		if version == "" {
			version = "-"
		}
		fmt.Printf("  %-*s  %-*s  %-*s\n",
			maxNameWidth, app.Name,
			maxStatusWidth, app.Status,
			maxVersionWidth, version)
	}
}

// displayWorkersStatus displays worker connectivity status
func displayWorkersStatus(endpoints []string) {
	statuses := checkClusterNodeConnectivity(endpoints)

	for _, endpoint := range endpoints {
		status := statuses[endpoint]
		if status == "OK" {
			fmt.Printf("  %s  connected\n", endpoint)
		} else {
			fmt.Printf("  %s  disconnected\n", endpoint)
		}
	}
}

// serverIdentity is the running server's own answer to "what am I and
// which file did I read?".
type serverIdentity struct {
	Name        string
	ClusterMode string
	ConfigPath  string
}

// reportServerIdentity prints what the RUNNING server says about itself
// whenever that differs from what this CLI just loaded.
//
// The two can differ: a server installed as a service runs under another
// account and reads that account's node.yaml, while the CLI reads the
// operator's. Both are valid configs, they simply are not the same one,
// and every symptom of the split shows up somewhere else entirely (a
// pair attempt refused as "this node is a coordinator", a stop that
// undoes itself). Printing the server's own view next to the CLI's is
// what turns that into a one-line diagnosis.
func reportServerIdentity(port int, local *config.NodeConfig) {
	remote, err := fetchNodeIdentity(port)
	if err != nil {
		fmt.Printf("\nServer identity unavailable (%v).\n", err)
		fmt.Println("The server is running but did not answer for itself, so this")
		fmt.Println("CLI cannot confirm it loaded the config shown above.")
		return
	}

	lines := identityMismatch(local, remote)
	if len(lines) == 0 {
		return
	}
	fmt.Println()
	for _, line := range lines {
		fmt.Println(line)
	}
}

// identityMismatch returns the operator-facing report when the running
// server disagrees with the config this CLI loaded, or nil when the two
// agree. A field the server left empty is not a disagreement: an older
// build does not report its config path.
func identityMismatch(local *config.NodeConfig, remote *serverIdentity) []string {
	sameConfig := remote.ConfigPath == "" || remote.ConfigPath == local.SourcePath
	sameMode := remote.ClusterMode == "" || remote.ClusterMode == clusterModeOrDisabled(local)
	if sameConfig && sameMode {
		return nil
	}
	lines := []string{"WARNING: the running server is not using the config shown above."}
	if remote.ConfigPath != "" {
		lines = append(lines, fmt.Sprintf("  server config: %s", remote.ConfigPath))
	}
	lines = append(lines,
		fmt.Sprintf("  server mode:   %s (this CLI: %s)", remote.ClusterMode, clusterModeOrDisabled(local)),
		fmt.Sprintf("  server name:   %s (this CLI: %s)", remote.Name, local.Node.Name),
		"Commands run here configure a file the server never reads.",
		"Edit the server's config, or stop the service and start the node",
		"under this account.",
	)
	return lines
}

// clusterModeOrDisabled renders an unset mode the way the server does.
func clusterModeOrDisabled(cfg *config.NodeConfig) string {
	if cfg.Cluster.Mode == "" {
		return string(config.ClusterModeDisabled)
	}
	return string(cfg.Cluster.Mode)
}

// fetchNodeIdentity asks the local server who it thinks it is.
//
// It asks /health rather than /zzrouter/v1/server/identity because a worker mounts
// no /zzrouter/v1/* at all, and a worker is where the split config
// actually happens. The config path is admin-gated and so absent here;
// /server/identity carries it where that route exists.
func fetchNodeIdentity(port int) (*serverIdentity, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/health?detailed=true", port)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := (&http.Client{Timeout: constants.ClusterHealthCheckTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var body struct {
		Details struct {
			NodeIdentity struct {
				Name        string `json:"node_name"`
				ClusterMode string `json:"cluster_mode"`
				ConfigPath  string `json:"config_path"`
			} `json:"node_identity"`
		} `json:"details"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	id := body.Details.NodeIdentity
	return &serverIdentity{Name: id.Name, ClusterMode: id.ClusterMode, ConfigPath: id.ConfigPath}, nil
}

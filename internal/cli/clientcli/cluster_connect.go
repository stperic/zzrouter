package clientcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/spf13/cobra"
	"github.com/stperic/zzrouter/pkg/apipath"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/version"
)

// =============================================================================
// CONNECT COMMAND
// =============================================================================

// NewConnectCmd creates the connect command for connecting to a zzRouter cluster
func NewConnectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect [url]",
		Short: "Connect to a zzRouter cluster",
		Long: `Connect to a zzRouter cluster for model management.

Without arguments, discovers available clusters via:
1. Process detection (finds running zzRouter servers automatically)
2. Localhost port scanning (ports 9090, 8080, 3000, 8000, 11434)
3. mDNS network discovery
4. Interactive prompt if none found

With a URL, connects to that specific cluster.

The connection is saved to your client configuration for future use.

Examples:
  zzrouter connect                        # Auto-discover and connect
  zzrouter connect http://192.0.2.10:9090  # Connect to specific cluster
  zzrouter connect 192.0.2.10:9090         # Connect (http:// assumed)
  zzrouter connect localhost              # Connect to local cluster`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cm := pkgConfig.NewConfigManager("zzrouter")

			if len(args) > 0 {
				return connectToCluster(args[0], cm)
			}

			return discoverAndConnect(cm)
		},
	}

	return cmd
}

// =============================================================================
// DISCONNECT COMMAND
// =============================================================================

// NewDisconnectCmd creates the disconnect command for disconnecting from cluster
func NewDisconnectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disconnect",
		Short: "Disconnect from zzRouter cluster",
		Long: `Disconnect from the current zzRouter cluster by clearing the saved connection.

After disconnecting, you'll need to use 'zzrouter connect' to connect to a cluster again.

Examples:
  zzrouter disconnect`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cm := pkgConfig.NewConfigManager("zzrouter")
			return runDisconnect(cm)
		},
	}

	return cmd
}

func runDisconnect(cm *pkgConfig.ConfigManager) error {
	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		// No config — nothing to disconnect
		fmt.Println("Not connected to any cluster.")
		fmt.Println()
		return nil //nolint:nilerr // absence of config is a soft "already disconnected" state
	}

	cfg := store.Config()
	if cfg.Node.Address == "" {
		fmt.Println("Not connected to any cluster.")
		fmt.Println()
		return nil
	}

	currentCluster := fmt.Sprintf("%s:%d", cfg.Node.Address, cfg.Node.Port)

	if err := store.Disconnect(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Disconnected from %s\n", currentCluster)
	fmt.Println()

	return nil
}

// =============================================================================
// CONNECTION HELPERS
// =============================================================================

func discoverAndConnect(cm *pkgConfig.ConfigManager) error {
	fmt.Printf("%s Discovering zzRouter clusters...\n", ui.GetSearchEmoji())
	fmt.Println()

	clientConfig, err := cm.LoadClientConfig()
	if err != nil {
		clientConfig = &pkgConfig.ClientConfig{}
	}

	if clientConfig.Node.Address != "" {
		fmt.Printf("Already connected to %s:%d\n", clientConfig.Node.Address, clientConfig.Node.Port)
		fmt.Println()
		fmt.Printf("%s To connect to a different cluster:\n", ui.GetInfoEmoji())
		fmt.Println("   zzrouter disconnect")
		fmt.Println("   zzrouter connect <url>")
		return nil
	}

	processURL := checkForRunningServer()
	if processURL != "" {
		if confirmYN("\nConnect to this cluster? (y/N): ") {
			return connectToCluster(processURL, cm)
		}
		if !confirmYN("Discover other clusters on the network? (y/N): ") {
			fmt.Println("Connection cancelled.")
			return nil
		}
	}

	fmt.Println("\nChecking localhost common ports...")
	localhostURL := checkLocalPorts()
	if localhostURL != "" {
		fmt.Printf("%s Found cluster on %s\n", ui.GetCheckEmoji(), localhostURL)
		if confirmYN("\nConnect to localhost? (y/N): ") {
			return connectToCluster(localhostURL, cm)
		}
		if !confirmYN("Discover other clusters on the network? (y/N): ") {
			fmt.Println("Connection cancelled.")
			return nil
		}
	}

	fmt.Println("\nScanning network via mDNS...")
	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()
	hostDiscovery := network.NewNodeDiscovery("", false, 0) // Discovery only, not registering
	discoveredNodes, err := hostDiscovery.DiscoverNodesWithContext(ctx)

	if err != nil {
		fmt.Printf("%s mDNS discovery failed: %v\n", ui.GetWarningEmoji(), err)
	} else if len(discoveredNodes) > 0 {
		fmt.Printf("%s Found %d cluster(s) via mDNS\n\n", ui.GetCheckEmoji(), len(discoveredNodes))
		displayDiscoveredClusters(discoveredNodes)

		recommended := discoveredNodes[0]
		prompt := fmt.Sprintf("\nConnect to %s:%d? (y/N): ", recommended.Node, recommended.Port)
		if confirmYN(prompt) {
			hostURL := fmt.Sprintf("http://%s:%d", recommended.Node, recommended.Port)
			return connectToCluster(hostURL, cm)
		}
	}

	fmt.Println()
	fmt.Printf("%s No zzRouter clusters found\n", ui.GetErrorEmoji())
	fmt.Println()
	fmt.Printf("%s Options:\n", ui.GetInfoEmoji())
	fmt.Println("   1. Start a local server: zzrouter-node start")
	fmt.Println("   2. Connect manually: zzrouter connect <url>")

	return nil
}

func checkForRunningServer() string {
	fmt.Println("Checking for running zzRouter servers...")

	// "zzrouter" would match our own running process (the connect CLI);
	// only search for the node binary. "zzrouter host" is a retired
	// subcommand — drop.
	processNames := []string{"zzrouter-node"}

	for _, processName := range processNames {
		pid, err := findProcess(processName)
		if err != nil {
			continue
		}

		proc, err := process.NewProcess(int32(pid))
		if err != nil {
			continue
		}

		cmdline, err := proc.Cmdline()
		if err != nil {
			continue
		}

		port := extractPort(cmdline)
		if port > 0 {
			if checkHealth(port) {
				fmt.Printf("%s Found zzRouter server on localhost:%d\n", ui.GetCheckEmoji(), port)
				return fmt.Sprintf("http://localhost:%d", port)
			}
		}
	}

	return ""
}

func findProcess(processName string) (int, error) {
	self := os.Getpid()

	ctx, cancel := context.WithTimeout(context.Background(), constants.ClusterHealthCheckTimeout)
	defer cancel()

	cmd := host.CommandContext(ctx, "pgrep", "-f", processName)
	output, err := cmd.Output()
	if err == nil && len(output) > 0 {
		// pgrep returns one PID per line; walk lines to skip our own PID.
		for _, line := range strings.Split(string(output), "\n") {
			pidStr := strings.TrimSpace(line)
			if pidStr == "" {
				continue
			}
			pid, atoiErr := strconv.Atoi(pidStr)
			if atoiErr != nil {
				continue
			}
			if pid == self {
				continue
			}
			return pid, nil
		}
	}

	procs, err := process.Processes()
	if err != nil {
		return 0, err
	}

	for _, proc := range procs {
		if int(proc.Pid) == self {
			continue
		}
		name, err := proc.Name()
		if err != nil {
			continue
		}

		if strings.Contains(name, processName) {
			return int(proc.Pid), nil
		}

		cmdline, err := proc.Cmdline()
		if err != nil {
			continue
		}

		if strings.Contains(cmdline, processName) {
			return int(proc.Pid), nil
		}
	}

	return 0, fmt.Errorf("process not found")
}

// portPatterns is the ordered list of regexes extractPort tries against a
// cmdline. Compiled once at package init so checkForRunningServer doesn't
// recompile ten regexes on every pgrep hit.
var portPatterns = []*regexp.Regexp{
	regexp.MustCompile(`--port[=\s](\d+)`),
	regexp.MustCompile(`-port\s+(\d+)`),
	regexp.MustCompile(`-p\s+(\d+)`),
	regexp.MustCompile(`port\s+(\d+)`),
	regexp.MustCompile(`:(\d+)(?:\s|$)`),
	regexp.MustCompile(`localhost:(\d+)`),
	regexp.MustCompile(`0\.0\.0\.0:(\d+)`),
	regexp.MustCompile(`127\.0\.0\.1:(\d+)`),
	regexp.MustCompile(`bind.*:(\d+)`),
	regexp.MustCompile(`listen.*:(\d+)`),
}

func extractPort(cmdline string) int {
	for _, re := range portPatterns {
		matches := re.FindStringSubmatch(cmdline)
		if len(matches) > 1 {
			if port, err := strconv.Atoi(matches[1]); err == nil && port > 0 && port < 65536 {
				return port
			}
		}
	}

	if strings.Contains(cmdline, "zzrouter-node") {
		return constants.DefaultZZROUTERPort
	}

	return 0
}

func checkHealth(port int) bool {
	client := &http.Client{Timeout: constants.ClusterHealthCheckTimeout}
	healthURL := fmt.Sprintf("http://localhost:%d/health", port)

	ctx, cancel := context.WithTimeout(context.Background(), constants.ClusterHealthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusOK
}

// checkLocalPorts probes common localhost ports looking for a zzRouter
// node. It hits /zzrouter/v1/health and requires a response body that
// advertises service_type = zzrouter-host/zzrouter-node — a bare 200
// from some other service listening on the same port (e.g. Ollama on
// 11434) is not accepted as a zzRouter match.
func checkLocalPorts() string {
	commonPorts := []int{constants.DefaultZZROUTERPort, 8080, 3000, 8000, constants.DefaultOllamaPort}
	client := &http.Client{Timeout: constants.ClusterHealthCheckTimeout}

	for _, port := range commonPorts {
		if !isZZRouterOnPort(client, port) {
			continue
		}
		return fmt.Sprintf("http://localhost:%d", port)
	}

	return ""
}

// isZZRouterOnPort returns true iff the given localhost port serves a
// /health response with service_type set to a zzRouter value. Probes
// the UNAUTHENTICATED top-level /health (not /zzrouter/v1/health which
// is admin-gated) because the caller has no key yet — auto-discovery
// happens before any credentials are configured. /health returns
// service_type in its JSON body (internal/server/health_handlers.go:34,49)
// so we can reject co-located services that happen to answer 200.
func isZZRouterOnPort(client *http.Client, port int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), constants.ClusterHealthCheckTimeout)
	defer cancel()

	url := fmt.Sprintf("http://localhost:%d/health", port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	var body struct {
		ServiceType string `json:"service_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	// Server emits ServiceTypeHost regardless of coordinator/worker
	// mode today. ServiceTypeNode is accepted defensively in case a
	// future worker-side handler distinguishes itself.
	return body.ServiceType == version.ServiceTypeHost || body.ServiceType == version.ServiceTypeNode
}

// connectHTTPClient returns a bounded HTTP client for probe/connect
// requests. Using http.DefaultClient here would leak connections and
// have no timeout beyond the request ctx — fine for the probe's short
// deadline but gives server-side half-open responses room to hang.
func connectHTTPClient() *http.Client {
	return &http.Client{Timeout: constants.HTTPShortTimeout}
}

// probeCluster hits /zzrouter/v1/server/version at clusterURL with the given
// apiKey (empty string = unauthenticated probe). Returns the HTTP
// status and any transport error. Caller interprets the status.
func probeCluster(ctx context.Context, clusterURL, apiKey string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", clusterURL+apipath.ServerVersion, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	if apiKey != "" {
		// Single canonical auth header. The server accepts X-API-Key;
		// we stopped sending Authorization: Bearer too so the wire
		// format is unambiguous and rotating the header format later
		// is a one-site change.
		req.Header.Set("X-API-Key", apiKey)
	}

	resp, err := connectHTTPClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// connectToCluster probes clusterURL, handles 401 by prompting for a
// fresh API key (once), and persists the connection on success.
func connectToCluster(clusterURL string, cm *pkgConfig.ConfigManager) error {
	if !strings.HasPrefix(clusterURL, "http://") && !strings.HasPrefix(clusterURL, "https://") {
		clusterURL = "http://" + clusterURL
	}

	fmt.Printf("Connecting to %s...\n", clusterURL)

	clientConfig, err := cm.LoadClientConfig()
	if err != nil {
		clientConfig = &pkgConfig.ClientConfig{}
	}

	apiKey := os.Getenv(clientConfig.Node.Key)
	if apiKey == "" {
		apiKey = clientConfig.Node.Key
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()

	status, err := probeCluster(ctx, clusterURL, apiKey)
	if err != nil {
		return fmt.Errorf("%s Failed to connect: %w\n\n%s Make sure the server is running:\n   zzrouter-node start", ui.GetErrorEmoji(), err, ui.GetInfoEmoji())
	}

	switch status {
	case http.StatusOK:
		host, port := parseURL(clusterURL)
		return saveConnection(clusterURL, host, port, cm)

	case http.StatusUnauthorized:
		return reconnectWithNewKey(clusterURL, clientConfig, cm)

	default:
		return fmt.Errorf("%s Server returned status %d", ui.GetErrorEmoji(), status)
	}
}

// reconnectWithNewKey prompts for a fresh admin key, writes it to .env,
// re-probes once, and persists on success. Split from connectToCluster
// so the happy path and the 401-retry path are both readable.
func reconnectWithNewKey(clusterURL string, clientConfig *pkgConfig.ClientConfig, cm *pkgConfig.ConfigManager) error {
	maskedKey := maskKey(clientConfig.Node.Key)
	fmt.Printf("%s Authentication failed: Invalid API key '%s'\n\n", ui.GetErrorEmoji(), maskedKey)

	newKey := readLine(fmt.Sprintf("%s Please enter a valid API key (or press Enter to cancel): ", ui.GetInfoEmoji()))
	if newKey == "" {
		fmt.Println("Connection cancelled.")
		return nil
	}

	// Store env var name in config, actual value in .env.
	clientConfig.Node.Key = "ZZROUTER_ADMIN_API_KEY"
	if err := pkgConfig.SetEnvVar("ZZROUTER_ADMIN_API_KEY", newKey); err != nil {
		return fmt.Errorf("%s Failed to save API key to .env: %w", ui.GetErrorEmoji(), err)
	}

	fmt.Printf("\nRetrying connection with new API key...\n")
	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()

	status, err := probeCluster(ctx, clusterURL, newKey)
	if err != nil {
		return fmt.Errorf("%s Failed to connect: %w\n\n%s Make sure the server is running:\n   zzrouter-node start", ui.GetErrorEmoji(), err, ui.GetInfoEmoji())
	}

	if status == http.StatusUnauthorized {
		return fmt.Errorf("%s Authentication failed again: The new API key is also invalid", ui.GetErrorEmoji())
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s Server returned status %d", ui.GetErrorEmoji(), status)
	}

	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		return fmt.Errorf("%s Failed to save new API key: %w", ui.GetErrorEmoji(), err)
	}
	if err := store.SetNodeKey("ZZROUTER_ADMIN_API_KEY"); err != nil {
		return fmt.Errorf("%s Failed to save new API key: %w", ui.GetErrorEmoji(), err)
	}

	fmt.Printf("%s New API key saved successfully!\n\n", ui.GetSuccessEmoji())

	host, port := parseURL(clusterURL)
	return saveConnection(clusterURL, host, port, cm)
}

func saveConnection(clusterURL, host string, port int, cm *pkgConfig.ConfigManager) error {
	store, err := pkgConfig.NewClientConfigStore(cm)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	secure := strings.HasPrefix(clusterURL, "https://")
	if err := store.SetNodeConnection(host, port, secure); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	configPath := cm.GetClientConfigPath()
	fmt.Println()
	fmt.Printf("Connected to %s\n", clusterURL)
	fmt.Printf("Saved as default cluster in %s\n", configPath)
	fmt.Println()

	return nil
}

func displayDiscoveredClusters(hosts []*network.NodeEntry) {
	columns := []ui.TableColumn{
		{Header: "NAME", MaxWidth: 30},
		{Header: "ADDRESS", MaxWidth: 20},
		{Header: "PORT", MaxWidth: 10},
	}

	var rows [][]string
	for _, host := range hosts {
		rows = append(rows, []string{
			host.Name,
			host.Node,
			fmt.Sprintf("%d", host.Port),
		})
	}

	renderer := ui.NewTerminalRendererWithOptions(nil, true, 0)
	defer func() { _ = renderer.Close() }()

	tableData := ui.TableData{
		Columns:    columns,
		Rows:       rows,
		MinColumns: 3,
	}

	if err := renderer.RenderTable(tableData); err != nil {
		fmt.Printf("Error rendering table: %v\n", err)
	}
}

// parseURL extracts host + port from a cluster URL. Accepts bare
// "host", "host:port", "http://host:port", "https://host:port", or
// IPv6 forms like "[::1]:9090". Falls back to DefaultZZROUTERPort
// when no port is present or malformed.
func parseURL(raw string) (string, int) {
	// Promote bare host / host:port to a URL-with-scheme so net/url
	// parses consistently. Can't use net/url without a scheme — it
	// treats scheme-less input as a path.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, constants.DefaultZZROUTERPort
	}

	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		// No port component — u.Host is the bare host.
		return u.Host, constants.DefaultZZROUTERPort
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port >= 65536 {
		return host, constants.DefaultZZROUTERPort
	}
	return host, port
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	if len(key) <= 16 {
		return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

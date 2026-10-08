package servercli

import (
	"fmt"
	"strconv"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// parseNodePortInput parses and validates host:port input.
func parseNodePortInput(input string) (string, int, error) {
	parts := strings.Split(input, ":")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("format must be host:port (e.g., 192.0.2.10:9090)")
	}
	host := strings.TrimSpace(parts[0])
	portStr := strings.TrimSpace(parts[1])
	if host == "" {
		return "", 0, fmt.Errorf("host cannot be empty")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port number: %s", portStr)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return host, port, nil
}

// displayCurrentClusterConfig prints the cluster endpoints configured on
// this node. Used by `cluster list` for a config-only (no server call)
// view — consistent with the pair flow being the sole path to mutate
// cluster membership.
func displayCurrentClusterConfig(config *pkgConfig.NodeConfig) {
	fmt.Println("Current cluster node(s):")
	if len(config.Cluster.Endpoints) == 0 {
		fmt.Println("  • No cluster nodes configured")
		return
	}
	for i, endpoint := range config.Cluster.Endpoints {
		fmt.Printf("  %d. %s\n", i+1, endpoint)
	}
}

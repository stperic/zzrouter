package servercli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/stperic/zzrouter/pkg/apipath"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
)

// NewClusterCmd creates the top-level cluster command. Cluster
// membership mutations go through the pairing flow (`cluster pair` on
// the worker → `cluster accept <code>` on the coordinator); the old
// admin-key `cluster add <host:port>` path was retired since it
// bypassed the mTLS handshake and duplicated the mesh-admission logic
// that now lives in admitClusterMember on the server.
func NewClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage cluster nodes",
		Long: `Manage cluster nodes for distributed model serving.

Use 'cluster pair' on a worker to join a coordinator, then 'cluster accept'
on the coordinator to finalize the handshake. 'cluster remove' drops a node
from the routing table.`,
	}

	cmd.AddCommand(
		newClusterRemoveCmd(),
		newClusterListCmd(),
		newClusterAcceptCmd(),
		newClusterPendingCmd(),
		newClusterCAFingerprintCmd(),
		newClusterPairCmd(),
		newClusterRotateKeyCmd(),
	)

	return cmd
}

func newClusterRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <host:port>",
		Short: "Remove a cluster node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return removeClusterEndpoint(args[0])
		},
	}
}

func newClusterListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all cluster nodes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listClusterNodes()
		},
	}
}

// removeClusterEndpoint removes a worker's endpoint row via the running
// server's API. Config persistence + cache invalidation happen
// server-side so no restart is required.
func removeClusterEndpoint(endpoint string) error {
	host, port, err := parseNodePortInput(endpoint)
	if err != nil {
		return err
	}
	normalizedEndpoint := fmt.Sprintf("%s:%d", host, port)

	cm := pkgConfig.NewConfigManager("zzrouter")
	config, err := cm.LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	localPort := config.Node.Port
	if localPort == 0 {
		localPort = constants.DefaultZZROUTERPort
	}
	adminKey, _ := config.Auth.GetAdminAPIKey()

	fmt.Printf("Removing node %s from cluster...", normalizedEndpoint)

	localURL := fmt.Sprintf("http://127.0.0.1:%d%s", localPort, apipath.ClusterEndpoint(normalizedEndpoint))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, localURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}

	client := &http.Client{Timeout: constants.ClusterQueryTimeout}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(" failed")
		return fmt.Errorf("local server unreachable (is it running?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		fmt.Println(" failed")
		var errorResp struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(respBody, &errorResp) == nil {
			msg := errorResp.Detail
			if msg == "" {
				msg = errorResp.Error
			}
			if msg != "" {
				fmt.Printf("Warning: %s\n\n", msg)
				return nil
			}
		}
		fmt.Printf("Warning: server returned status %d\n\n", resp.StatusCode)
		return nil
	}

	fmt.Println(" ok")
	fmt.Printf("Node %s removed from cluster\n\n", normalizedEndpoint)
	return nil
}

// listClusterNodes displays all configured cluster nodes by reading
// local config. The richer runtime view (status + resources) is on
// `zzrouter nodes` via the client CLI.
func listClusterNodes() error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	config, err := cm.LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	displayCurrentClusterConfig(config)
	return nil
}

package server

import (
	"fmt"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// buildClusternodeConfig maps the on-disk node.yaml ClusterConfig to
// the runtime clusternode.Config. No I/O beyond a filesystem probe for
// the worker-resume decision (ca.pem + coordinator_url both present
// AND identity chains to the stored CA = Worker resume; anything else
// falls back to Unclaimed).
//
// Stays in internal/server so pkg/cluster/node doesn't grow a
// pkg/config dependency.
func buildClusternodeConfig(cfg *pkgConfig.NodeConfig) (clusternode.Config, error) {
	paths := clusternode.PathsFromConfigDir(pkgConfig.Paths().GetConfigDir())

	nodeCfg := clusternode.Config{
		Port:                 cfg.Cluster.BindPort,
		BindHost:             cfg.Cluster.BindAddr,
		IdentityDir:          paths.IdentityDir,
		CADir:                paths.CADir,
		ClusterDir:           paths.ClusterDir,
		PairingPath:          paths.PairingPath,
		NodeName:             cfg.Node.Name,
		AdvertiseDNSNames:    cfg.Cluster.AdvertiseDNSNames,
		AdvertiseIPs:         clusternode.ParseAdvertiseIPs(cfg.Cluster.AdvertiseIPs),
		RequireSecurePairing: cfg.Cluster.RequireSecurePairing,
	}

	switch cfg.Cluster.Mode {
	case pkgConfig.ClusterModeDisabled, pkgConfig.ClusterModeStandalone, "":
		nodeCfg.Mode = clusternode.Disabled
	case pkgConfig.ClusterModeCoordinator:
		nodeCfg.Mode = clusternode.Coordinator
	case pkgConfig.ClusterModeWorker:
		if clusternode.IsPaired(paths) {
			nodeCfg.Mode = clusternode.Worker
			url, err := clusternode.ReadCoordinatorURL(paths.ClusterDir)
			if err != nil {
				return clusternode.Config{}, fmt.Errorf("worker mode: read coordinator URL: %w", err)
			}
			nodeCfg.CoordinatorURL = url
		} else {
			nodeCfg.Mode = clusternode.Unclaimed
		}
	default:
		return clusternode.Config{}, fmt.Errorf("unknown cluster.mode: %q", cfg.Cluster.Mode)
	}

	return nodeCfg, nil
}

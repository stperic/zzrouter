package server

import (
	"testing"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baseNodeCfg returns a minimal NodeConfig with cluster + node name set.
// Each test case overrides the cluster mode.
func baseNodeCfg(mode pkgConfig.ClusterMode) *pkgConfig.NodeConfig {
	return &pkgConfig.NodeConfig{
		Node: pkgConfig.ServeConfig{
			Name: "test-node",
		},
		Cluster: pkgConfig.ClusterConfig{
			Mode:     mode,
			BindAddr: "127.0.0.1",
			BindPort: 0, // OS-assigned for tests
		},
	}
}

func TestBuildClusternodeConfig_Modes(t *testing.T) {
	tests := []struct {
		name string
		mode pkgConfig.ClusterMode
		want clusternode.Mode
	}{
		{"empty mode defaults to Disabled", "", clusternode.Disabled},
		{"explicit disabled", pkgConfig.ClusterModeDisabled, clusternode.Disabled},
		{"standalone maps to Disabled", pkgConfig.ClusterModeStandalone, clusternode.Disabled},
		{"coordinator", pkgConfig.ClusterModeCoordinator, clusternode.Coordinator},
		// Worker is covered by pkg/cluster/node/paths_test.go IsPaired tests;
		// its mode depends on filesystem state that shared-path tests can't
		// guarantee here.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nc := baseNodeCfg(tt.mode)
			got, err := buildClusternodeConfig(nc)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Mode)
		})
	}
}

func TestBuildClusternodeConfig_RejectsUnknownMode(t *testing.T) {
	nc := baseNodeCfg("bogus")
	_, err := buildClusternodeConfig(nc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown cluster.mode")
}

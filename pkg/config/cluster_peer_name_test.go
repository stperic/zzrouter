package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SetClusterEndpointName caches a probed peer's name and, crucially,
// reports whether it actually changed anything.
//
// It runs on the probe path, so it is called on every health tick for
// every peer. Without the unchanged short-circuit that is a node.yaml
// rewrite every 30 seconds per peer, forever, to store what is already
// there.
func TestSetClusterEndpointName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)

	store := NewNodeConfigStoreFromConfig(&NodeConfig{
		Node:    ServeConfig{Bind: "localhost", Port: 12345, Name: "coord"},
		Cluster: ClusterConfig{Mode: ClusterModeDisabled},
	})
	require.NoError(t, store.AddClusterEndpoint("198.51.100.235:9090"))

	changed, err := store.SetClusterEndpointName("198.51.100.235:9090", "WINDOWS-WORKER")
	require.NoError(t, err)
	assert.True(t, changed, "first name must be recorded")

	body, err := os.ReadFile(filepath.Join(root, "node.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "WINDOWS-WORKER", "name must reach node.yaml")

	// The steady state: re-probing an already-named peer writes nothing.
	changed, err = store.SetClusterEndpointName("198.51.100.235:9090", "WINDOWS-WORKER")
	require.NoError(t, err)
	assert.False(t, changed, "unchanged name must not rewrite config")

	// A peer that genuinely renamed is followed.
	changed, err = store.SetClusterEndpointName("198.51.100.235:9090", "renamed-box")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "renamed-box", store.Config().Cluster.Endpoints.NameFor("198.51.100.235:9090"))

	// Membership belongs to AddClusterEndpoint alone: naming an address
	// that is not a member must not quietly admit it to the cluster.
	changed, err = store.SetClusterEndpointName("10.9.9.9:9090", "intruder")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Len(t, store.Config().Cluster.Endpoints, 1, "unknown address must not be added")

	// Empty inputs are no-ops rather than a way to blank a name.
	changed, err = store.SetClusterEndpointName("198.51.100.235:9090", "")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, "renamed-box", store.Config().Cluster.Endpoints.NameFor("198.51.100.235:9090"))
}

// Removing a peer must take its cached name with it, or a later re-add
// of the same address would inherit a name nobody verified.
func TestRemoveClusterEndpoint_DropsCachedName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)

	store := NewNodeConfigStoreFromConfig(&NodeConfig{
		Node:    ServeConfig{Bind: "localhost", Port: 12345, Name: "coord"},
		Cluster: ClusterConfig{Mode: ClusterModeDisabled},
	})
	require.NoError(t, store.AddClusterEndpoint("198.51.100.235:9090"))
	_, err := store.SetClusterEndpointName("198.51.100.235:9090", "WINDOWS-WORKER")
	require.NoError(t, err)

	found, err := store.RemoveClusterEndpoint("198.51.100.235:9090")
	require.NoError(t, err)
	require.True(t, found)

	require.NoError(t, store.AddClusterEndpoint("198.51.100.235:9090"))
	assert.Empty(t, store.Config().Cluster.Endpoints.NameFor("198.51.100.235:9090"),
		"re-added peer must start unnamed until it is probed")
}

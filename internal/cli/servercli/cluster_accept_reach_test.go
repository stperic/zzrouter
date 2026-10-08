package servercli

import (
	"strings"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const nodesBody = `{"data":[
  {"name":"macbook-pro","health_status":"healthy"},
  {"name":"worker-1","health_status":"healthy"},
  {"name":"windows-desktop","health_status":"unknown"}
]}`

// TestDecodeNodeHealth_FindsTheNamedNode covers the read that decides
// whether a freshly paired worker is actually usable.
func TestDecodeNodeHealth_FindsTheNamedNode(t *testing.T) {
	health, err := decodeNodeHealth(strings.NewReader(nodesBody), "worker-1")
	require.NoError(t, err)
	assert.Equal(t, healthStatusHealthy, health)
}

// TestDecodeNodeHealth_ReportsAnUnreachableWorker is the case worth
// catching: pairing reported success, and the node is registered but
// answering nothing.
func TestDecodeNodeHealth_ReportsAnUnreachableWorker(t *testing.T) {
	health, err := decodeNodeHealth(strings.NewReader(nodesBody), "windows-desktop")
	require.NoError(t, err)
	assert.NotEqual(t, healthStatusHealthy, health)
	assert.Equal(t, "unknown", health)
}

// TestDecodeNodeHealth_MatchesCaseInsensitively keeps a node whose
// registered name differs in case from the accept response from
// looking absent, which would read as a much worse failure than it is.
func TestDecodeNodeHealth_MatchesCaseInsensitively(t *testing.T) {
	health, err := decodeNodeHealth(strings.NewReader(nodesBody), "Worker-1")
	require.NoError(t, err)
	assert.Equal(t, healthStatusHealthy, health)
}

// TestDecodeNodeHealth_AbsentNodeIsAnError keeps "not registered yet"
// distinct from "registered and unhealthy": only the second is a
// verdict.
func TestDecodeNodeHealth_AbsentNodeIsAnError(t *testing.T) {
	_, err := decodeNodeHealth(strings.NewReader(nodesBody), "nowhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nowhere")
}

// TestClusterPortOf_FallsBackToTheDefault pins the port named in the
// warning text. Telling an operator to open the wrong port is worse
// than telling them nothing.
func TestClusterPortOf_FallsBackToTheDefault(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{}
	assert.Equal(t, constants.DefaultClusterPort, clusterPortOf(cfg))

	cfg.Cluster.BindPort = 19091
	assert.Equal(t, 19091, clusterPortOf(cfg))
}

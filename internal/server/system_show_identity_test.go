package server

import (
	"encoding/json"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShowLocal_ReportsLoadedConfig is what makes a split config
// diagnosable: the server names the file it actually read, so a CLI
// that loaded a different one can say so instead of leaving the
// operator to infer it from an unrelated symptom.
func TestShowLocal_ReportsLoadedConfig(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.ClusterMode = pkgConfig.ClusterModeCoordinator
	server := createTestNode(t, cfg)
	server.config.SourcePath = "/etc/zzrouter/node.yaml"

	resp := makeAuthRequest(t, server, "GET", "/zzrouter/v1/server/identity", TestAdminKey, nil)
	require.Equal(t, 200, resp.Code)

	var body struct {
		Data struct {
			Server      string `json:"server"`
			Cluster     bool   `json:"cluster"`
			ClusterMode string `json:"cluster_mode"`
			ConfigPath  string `json:"config_path"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	assert.Equal(t, "/etc/zzrouter/node.yaml", body.Data.ConfigPath)
	assert.Equal(t, string(pkgConfig.ClusterModeCoordinator), body.Data.ClusterMode)
	assert.True(t, body.Data.Cluster)
	assert.NotEmpty(t, body.Data.Server)
}

// TestNodeIdentityReport_UnsetModeReadsAsDisabled pins the rendering
// callers compare against, so an unset mode is reported as a value
// rather than as an empty string they would have to guess at.
func TestNodeIdentityReport_UnsetModeReadsAsDisabled(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{}
	cfg.Node.Name = "n1"

	assert.Equal(t, string(pkgConfig.ClusterModeDisabled), nodeIdentityReportFrom(cfg).Mode)
}

// TestNodeIdentityReport_ConfigPathIsLoopbackOnly keeps the config
// path, which names the OS account the server runs under, from
// reaching a caller on the LAN through the unauthenticated health
// route, while still answering this node's own CLI.
func TestNodeIdentityReport_ConfigPathIsLoopbackOnly(t *testing.T) {
	cfg := &pkgConfig.NodeConfig{SourcePath: "/etc/zzrouter/node.yaml"}
	cfg.Node.Name = "n1"
	cfg.Cluster.Mode = pkgConfig.ClusterModeWorker
	report := nodeIdentityReportFrom(cfg)

	for _, addr := range []string{"127.0.0.1:51000", "[::1]:51000", "127.0.0.1"} {
		assert.Equal(t, cfg.SourcePath, report.forRemoteAddr(addr).ConfigPath, "loopback caller %q", addr)
	}
	for _, addr := range []string{"198.51.100.235:51000", "192.168.1.9:80", "", "garbage"} {
		assert.Empty(t, report.forRemoteAddr(addr).ConfigPath, "off-box caller %q", addr)
	}

	// Name and mode are not withheld: they are what a peer already
	// learns from any cluster exchange.
	remote := report.forRemoteAddr("198.51.100.235:51000")
	assert.Equal(t, "n1", remote.Name)
	assert.Equal(t, string(pkgConfig.ClusterModeWorker), remote.Mode)
}

// TestHealthDetailed_ReportsIdentityOnAWorker is the case /show cannot
// cover: a worker mounts no /zzrouter/v1/* at all, and a worker is
// exactly where a service-versus-operator config split shows up. Health
// is the one route every node serves.
func TestHealthDetailed_ReportsIdentityOnAWorker(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.ClusterMode = pkgConfig.ClusterModeWorker
	server := createTestNode(t, cfg)

	resp := makeRequest(t, server, TestRequest{Method: "GET", Path: "/health?detailed=true"})
	require.Equal(t, 200, resp.Code)

	var body struct {
		Details struct {
			NodeIdentity NodeIdentityReport `json:"node_identity"`
		} `json:"details"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	assert.Equal(t, string(pkgConfig.ClusterModeWorker), body.Details.NodeIdentity.Mode)
	assert.NotEmpty(t, body.Details.NodeIdentity.Name)
}

// TestHealth_OmitsIdentityByDefault keeps the payload that probes and
// peer version discovery parse unchanged: identity is opt-in.
func TestHealth_OmitsIdentityByDefault(t *testing.T) {
	server := createTestNodeWithDefaults(t)

	resp := makeRequest(t, server, TestRequest{Method: "GET", Path: "/health"})
	require.Equal(t, 200, resp.Code)

	var body struct {
		Details map[string]any `json:"details"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	assert.NotContains(t, body.Details, "node_identity")
}

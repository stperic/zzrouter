package servercli

import (
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localConfig(path string, mode config.ClusterMode) *config.NodeConfig {
	cfg := &config.NodeConfig{SourcePath: path}
	cfg.Node.Name = "this-node"
	cfg.Cluster.Mode = mode
	return cfg
}

// TestIdentityMismatch_SilentWhenAgreed keeps the common case quiet:
// one config, one server, nothing to warn about.
func TestIdentityMismatch_SilentWhenAgreed(t *testing.T) {
	local := localConfig("/home/op/node.yaml", config.ClusterModeWorker)
	remote := &serverIdentity{Name: "this-node", ClusterMode: "worker", ConfigPath: "/home/op/node.yaml"}

	assert.Nil(t, identityMismatch(local, remote))
}

// TestIdentityMismatch_DifferentConfigFile is the case that cost a full
// onboarding session: a service running as another account reads that
// account's node.yaml, so the CLI configures a file the server never
// reads and every symptom shows up somewhere else.
func TestIdentityMismatch_DifferentConfigFile(t *testing.T) {
	local := localConfig(`C:\Users\op\AppData\Roaming\zzrouter\node.yaml`, config.ClusterModeWorker)
	remote := &serverIdentity{
		Name:        "this-node",
		ClusterMode: "worker",
		ConfigPath:  `C:\Windows\System32\config\systemprofile\AppData\Roaming\zzrouter\node.yaml`,
	}

	lines := identityMismatch(local, remote)
	require.NotEmpty(t, lines)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, remote.ConfigPath, "the server's own file is the actionable detail")
}

// TestIdentityMismatch_DifferentMode covers the same split seen through
// its symptom rather than its cause: the CLI believes it is a worker
// while the server it is talking to is a coordinator.
func TestIdentityMismatch_DifferentMode(t *testing.T) {
	local := localConfig("/home/op/node.yaml", config.ClusterModeWorker)
	remote := &serverIdentity{Name: "this-node", ClusterMode: "coordinator", ConfigPath: "/home/op/node.yaml"}

	lines := identityMismatch(local, remote)
	require.NotEmpty(t, lines)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "coordinator")
	assert.Contains(t, joined, "worker")
}

// TestIdentityMismatch_UnsetLocalModeReadsAsDisabled pins the rendering
// the server uses for an unset mode, so an empty local mode and a
// server reporting "disabled" do not look like a disagreement.
func TestIdentityMismatch_UnsetLocalModeReadsAsDisabled(t *testing.T) {
	local := localConfig("/home/op/node.yaml", "")
	remote := &serverIdentity{Name: "this-node", ClusterMode: "disabled", ConfigPath: "/home/op/node.yaml"}

	assert.Nil(t, identityMismatch(local, remote))
}

// TestIdentityMismatch_SilentWhenServerOmitsFields keeps a server that
// does not report these fields from being accused of a mismatch.
func TestIdentityMismatch_SilentWhenServerOmitsFields(t *testing.T) {
	local := localConfig("/home/op/node.yaml", config.ClusterModeWorker)
	remote := &serverIdentity{Name: "this-node"}

	assert.Nil(t, identityMismatch(local, remote))
}

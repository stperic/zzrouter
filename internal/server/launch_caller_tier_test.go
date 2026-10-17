package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

// What a launch stores is what a restart replays. EnsureRun keeps
// the parameters it was handed as the request tier and re-reads tiers
// 0-3 from current config on every relaunch, which is what lets a
// restart pick up a parameter change at all
// (pkg/prov_apps: TestLaunchStoresRequestTierNotMergedResult).
//
// So a caller that resolves the tree itself and hands the result over
// does not get a second opinion, it gets the last word: every
// configured value comes back as if the caller had typed it, and from
// then on it outranks the config it was read from. The launch path did
// exactly that — it walked the tiers, then passed the merged map as the
// request — so a run started this way could never see a later edit,
// however many times it was restarted.
func TestEnsureRun_HandsTheManagerTheCallersTierOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tier-model", "gguf")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tier-model.gguf"), []byte("GGUF"), 0o644))

	cfg := DefaultTestNodeConfig()
	cfg.NodeName = "tier-node"
	server := createSeededTestNode(t, seededTestNodeConfig{TestNodeConfig: cfg, modelsDir: root})

	const provider = "tierprov"
	enabled := true
	require.NoError(t, server.appsConfig().AddApp(provider, pkgConfig.ServiceConfig{
		Enabled:  &enabled,
		Name:     "Tier Provider",
		Protocol: pkgConfig.ProtocolOpenAI,
		Mode:     "on-demand",
		Runtime: &pkgConfig.AppRuntimeConfig{
			PortRange: []int{8400, 8405},
			BasePort:  8400,
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "sleep", Args: []string{"2"}},
		},
		// A value the tree supplies and the caller does not.
		Defaults:     &pkgConfig.AppDefaultsConfig{Parameters: map[string]string{"ctx-size": "8192"}},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))

	server.providers.appMgr.ReloadConfig(server.appsConfig())
	res, err := server.providers.appMgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Model: "tier-model", Provider: provider, Parameters: map[string]string{"seed": "42"}})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"seed": "42"}, res.Run.SnapshotConfig().Parameters,
		"the launch request must carry the caller's parameters and nothing else; "+
			"a configured value in here is replayed on every restart as the caller's own")
	assert.NotContains(t, res.Run.SnapshotConfig().Parameters, "ctx-size",
		"the tier tree is resolved inside EnsureRun, once")
}

package servercli

import (
	"os"
	"path/filepath"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

func providerCommandHome(t *testing.T, nodeYAML string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	t.Setenv("ZZROUTER_CONFIG_DIR", root)
	t.Setenv("ZZROUTER_DISABLE_DOTENV", "true")
	require.NoError(t, os.WriteFile(filepath.Join(root, "node.yaml"), []byte(nodeYAML), 0600))
	require.NoError(t, templates.InstallDefaults(filepath.Join(root, "providers")))
	return root
}

// The CLI records the node's own choice, as the server's finalization does.
func TestProviderEnableRecordsThisNodesChoice(t *testing.T) {
	providerCommandHome(t, "node:\n  name: mac\ncluster:\n  mode: coordinator\n")
	require.NoError(t, toggleApp("vllm", true))
	store, err := pkgConfig.NewAppsConfigStoreFromStandardLocations("mac")
	require.NoError(t, err)
	sc, _ := store.Config().LookupApp("vllm")
	require.True(t, sc.IsEnabled())
	require.False(t, *store.Config().GetOnDemand("vllm").Enabled, "other nodes keep the default")
}

// Sync never carries a cloud provider, so a worker's copy is its own.
func TestProviderEnableOnAWorkerWritesWhatSyncNeverCarries(t *testing.T) {
	providerCommandHome(t, "node:\n  name: localai\ncluster:\n  mode: worker\n")
	require.NoError(t, toggleApp("openai", true))
	store, err := pkgConfig.NewAppsConfigStoreFromStandardLocations("localai")
	require.NoError(t, err)
	sc, _ := store.Config().LookupApp("openai")
	require.True(t, sc.IsEnabled())
}

func TestProviderEnableRefusesOnAWorker(t *testing.T) {
	root := providerCommandHome(t, "node:\n  name: localai\ncluster:\n  mode: worker\n")
	before, err := os.ReadFile(filepath.Join(root, "providers", "on-demand", "vllm", "config.yaml"))
	require.NoError(t, err)
	err = toggleApp("vllm", true)
	require.ErrorContains(t, err, "PATCH /zzrouter/v1/providers/vllm?node=localai")
	after, err := os.ReadFile(filepath.Join(root, "providers", "on-demand", "vllm", "config.yaml"))
	require.NoError(t, err)
	require.Equal(t, before, after)
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPathResolver_TestHomeOverride_RedirectsAllDirs verifies that setting
// ZZROUTER_TEST_HOME redirects the entire path tree under a single root so
// tests never clobber the user's real XDG config.
func TestPathResolver_TestHomeOverride_RedirectsAllDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)

	p := NewPathResolver("zzrouter")

	assert.Equal(t, root, p.GetConfigDir())
	assert.Equal(t, root, p.GetDataDir())
	assert.Equal(t, filepath.Join(root, SubdirCache), p.GetCacheDir())
	assert.Equal(t, filepath.Join(root, SubdirLogs), p.GetLogsDir())
	assert.Equal(t, filepath.Join(root, "node.yaml"), p.GetNodeConfigPath())
	assert.Equal(t, filepath.Join(root, "providers"), p.GetAppsConfigDir())
	assert.Equal(t, filepath.Join(root, ".env"), p.GetEnvFilePath())
}

// TestPathResolver_TestHomeOverride_IsDynamic asserts that clearing and
// resetting the env var takes effect on subsequent accesses — the resolver
// must not cache the layout, otherwise t.Setenv in tests won't work when
// the resolver was constructed before the env was set.
func TestPathResolver_TestHomeOverride_IsDynamic(t *testing.T) {
	p := NewPathResolver("zzrouter")
	originalConfig := p.GetConfigDir()
	require.False(t, strings.HasPrefix(originalConfig, os.TempDir()), "precondition: real config dir should not be under TempDir")

	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	assert.Equal(t, root, p.GetConfigDir(), "override must apply to a resolver created before the env was set")

	t.Setenv("ZZROUTER_TEST_HOME", "")
	assert.Equal(t, originalConfig, p.GetConfigDir(), "clearing override must restore the real path")
}

// TestNodeConfigStore_SaveHonorsTestHomeOverride is the regression test for
// the bug where integration tests wrote over the user's real node.yaml.
// With ZZROUTER_TEST_HOME set, a mutation + save must land inside the
// override dir, and the real XDG node.yaml must be untouched.
func TestNodeConfigStore_SaveHonorsTestHomeOverride(t *testing.T) {
	// Snapshot the real config path's state BEFORE the override is set, so
	// we can compare after.
	realPaths := NewPathResolver("zzrouter")
	realNodeYAML := realPaths.GetNodeConfigPath()
	realBefore, realExistedBefore := readFileIfExists(t, realNodeYAML)

	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)

	cfg := &NodeConfig{
		Node: ServeConfig{
			Bind: "localhost",
			Port: 12345,
			Name: "isolation-test",
		},
		Cluster: ClusterConfig{Mode: ClusterModeDisabled},
	}
	store := NewNodeConfigStoreFromConfig(cfg)

	require.NoError(t, store.AddClusterEndpoint("10.0.0.1:9090"))

	// Override dir must now contain node.yaml.
	overrideNodeYAML := filepath.Join(root, "node.yaml")
	_, err := os.Stat(overrideNodeYAML)
	require.NoError(t, err, "node.yaml must be written under the override root")

	overrideContent, err := os.ReadFile(overrideNodeYAML)
	require.NoError(t, err)
	assert.Contains(t, string(overrideContent), "10.0.0.1:9090", "override node.yaml must carry the mutation")
	assert.Contains(t, string(overrideContent), "isolation-test", "override node.yaml must carry the test-only node name")

	// Real XDG node.yaml must be byte-identical to its pre-test state.
	realAfter, realExistsAfter := readFileIfExists(t, realNodeYAML)
	assert.Equal(t, realExistedBefore, realExistsAfter, "real node.yaml existence must not change")
	assert.Equal(t, realBefore, realAfter, "real node.yaml content must not be modified by test-scoped save")
}

func readFileIfExists(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false
	}
	require.NoError(t, err)
	return b, true
}

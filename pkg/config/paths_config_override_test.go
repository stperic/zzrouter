package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigDirOverrideMovesConfigRoot pins the whole point of the
// override: a service and an operator CLI running as different accounts
// must resolve the same config root, including the cluster material
// derived from it. Without this, a service reads a freshly-defaulted
// node.yaml under its own profile and reports a mode nobody set.
func TestConfigDirOverrideMovesConfigRoot(t *testing.T) {
	want := t.TempDir()
	t.Setenv(envConfigDirOverride, want)

	p := NewPathResolver("zzrouter")

	assert.Equal(t, want, p.GetConfigDir())
	assert.Equal(t, filepath.Join(want, "node.yaml"), p.GetNodeConfigPath())
	assert.Equal(t, filepath.Join(want, "providers"), p.GetAppsConfigDir())
	assert.Equal(t, filepath.Join(want, ".env"), p.GetEnvFilePath())
}

// TestConfigDirOverrideLeavesDataAndLogs guards the boundary: the
// override is config-only. Redirecting data or logs as a side effect
// would silently relocate models and PIDs.
func TestConfigDirOverrideLeavesDataAndLogs(t *testing.T) {
	p := NewPathResolver("zzrouter")
	baseData := p.GetDataDir()
	baseLogs := p.GetLogsDir()
	baseCache := p.GetCacheDir()

	t.Setenv(envConfigDirOverride, t.TempDir())

	assert.Equal(t, baseData, p.GetDataDir())
	assert.Equal(t, baseLogs, p.GetLogsDir())
	assert.Equal(t, baseCache, p.GetCacheDir())
}

// TestConfigDirOverrideUnsetKeepsPlatformDefault ensures the override is
// inert when absent, so the ordinary single-account install is untouched.
func TestConfigDirOverrideUnsetKeepsPlatformDefault(t *testing.T) {
	p := NewPathResolver("zzrouter")
	def := p.GetConfigDir()
	require.NotEmpty(t, def)

	t.Setenv(envConfigDirOverride, "")
	assert.Equal(t, def, p.GetConfigDir())
}

// TestTestHomeOverrideStillWins keeps the test harness authoritative:
// ZZROUTER_TEST_HOME pins every directory, and a stray machine-wide
// ZZROUTER_CONFIG_DIR must not leak into a test run.
func TestTestHomeOverrideStillWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envTestHomeOverride, home)
	t.Setenv(envConfigDirOverride, t.TempDir())

	p := NewPathResolver("zzrouter")
	assert.Equal(t, home, p.GetConfigDir())
}

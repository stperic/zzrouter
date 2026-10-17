package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tree that exists but fails to load must not read as "no providers":
// startup treats only ErrProvidersDirNotFound as survivable.
func TestStandardLocationsSeparateMissingFromBroken(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envTestHomeOverride, home)

	_, _, err := LoadAppsConfigFromStandardLocations()
	require.ErrorIs(t, err, ErrProvidersDirNotFound)
	_, err = NewAppsConfigStoreFromStandardLocations("node-a")
	require.ErrorIs(t, err, ErrProvidersDirNotFound)

	dir := filepath.Join(NewPathResolver("zzrouter").GetAppsConfigDir(), "on-demand", "llamacpp")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("unknown_key: 1\n"), 0o600))

	_, _, err = LoadAppsConfigFromStandardLocations()
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrProvidersDirNotFound)
	_, err = NewAppsConfigStoreFromStandardLocations("node-a")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrProvidersDirNotFound)
}

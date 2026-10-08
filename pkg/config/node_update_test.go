package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeUpdateEnabledPersistsOnlyScheduling(t *testing.T) {
	cfg := &NodeConfig{}
	cfg.Update.Channel = "beta"
	store := NewNodeConfigStoreFromConfig(cfg)
	store.dir = t.TempDir()
	require.NoError(t, store.SetUpdateEnabled(false))
	require.False(t, store.Config().Update.IsEnabled())
	loaded, err := store.cm.LoadNodeConfigFromDir(store.dir)
	require.NoError(t, err)
	require.False(t, loaded.Update.IsEnabled())
	require.Equal(t, "beta", loaded.Update.Channel)
	require.True(t, cfg.Update.IsEnabled(), "mutation changed an earlier reader's snapshot")
	file := filepath.Join(t.TempDir(), "not-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0600))
	store.dir = file
	require.Error(t, store.SetUpdateEnabled(true))
	require.False(t, store.Config().Update.IsEnabled(), "failed save changed state")
}

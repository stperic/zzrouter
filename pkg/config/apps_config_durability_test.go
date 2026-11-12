package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stretchr/testify/require"
)

func durabilityConfig(t *testing.T) (*AppsConfigStore, string) {
	t.Helper()
	cfg := &AppsConfig{}
	//nolint:gosec // G101: environment placeholder in a disposable persistence fixture.
	require.NoError(t, cfg.AddApp("a", ServiceConfig{Mode: "cloud", Description: "before", Enabled: new(false), Runtime: &AppRuntimeConfig{Endpoint: "https://example.test", API: &APIConfig{AuthType: AuthTypeBearer, Token: "${DURABILITY_TEST_TOKEN}"}}, Capabilities: &AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	dir := t.TempDir()
	require.NoError(t, cfg.SaveToDir(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	return store, dir
}

func TestProviderMutationReconcilesPartialFiles(t *testing.T) {
	store, dir := durabilityConfig(t)
	previous := store.Config()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "z"), []byte("blocks new directory"), 0600))
	notifications := 0
	store.OnChange("observe", func(cfg *AppsConfig) ReloadDisposition {
		notifications++
		require.Equal(t, "committed", cfg.GetCloud("a").Description)
		return DispositionApplied
	})
	err := store.mutate(func(cfg *AppsConfig) (bool, error) {
		if err := cfg.UpdateApp("a", func(s *ServiceConfig) error { s.Description = "committed"; return nil }); err != nil {
			return false, err
		}
		app, _ := cfg.LookupApp("a")
		return true, cfg.AddApp("z", app)
	})
	require.Error(t, err)
	require.Equal(t, "committed", store.Config().GetCloud("a").Description, "the first file committed before the later mkdir failure")
	require.Nil(t, store.Config().GetCloud("z"), "never publish the entire proposed tree after a partial save")
	require.Equal(t, "before", previous.GetCloud("a").Description, "old snapshots remain immutable")
	require.Equal(t, 1, notifications)
	reopened, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	require.Equal(t, store.Config().Fingerprint(), reopened.Config().Fingerprint())
}

func TestProviderDurabilityErrorPublishesCommittedBytes(t *testing.T) {
	store, dir := durabilityConfig(t)
	next := store.Config().clone()
	require.NoError(t, next.UpdateApp("a", func(s *ServiceConfig) error { s.Enabled = new(true); return nil }))
	require.NoError(t, next.SaveToDir(dir))
	notifications := 0
	store.OnChange("observe", func(cfg *AppsConfig) ReloadDisposition {
		notifications++
		require.True(t, cfg.GetCloud("a").IsEnabled())
		return DispositionApplied
	})
	// The shared primitive tests the filesystem fault itself. Exercise the
	// router boundary with the same error after real replacement committed.
	store.mu.Lock()
	err := store.reconcileWriteFailure("mutation", atomicfile.ErrPostRenameDurability)
	require.ErrorIs(t, err, atomicfile.ErrPostRenameDurability)
	require.True(t, store.Config().GetCloud("a").IsEnabled())
	require.Equal(t, 1, notifications)
}

func TestProviderSyncReconcilesConfigWhenLaterSchemaWriteFails(t *testing.T) {
	store, dir := durabilityConfig(t)
	app := *store.Config().GetCloud("a")
	app.Description = "new config"
	body, err := marshalProviderBody(&app)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cloud", "a", "schema.yaml"), 0700))
	notified := false
	store.OnChange("observe", func(cfg *AppsConfig) ReloadDisposition { notified = true; return DispositionApplied })
	changed, err := store.WriteProviderFiles("a", KindCloud, ProviderFiles{Config: body, Schema: []byte("new schema")})
	require.Error(t, err)
	require.True(t, changed)
	require.True(t, notified)
	require.Equal(t, "new config", store.Config().GetCloud("a").Description)
}

func TestProviderFailedPostWriteReloadFailsClosedUntilRepair(t *testing.T) {
	store, dir := durabilityConfig(t)
	body, err := os.ReadFile(filepath.Join(dir, "cloud", "a", "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "a", "config.yaml"), []byte("malformed: ["), 0600))
	unavailable := false
	store.OnChange("observe", func(cfg *AppsConfig) ReloadDisposition { unavailable = cfg == nil; return DispositionApplied })
	store.mu.Lock()
	require.Error(t, store.reconcileWriteFailure("mutation", errors.New("partial write")))
	require.Nil(t, store.Config())
	require.True(t, unavailable)
	_, err = store.SetProviderEnabled("a", "", true)
	require.Error(t, err, "cannot save an unreadable tree")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cloud", "a", "config.yaml"), body, 0600))
	_, err = store.Reload()
	require.NoError(t, err)
	require.NotNil(t, store.Config())
}

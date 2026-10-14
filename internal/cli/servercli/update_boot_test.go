package servercli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func updateBootFixture(t *testing.T) (*pkgConfig.NodeConfig, *update.Confirmer) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	t.Setenv("ZZROUTER_CONFIG_DIR", root)
	t.Setenv("ZZROUTER_DISABLE_DOTENV", "true")
	dir := filepath.Join(root, "providers", "external", "custom")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("protocol: openai\nenabled: false\nruntime: {endpoint: 'http://127.0.0.1:1'}\noperator_typo: true\n"), 0600))
	confirmer := update.NewConfirmerIn(root, 2)
	require.NoError(t, confirmer.Record(&version.Version{Major: 1}, &version.Version{Major: 2}, nil))
	return &pkgConfig.NodeConfig{}, confirmer
}

func readBootRecord(t *testing.T, confirmer *update.Confirmer) update.PendingConfirm {
	t.Helper()
	data, err := os.ReadFile(confirmer.Path())
	require.NoError(t, err)
	var pending update.PendingConfirm
	require.NoError(t, json.Unmarshal(data, &pending))
	return pending
}

func TestProviderRefusalCountsOnlyForegroundBoots(t *testing.T) {
	cfg, confirmer := updateBootFixture(t)
	for _, tc := range []struct {
		name     string
		detach   bool
		attempts int
	}{
		{"detached parent", true, 0},
		{"foreground child", false, 1},
		{"another parent", true, 1},
		{"second boot", false, 2},
		{"third boot", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := startPreparedNode(cfg, NewStartCmd(), tc.detach)
			var schemaErr *jsonschema.ValidationError
			require.ErrorAs(t, err, &schemaErr, "the strict provider loader must refuse the typo")
			pending := readBootRecord(t, confirmer)
			assert.Equal(t, tc.attempts, pending.Attempts)
			assert.Equal(t, "1.0.0", pending.FromVersion)
			assert.Equal(t, "2.0.0", pending.ToVersion)
		})
	}
}

func TestProviderRefusalWithoutPendingUpdateCreatesNoRecord(t *testing.T) {
	cfg, confirmer := updateBootFixture(t)
	require.NoError(t, confirmer.Commit())
	err := startPreparedNode(cfg, NewStartCmd(), false)
	var schemaErr *jsonschema.ValidationError
	require.ErrorAs(t, err, &schemaErr)
	assert.NoFileExists(t, confirmer.Path())
}

func TestUpdateGuardCanceledConfirmationRetainsRecord(t *testing.T) {
	cfg, confirmer := updateBootFixture(t)
	guard := newUpdateGuard(cfg)
	require.NotNil(t, guard)
	before := readBootRecord(t, confirmer)
	done := make(chan struct{})
	close(done)
	assert.False(t, guard.ConfirmWhenHealthy(done))
	assert.Equal(t, before, readBootRecord(t, confirmer))
}

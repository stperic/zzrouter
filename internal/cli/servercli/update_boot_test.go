package servercli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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

func TestProviderPreparationFailureCountsBootWithoutConfirmation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission failures require an unprivileged node identity")
	}
	for _, failure := range []string{"receipt", "required repair"} {
		t.Run(failure, func(t *testing.T) {
			cfg, confirmer := updateBootFixture(t)
			dir := pkgConfig.Paths().GetAppsConfigDir()
			require.NoError(t, os.Remove(filepath.Join(dir, "external", "custom", "config.yaml")))
			require.NoError(t, templates.InstallDefaults(dir))
			if failure == "receipt" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".managed-ownership.json"), []byte("{"), 0600))
			} else {
				path := filepath.Join(dir, "on-demand", "comfyui", "config.yaml")
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				var live map[string]any
				require.NoError(t, yaml.Unmarshal(body, &live))
				live["runtime"].(map[string]any)["max_concurrent_requests"] = 2
				body, err = yaml.Marshal(live)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, body, 0600))
				_, err = pkgConfig.LoadAppsConfig(dir)
				require.NoError(t, err, "the residual bytes decode, but must not allow confirmation")
				parent := filepath.Dir(path)
				require.NoError(t, os.Chmod(parent, 0500))
				t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0700)) })
			}
			err := startPreparedNode(cfg, NewStartCmd(), false)
			if failure == "receipt" {
				var syntax *json.SyntaxError
				require.ErrorAs(t, err, &syntax)
			} else {
				require.ErrorIs(t, err, fs.ErrPermission)
			}
			pending := readBootRecord(t, confirmer)
			assert.Equal(t, 1, pending.Attempts)
			assert.Equal(t, "2.0.0", pending.ToVersion)
		})
	}
}

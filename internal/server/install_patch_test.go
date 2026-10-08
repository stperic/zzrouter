package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallPatchRejectsBeforeWriting(t *testing.T) {
	cfg := DefaultTestNodeConfig()
	cfg.SeedProvidersDir = true
	server := createTestNode(t, cfg)
	node := server.node.Nodename()
	path := filepath.Join(server.configStore.DirPath(), "on-demand", "mlx", "config.yaml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	for name, body := range map[string]any{
		"unsafe override": map[string]any{"nodes": map[string]any{node: map[string]any{"install": map[string]any{"runtimes": map[string]any{"mlx": map[string]any{"timeout": "40m"}}}}}},
		"model tier":      map[string]any{"models": map[string]any{"m": map[string]any{"install": map[string]any{}}}},
		"restart":         map[string]any{"defaults": map[string]any{"install": map[string]any{"runtimes": map[string]any{"mlx": map[string]any{"timeout": "40m"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			route := "/zzrouter/v1/providers/mlx/parameters"
			if name == "restart" {
				route += "?restart=affected"
			}
			response := makeAuthRequest(t, server, "PATCH", route, TestAdminKey, body)
			assert.Equal(t, http.StatusBadRequest, response.Code, string(response.Body))
			if name == "unsafe override" {
				assert.Contains(t, string(response.Body), "preflight_failed")
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestInstallPatchCollectsTypedFieldErrors(t *testing.T) {
	errors := installPatchShape(json.RawMessage(`{"runtimes":{"mlx":{"package":"x @ https://bad","indexes":{"primary":"http://example.com/simple"},"only_binary":"yes","surprise":true}}}`), "defaults.install")
	require.Len(t, errors, 4)
	keys := map[string]bool{}
	for _, e := range errors {
		keys[e.Key] = true
	}
	for _, field := range []string{"package", "indexes.primary", "only_binary", "surprise"} {
		assert.True(t, keys["defaults.install.runtimes.mlx."+field], field)
	}
}

func TestInstallPatchChecksOnlyAffectedNodeAndOfflineRefusal(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	apps, err := config.LoadAppsConfig(dir)
	require.NoError(t, err)
	before, ok := apps.LookupApp("mlx")
	require.True(t, ok)
	after, err := cloneService(&before)
	require.NoError(t, err)
	override, err := config.MergeInstallPatch(nil, json.RawMessage(`{"runtimes":{"mlx":{"timeout":"40m"}}}`))
	require.NoError(t, err)
	after.Nodes = map[string]config.NodeSpec{"remote": {Install: override}}
	calls := []string{}
	e := ParamsExecutor{installAuthority: func(_ context.Context, node string, _ config.ServiceConfig) error {
		calls = append(calls, node)
		return fmt.Errorf("worker offline")
	}}
	err = e.checkInstallMutation(t.Context(), &before, &after, map[string]bool{"local": true, "remote": true})
	require.ErrorContains(t, err, "worker offline")
	assert.Equal(t, []string{"remote"}, calls)
}

func TestNodeDeletionClassifiesActualInstallChange(t *testing.T) {
	patch := &paramPatchBody{Nodes: map[string]*nodePatch{"node": nil}}
	cfg := config.ServiceConfig{Nodes: map[string]config.NodeSpec{"node": {Parameters: map[string]string{"threads": "2"}}}}
	assert.False(t, hasInstallPatch(patch, cfg), "parameter reset must remain usable with restart=affected")
	cell := cfg.Nodes["node"]
	cell.Install = &config.InstallConfig{}
	cfg.Nodes["node"] = cell
	assert.True(t, hasInstallPatch(patch, cfg))
}

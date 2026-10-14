package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestRollbackPreparationPreservesPersistedOperatorEdits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("historical recovery requires a nonroot node identity")
	}
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.ApplyParameterPatch("comfyui", func(s *ServiceConfig) error {
		s.Enabled = new(true)
		s.Defaults.Parameters["gpu-reserve-mib"] = "16384"
		s.Description = "operator edit after upgrade"
		return nil
	}))
	path := filepath.Join(dir, "on-demand", "comfyui", "config.yaml")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var live map[string]any
	require.NoError(t, yaml.Unmarshal(body, &live))
	originalInstall := live["install"]
	live["future_recipe"] = map[string]any{"new": true}
	live["runtime"].(map[string]any)["future_spine"] = "new"
	live["install"] = map[string]any{"runtimes": map[string]any{"future": map[string]any{"source": "newer source"}}}
	body, err = yaml.Marshal(live)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0600))
	futureDir := filepath.Join(dir, "on-demand", "future-only")
	require.NoError(t, os.MkdirAll(filepath.Join(futureDir, "assets"), 0700))
	future := []byte("enabled: true\nnewer_unknown_recipe: preserved\n")
	require.NoError(t, os.WriteFile(filepath.Join(futureDir, "config.yaml"), future, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(futureDir, "assets", "workflow.json"), []byte("operator asset"), 0600))
	// Synthetic future receipt: only this descriptor grants historical ownership.
	receiptPath := filepath.Join(dir, ".managed-ownership.json")
	body, err = os.ReadFile(receiptPath)
	require.NoError(t, err)
	var receipt struct {
		Format int                   `json:"format"`
		Files  map[string][][]string `json:"files"`
	}
	require.NoError(t, json.Unmarshal(body, &receipt))
	receipt.Files["on-demand/comfyui/config.yaml"] = append(receipt.Files["on-demand/comfyui/config.yaml"], []string{"future_recipe"}, []string{"runtime", "future_spine"})
	receipt.Files["on-demand/future-only/config.yaml"] = [][]string{{"newer_unknown_recipe"}}
	body, err = json.Marshal(receipt)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(receiptPath, body, 0600))
	_, err = LoadAppsConfig(dir)
	var validation *jsonschema.ValidationError
	require.ErrorAs(t, err, &validation, "fixture must fail the actual strict decoder before repair")
	_, err = templates.PrepareProviders(dir)
	require.NoError(t, err)
	cfg, err := LoadAppsConfig(dir)
	require.NoError(t, err)
	comfy := cfg.GetOnDemand("comfyui")
	require.NotNil(t, comfy)
	assert.True(t, *comfy.Enabled)
	assert.Equal(t, "16384", comfy.Defaults.Parameters["gpu-reserve-mib"])
	assert.Equal(t, "operator edit after upgrade", comfy.Description)
	body, err = os.ReadFile(path)
	require.NoError(t, err)
	live = map[string]any{}
	require.NoError(t, yaml.Unmarshal(body, &live))
	assert.NotContains(t, live, "future_recipe")
	assert.NotContains(t, live["runtime"], "future_spine")
	assert.Equal(t, originalInstall, live["install"], "whole install recipes revert through ordinary reconcile")
	archived, err := os.ReadFile(filepath.Join(futureDir, "config.yaml.rollback"))
	require.NoError(t, err)
	assert.Equal(t, future, archived)
	assert.FileExists(t, filepath.Join(futureDir, "assets", "workflow.json"))
	assert.Nil(t, cfg.GetOnDemand("future-only"))
	live["operator_typo"] = true
	body, err = yaml.Marshal(live)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0600))
	_, err = templates.PrepareProviders(dir)
	require.NoError(t, err)
	_, err = LoadAppsConfig(dir)
	require.ErrorAs(t, err, &validation, "a typo without ownership evidence must still be refused")
}

func TestRollbackPreparationWithoutEvidenceDoesNotGuess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("historical recovery requires a nonroot node identity")
	}
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	path := filepath.Join(dir, "on-demand", "comfyui", "config.yaml")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(body, []byte("future_recipe: do-not-guess\n")...), 0600))
	require.NoError(t, os.Remove(filepath.Join(dir, ".managed-ownership.json")))
	_, err = templates.PrepareProviders(dir)
	require.NoError(t, err)
	_, err = LoadAppsConfig(dir)
	var validation *jsonschema.ValidationError
	require.ErrorAs(t, err, &validation)
	body, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "future_recipe: do-not-guess")
}

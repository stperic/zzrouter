package prov_apps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

const tierVariant = "llama3+long"

func withVariant(svc *config.ServiceConfig) {
	svc.Models[tierVariant] = config.ModelSpec{From: "llama3", Parameters: map[string]string{"only-variant": "v"}}
}

// A variant that changes the launch is its own run, keyed by its own
// name, launched with its base's cells and then its own.
func TestLaunch_VariantIsItsOwnRun(t *testing.T) {
	cfg := &config.AppsConfig{}
	svc := tierServiceConfig()
	withVariant(&svc)
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	cleanupProviderManager(t, m)

	inst, err := m.LaunchInstance(t.Context(), LaunchRequest{Provider: "vllm", Model: tierVariant})
	require.NoError(t, err)
	assert.Equal(t, tierVariant, inst.Model)
	got := inst.Resolved().Parameters
	assert.Equal(t, "tier1", got["ctx-size"], "the base's model cell")
	assert.Equal(t, "v", got["only-variant"], "the variant's own")

	found, ok := m.Instances().GetByModelEndpoint(tierVariant, string(EndpointChat))
	require.True(t, ok)
	assert.Equal(t, inst.ID, found.ID)
	_, ok = m.Instances().GetByModelEndpoint("llama3", string(EndpointChat))
	assert.False(t, ok, "the base has no run of its own")

	// Its base's cell is part of what it launched with.
	reloadWith(t, m, func(svc *config.ServiceConfig) {
		withVariant(svc)
		svc.Models["llama3"] = config.ModelSpec{Parameters: map[string]string{"ctx-size": "changed"}}
	})
	st := m.ParametersStatus(inst)
	assert.Equal(t, instance.ParametersStale, st.State)
	assert.Contains(t, st.Changed, "parameters.ctx-size")

	owner, b, ok := m.Variant("", tierVariant)
	require.True(t, ok)
	assert.Equal(t, "vllm", owner)
	assert.Equal(t, "llama3", b)
}

// A run records where the weights it loads came from, as the catalog
// knows them, so no view has to guess it from the provider. A variant
// loads its base's weights, so it states the base's source.
func TestLaunch_RecordsTheWeightsSource(t *testing.T) {
	cfg := &config.AppsConfig{}
	svc := tierServiceConfig()
	withVariant(&svc)
	require.NoError(t, cfg.AddApp("vllm", svc))
	var asked []string
	m, err := NewProviderAppManager(cfg, WithModelSource(func(_ context.Context, model string) string {
		asked = append(asked, model)
		return "huggingface"
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, m)

	inst, err := m.LaunchInstance(t.Context(), LaunchRequest{Provider: "vllm", Model: tierVariant})
	require.NoError(t, err)

	assert.Equal(t, []string{"llama3"}, asked, "the weights, not the variant's name")
	assert.Equal(t, "huggingface", inst.SourceRepo)
	assert.Equal(t, "huggingface", inst.ToInfo("n").SourceRepo, "every view states it")
}

// A cold launch must check the requested variant before canonicalization
// erases its reserved identity.
func TestLaunch_ColdVariantConflictBeforeCanonicalization(t *testing.T) {
	root := t.TempDir()
	previousRoot, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previousRoot) })
	cfg := &config.AppsConfig{}
	svc := tierServiceConfig()
	withVariant(&svc)
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg, WithModelCanonicalizer(func(string) string { return "llama3" }))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	require.NoError(t, os.WriteFile(filepath.Join(root, strings.ToUpper(tierVariant)+".gguf"), []byte("GGUF"), 0o600))
	for _, name := range []string{tierVariant, strings.ToUpper(tierVariant) + ".gguf", filepath.Join(root, strings.ToUpper(tierVariant)+".gguf")} {
		got, err := m.LaunchInstance(t.Context(), LaunchRequest{Provider: "vllm", Model: name})
		require.ErrorIs(t, err, config.ErrModelNameConflict)
		assert.Nil(t, got)
	}
	assert.Empty(t, m.Instances().List())
}

func TestValidateModel_NoFilesystemAdmission(t *testing.T) {
	root := t.TempDir()
	previousRoot, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previousRoot) })
	cfg := &config.AppsConfig{}
	svc := tierServiceConfig()
	withVariant(&svc)
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	require.NoError(t, os.WriteFile(filepath.Join(root, tierVariant+".gguf"), []byte("GGUF"), 0o600))
	require.NoError(t, m.ValidateModel(t.Context(), tierVariant), "unpublished weights are checked only at cold launch")
	_, err = m.LaunchInstance(t.Context(), LaunchRequest{Provider: "vllm", Model: tierVariant})
	require.ErrorIs(t, err, config.ErrModelNameConflict)
}

func TestLocalModelAdmission_ReusesOnlyTheCheckedOperation(t *testing.T) {
	root := t.TempDir()
	previousRoot, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previousRoot) })
	cfg := &config.AppsConfig{}
	svc := tierServiceConfig()
	withVariant(&svc)
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	admitted, err := m.AdmitLocalModel(t.Context(), tierVariant)
	require.NoError(t, err)

	// Evidence arriving after admission belongs to the next operation.
	require.NoError(t, os.WriteFile(filepath.Join(root, tierVariant+".gguf"), []byte("GGUF"), 0o600))
	_, err = m.AdmitLocalModel(admitted, tierVariant)
	require.NoError(t, err)
	_, _, err = m.buildLaunch(svc, "vllm", "chat", tierVariant, 8100, nil)
	require.NoError(t, err, "command construction must not repeat admission")
	_, err = m.AdmitLocalModel(t.Context(), tierVariant)
	require.ErrorIs(t, err, config.ErrModelNameConflict, "a new operation must check fresh evidence")
	_, err = m.AdmitLocalModel(admitted, tierVariant+".gguf")
	require.ErrorIs(t, err, config.ErrModelNameConflict, "proof only covers the addressed name")
	canceled, cancel := context.WithCancel(admitted)
	cancel()
	_, err = m.AdmitLocalModel(canceled, tierVariant)
	require.ErrorIs(t, err, context.Canceled)
	inst := instance.NewInstance("admission-status", "vllm", tierVariant, 8100, 0, 0)
	inst.Config = instance.Config{Provider: "vllm", Model: tierVariant, Port: 8100}
	status := m.ParametersStatus(inst)
	require.Equal(t, instance.ParametersUnknown, status.State)
	assert.Contains(t, status.Error, "model name conflict")
	require.NoError(t, cfg.UpdateApp("vllm", func(sc *config.ServiceConfig) error {
		variant := sc.Models[tierVariant]
		variant.Parameters = map[string]string{"ctx-size": "changed"}
		sc.Models[tierVariant] = variant
		return nil
	}))
	m.ReloadConfig(cfg)
	_, err = m.AdmitLocalModel(admitted, tierVariant)
	require.ErrorIs(t, err, config.ErrModelNameConflict, "in-place config reload must invalidate proof")

	reloadWith(t, m, withVariant)
	_, err = m.AdmitLocalModel(admitted, tierVariant)
	require.ErrorIs(t, err, config.ErrModelNameConflict, "config replacement requires fresh admission")
}

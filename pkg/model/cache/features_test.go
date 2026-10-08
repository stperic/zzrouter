package cache

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeatureDetailsWorkerRoundTrip(t *testing.T) {
	for _, state := range []string{"present", "available", "unsupported", "unknown", "bad"} {
		original := &CachedModel{Name: "model", Node: "worker", Details: map[string]any{"features": map[string]string{"vision": state}, "wire_endpoints": []string{}}}
		data, err := json.Marshal(original)
		require.NoError(t, err)
		var remote CachedModel
		require.NoError(t, json.Unmarshal(data, &remote))
		want := state
		if state == "bad" {
			want = "unknown"
		}
		assert.Equal(t, want, remote.FeatureState("vision"))
		eps, known := remote.WireEndpoints()
		assert.True(t, known)
		assert.Empty(t, eps)
	}
	var absent *CachedModel
	assert.Equal(t, "unknown", absent.FeatureState("vision"))
	_, known := absent.WireEndpoints()
	assert.False(t, known)
}

func TestLocalVariantFeaturePolicyAndWorkerPropagation(t *testing.T) {
	cfg := variantApps(t, true, map[string]pkgConfig.ModelSpec{"variant": {From: "BASE"}})
	source := map[string]any{"family": "source"}
	items := EnrichLocalItems([]map[string]any{{"name": "base", "node": "worker", "assigned_app": "llamacpp", "source_id": "repo", "details": source}}, cfg, func(provider, model string, d map[string]any) map[string]any {
		assert.Equal(t, "llamacpp", provider)
		out := maps.Clone(d)
		state := "present"
		if model == "variant" {
			state = "available"
		}
		out["features"] = map[string]string{"vision": state}
		return out
	})
	require.Len(t, items, 2)
	assert.NotContains(t, source, "features")
	assert.NotContains(t, items[1], "source_id")
	data, err := json.Marshal(items)
	require.NoError(t, err)
	var remote []map[string]any
	require.NoError(t, json.Unmarshal(data, &remote))
	c := New(Config{AppsConfig: func() *pkgConfig.AppsConfig { return cfg }})
	c.populateModelCacheFromItems(remote)
	base, ok := c.LookupByName("base")
	require.True(t, ok)
	variant, ok := c.LookupByName("variant")
	require.True(t, ok)
	assert.Equal(t, "present", base.FeatureState("vision"))
	assert.Equal(t, "available", variant.FeatureState("vision"))
	assert.Equal(t, "BASE", variant.VariantOf)
	assert.Equal(t, "worker", variant.Node)
	assert.Len(t, c.GetAllModels(), 2)
}

func TestLocalVariantUsesOwningProviderEvidence(t *testing.T) {
	cfg := variantApps(t, true, map[string]pkgConfig.ModelSpec{"variant": {From: "base"}})
	items := EnrichLocalItems([]map[string]any{
		{"name": "base", "node": "worker", "assigned_app": "llamacpp", "details": map[string]any{"vision": true}},
		{"name": "base", "node": "worker", "assigned_app": "other", "details": map[string]any{"vision": false}},
	}, cfg, func(provider, model string, d map[string]any) map[string]any { return maps.Clone(d) })
	require.Len(t, items, 3)
	assert.Equal(t, true, items[2]["details"].(map[string]any)["vision"])
}

func TestLookupTargetKeepsReplicaIdentity(t *testing.T) {
	c := New(Config{})
	models := []*CachedModel{
		{Name: "Weights", SourceID: "vendor/repo", Provider: "llamacpp", Node: "local", Details: map[string]any{"features": map[string]string{"vision": "available"}}},
		{Name: "Weights", SourceID: "vendor/repo", Provider: "llamacpp", Node: "worker", Details: map[string]any{"features": map[string]string{"vision": "present"}}},
		{Name: "Weights", SourceID: "vendor/repo", Provider: "other", Node: "worker"},
	}
	c.updateModelCache(models)
	for _, m := range models {
		for _, name := range []string{"weights", "VENDOR/REPO"} {
			got, err := c.LookupTarget(context.Background(), name, m.Provider, m.Node)
			require.NoError(t, err)
			assert.Same(t, m, got)
		}
	}
	got, err := c.LookupTarget(context.Background(), "Weights", "other", "local")
	require.NoError(t, err)
	assert.Nil(t, got)
	c.updateModelCache(models[:1])
	got, err = c.LookupTarget(context.Background(), "Weights", "llamacpp", "worker")
	require.NoError(t, err)
	assert.Nil(t, got, "refresh removes old replica entries")
	c.mu.Lock()
	c.invalidateLocked()
	assert.Empty(t, c.indexByTarget)
	c.mu.Unlock()
}

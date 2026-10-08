package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

func variantApps(t *testing.T, enabled bool, models map[string]pkgConfig.ModelSpec) *pkgConfig.AppsConfig {
	t.Helper()
	cfg := &pkgConfig.AppsConfig{}
	require.NoError(t, cfg.AddApp("llamacpp", pkgConfig.ServiceConfig{
		Enabled:  &enabled,
		Mode:     "on-demand",
		Protocol: pkgConfig.ProtocolOpenAI,
		Runtime: &pkgConfig.AppRuntimeConfig{PortRange: []int{8080, 8081}, BasePort: 8080,
			Execution: pkgConfig.ExecutionConfig{Type: "cli", Command: "llama-server"}},
		Capabilities: &pkgConfig.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		Models:       models,
	}))
	return cfg
}

func TestExpandVariants_ListsAVariantWhereverItsBaseIs(t *testing.T) {
	cfg := variantApps(t, true, map[string]pkgConfig.ModelSpec{
		"Qwen3.8-27B-Q8_0+agent": {From: "Qwen3.8-27B-Q8_0"},
		"repo+agent":             {From: "unsloth/Qwen3.8-27B-GGUF"}, // an alias, not the catalog's name
		"orphan+agent":           {From: "never-downloaded"},
	})
	models := []*CachedModel{
		{Name: "Qwen3.8-27B-Q8_0", SourceID: "unsloth/Qwen3.8-27B-GGUF", Node: "worker-1", Size: 29, Format: "gguf"},
		{Name: "Qwen3.8-27B-Q8_0", Node: "mac", Provider: "llamacpp"},
		{Name: "Qwen3.8-27B-Q8_0", Node: "other", Provider: "vllm"},
		// Real weights that happen to carry a variant's name keep it.
		{Name: "QWEN3.8-27B-Q8_0+AGENT", Node: "other2"},
		{Name: "Qwen3.8-27B-Q8_0", Node: "other2"},
	}

	got := expandVariants(models, cfg)
	byPlace := map[string]*CachedModel{}
	for _, v := range got {
		byPlace[v.Name+"@"+v.Node] = v
	}
	require.Len(t, got, 2, "%v", byPlace)

	v := byPlace["Qwen3.8-27B-Q8_0+agent@worker-1"]
	require.NotNil(t, v)
	assert.Equal(t, "Qwen3.8-27B-Q8_0", v.VariantOf)
	assert.Equal(t, "llamacpp", v.Provider, "a variant belongs to the provider that defines it")
	assert.Equal(t, "Qwen3.8-27B-Q8_0+agent", v.Model)
	assert.Empty(t, v.SourceID, "the base's aliases stay the base's")
	assert.Equal(t, int64(29), v.Size)
	assert.NotNil(t, byPlace["Qwen3.8-27B-Q8_0+agent@mac"])
	assert.Nil(t, byPlace["Qwen3.8-27B-Q8_0+agent@other"], "another engine's weights")
	assert.Nil(t, byPlace["repo+agent@worker-1"], "a base named by an alias is not matched")
	assert.Nil(t, byPlace["Qwen3.8-27B-Q8_0+agent@other2"], "real weights of that name, in any case, are not shadowed")
	assert.Nil(t, byPlace["orphan+agent@worker-1"], "a variant whose weights are nowhere is not listed")
	assert.Equal(t, "unsloth/Qwen3.8-27B-GGUF", models[0].SourceID, "the base entry is not modified")

	assert.Empty(t, expandVariants(models, variantApps(t, false, map[string]pkgConfig.ModelSpec{
		"Qwen3.8-27B-Q8_0+agent": {From: "Qwen3.8-27B-Q8_0"},
	})), "a disabled provider launches nothing")
	assert.Empty(t, expandVariants(models, nil))
}

// The cache indexes variants like any model, so a lookup by the
// variant's name finds it.
func TestCache_IndexesVariants(t *testing.T) {
	cfg := variantApps(t, true, map[string]pkgConfig.ModelSpec{"Qwen3.8-27B-Q8_0+agent": {From: "Qwen3.8-27B-Q8_0"}})
	mc := New(Config{AppsConfig: func() *pkgConfig.AppsConfig { return cfg }})
	mc.populateModelCacheFromItems([]map[string]any{{"name": "Qwen3.8-27B-Q8_0", "node": "worker-1"}})

	m, ok := mc.LookupByName("Qwen3.8-27B-Q8_0+agent")
	require.True(t, ok)
	assert.Equal(t, "worker-1", m.Node)
	assert.Equal(t, "Qwen3.8-27B-Q8_0", m.VariantOf)
}

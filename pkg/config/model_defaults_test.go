package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// familyConfig is a provider whose release ships a template for one
// family, with every operator tier available to override it.
func familyConfig() ServiceConfig {
	return ServiceConfig{
		Defaults: &AppDefaultsConfig{Parameters: map[string]string{"ctx-size": "8192"}},
		ModelDefaults: map[string]ModelSpec{
			"qwen3.8-*": {Parameters: map[string]string{"chat-template-file": "fixed.jinja", "ctx-size": "32768"}},
		},
	}
}

func TestModelDefaults_SitAboveDefaultsAndReportTheirPattern(t *testing.T) {
	sc := familyConfig()
	got := sc.Resolve("n1", "Qwen3.8-27B-Q8_0").Parameters

	require.Contains(t, got, "chat-template-file")
	assert.Equal(t, "fixed.jinja", got["chat-template-file"].Value)
	assert.Equal(t, TierModelDefault, got["chat-template-file"].Tier)
	assert.Equal(t, "qwen3.8-*", got["chat-template-file"].Pattern)
	assert.Equal(t, "Qwen3.8-27B-Q8_0", got["chat-template-file"].Model)
	// The family's value beats the provider-wide default.
	assert.Equal(t, "32768", got["ctx-size"].Value)
	assert.Equal(t, "model-default", TierModelDefault.String())
}

// Each operator tier must beat the release's family default. A resolver
// that walked model_defaults after any of them would hand the operator's
// own choice back to the release.
func TestModelDefaults_EveryOperatorTierOverrides(t *testing.T) {
	cases := []struct {
		name string
		set  func(*ServiceConfig)
		tier Tier
	}{
		{"model", func(sc *ServiceConfig) {
			sc.Models = map[string]ModelSpec{"Qwen3.8-27B-Q8_0": {Parameters: map[string]string{"chat-template-file": "mine.jinja"}}}
		}, TierModel},
		{"model glob", func(sc *ServiceConfig) {
			sc.Models = map[string]ModelSpec{"qwen*": {Parameters: map[string]string{"chat-template-file": "mine.jinja"}}}
		}, TierModel},
		{"node", func(sc *ServiceConfig) {
			sc.Nodes = map[string]NodeSpec{"n1": {Parameters: map[string]string{"chat-template-file": "mine.jinja"}}}
		}, TierNode},
		{"node x model", func(sc *ServiceConfig) {
			sc.Nodes = map[string]NodeSpec{"n1": {Models: map[string]NodeModelSpec{
				"Qwen3.8-27B-Q8_0": {Parameters: map[string]string{"chat-template-file": "mine.jinja"}},
			}}}
		}, TierNodeModel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := familyConfig()
			tc.set(&sc)
			got := sc.Resolve("n1", "Qwen3.8-27B-Q8_0").Parameters["chat-template-file"]
			assert.Equal(t, "mine.jinja", got.Value)
			assert.Equal(t, tc.tier, got.Tier)
		})
	}
}

// "auto" at an operator tier is how the shipped template is turned off:
// the key resolves to the sentinel and the launch drops the flag.
func TestModelDefaults_AutoTurnsTheReleaseValueOff(t *testing.T) {
	sc := familyConfig()
	sc.Models = map[string]ModelSpec{"Qwen3.8-27B-Q8_0": {Parameters: map[string]string{"chat-template-file": "auto"}}}

	flat := FilterAutoValues(FlattenParameters(sc.Resolve("", "Qwen3.8-27B-Q8_0").Parameters))
	assert.NotContains(t, flat, "chat-template-file")
	assert.Equal(t, "32768", flat["ctx-size"], "only the key set to auto is dropped")
}

// A shipped pattern cannot know whose repository a model came from, so it
// is tried against the last path element too. Operator keys keep the
// full-name rule.
func TestModelDefaults_MatchTheNameAsAnyRegistrySpellsIt(t *testing.T) {
	sc := familyConfig()
	for _, name := range []string{
		"Qwen3.8-27B-Q8_0",
		"qwen3.8-27b-q8_0",
		"Qwen/Qwen3.8-27B",
		"mlx-community/Qwen3.8-27B-4bit",
		"unsloth/Qwen3.8-27B-GGUF#Qwen3.8-27B-Q8_0.gguf",
	} {
		got, ok := sc.Resolve("", name).Parameters["chat-template-file"]
		if assert.True(t, ok, name) {
			assert.Equal(t, TierModelDefault, got.Tier, name)
		}
	}
	for _, name := range []string{"Qwen3-8B-Q4_K_M", "Qwen/Qwen3-8B", "llama-3.1-8b", "qwen3.8", ""} {
		_, ok := sc.Resolve("", name).Parameters["chat-template-file"]
		assert.False(t, ok, "%q is not in the family", name)
	}

	// The last-element retry is model_defaults' rule alone.
	sc.Models = map[string]ModelSpec{"qwen3.8-*": {Parameters: map[string]string{"threads": "8"}}}
	_, ok := sc.Resolve("", "Qwen/Qwen3.8-27B").Parameters["threads"]
	assert.False(t, ok, "an operator glob still matches the full name only")
}

func TestModelDefaults_EndpointOverlayApplies(t *testing.T) {
	sc := ServiceConfig{ModelDefaults: map[string]ModelSpec{
		"qwen3.8-*": {Endpoints: map[string]EndpointOverlay{"embeddings": {Parameters: map[string]string{"pooling": "last"}}}},
	}}
	got := sc.ResolveEndpoint("", "Qwen3.8-Embed-8B", "embeddings").Parameters["pooling"]
	assert.Equal(t, "last", got.Value)
	assert.Equal(t, TierModelDefault, got.Tier)
	assert.Equal(t, "embeddings", got.Endpoint)
	assert.NotContains(t, sc.Resolve("", "Qwen3.8-Embed-8B").Parameters, "pooling")
}

func TestModelDefaults_AreParameterSites(t *testing.T) {
	sc := familyConfig()
	var paths []string
	for _, site := range sc.ParameterSites() {
		paths = append(paths, site.Path)
	}
	assert.Contains(t, paths, "model_defaults.qwen3.8-*.parameters")
}

func TestTierOrder_ModelDefaultSitsBetweenDefaultAndModel(t *testing.T) {
	assert.Less(t, TierDefault, TierModelDefault)
	assert.Less(t, TierModelDefault, TierModel)
}

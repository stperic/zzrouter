package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveEndpoint covers the 8 merge points: each of the 4 tiers
// supplies a base value AND an embeddings-overlay value for the same
// key, with higher tiers overriding lower. Verifies endpoint provenance
// is set when the value came from an overlay.
func TestResolveEndpoint_AllTiersOverlay(t *testing.T) {
	sc := ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Parameters: map[string]string{"k_default_base": "d_base", "k_default_ov": "d_base"},
			Endpoints: map[string]EndpointOverlay{
				"embeddings": {Parameters: map[string]string{"k_default_ov": "d_ov"}},
			},
		},
		Models: map[string]ModelSpec{
			"m1": {
				Parameters: map[string]string{"k_model_base": "m_base", "k_model_ov": "m_base"},
				Endpoints: map[string]EndpointOverlay{
					"embeddings": {Parameters: map[string]string{"k_model_ov": "m_ov"}},
				},
			},
		},
		Nodes: map[string]NodeSpec{
			"n1": {
				Parameters: map[string]string{"k_node_base": "n_base", "k_node_ov": "n_base"},
				Endpoints: map[string]EndpointOverlay{
					"embeddings": {Parameters: map[string]string{"k_node_ov": "n_ov"}},
				},
				Models: map[string]NodeModelSpec{
					"m1": {
						Parameters: map[string]string{"k_nm_base": "nm_base", "k_nm_ov": "nm_base"},
						Endpoints: map[string]EndpointOverlay{
							"embeddings": {Parameters: map[string]string{"k_nm_ov": "nm_ov"}},
						},
					},
				},
			},
		},
	}

	got := sc.ResolveEndpoint("n1", "m1", "embeddings")

	cases := []struct {
		key      string
		want     string
		wantTier Tier
		wantOver string // "embeddings" if from overlay, "" if from base
	}{
		{"k_default_base", "d_base", TierDefault, ""},
		{"k_default_ov", "d_ov", TierDefault, "embeddings"},
		{"k_model_base", "m_base", TierModel, ""},
		{"k_model_ov", "m_ov", TierModel, "embeddings"},
		{"k_node_base", "n_base", TierNode, ""},
		{"k_node_ov", "n_ov", TierNode, "embeddings"},
		{"k_nm_base", "nm_base", TierNodeModel, ""},
		{"k_nm_ov", "nm_ov", TierNodeModel, "embeddings"},
	}
	for _, tc := range cases {
		rv, ok := got.Parameters[tc.key]
		assert.True(t, ok, "missing key %q", tc.key)
		assert.Equal(t, tc.want, rv.Value, "value for %q", tc.key)
		assert.Equal(t, tc.wantTier, rv.Tier, "tier for %q", tc.key)
		assert.Equal(t, tc.wantOver, rv.Endpoint, "endpoint for %q", tc.key)
	}
}

// TestResolveEndpoint_HigherTierOverlayBeatsLowerTierBase is the
// load-bearing precedence claim: model.endpoints[E] still beats
// defaults.parameters, and node.endpoints[E] beats model.endpoints[E].
func TestResolveEndpoint_PrecedenceAcrossTiers(t *testing.T) {
	sc := ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Parameters: map[string]string{"shared": "d_base"},
			Endpoints: map[string]EndpointOverlay{
				"embeddings": {Parameters: map[string]string{"shared": "d_ov"}},
			},
		},
		Models: map[string]ModelSpec{
			"m1": {Parameters: map[string]string{"shared": "m_base"}},
		},
		Nodes: map[string]NodeSpec{
			"n1": {
				Parameters: map[string]string{"shared": "n_base"},
				Endpoints: map[string]EndpointOverlay{
					"embeddings": {Parameters: map[string]string{"shared": "n_ov"}},
				},
			},
		},
	}

	got := sc.ResolveEndpoint("n1", "m1", "embeddings")
	rv := got.Parameters["shared"]
	assert.Equal(t, "n_ov", rv.Value)
	assert.Equal(t, TierNode, rv.Tier)
	assert.Equal(t, "embeddings", rv.Endpoint)
}

// TestResolveEndpoint_UnknownEndpointMissesAllOverlays confirms a typo
// or never-seen endpoint silently passes through to chat-equivalent
// resolution without erroring; validation lives at the dispatch boundary.
func TestResolveEndpoint_UnknownEndpoint(t *testing.T) {
	sc := ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Parameters: map[string]string{"k": "base"},
			Endpoints: map[string]EndpointOverlay{
				"embeddings": {Parameters: map[string]string{"k": "embed_ov"}},
			},
		},
	}

	got := sc.ResolveEndpoint("", "", "moderations")
	assert.Equal(t, "base", got.Parameters["k"].Value)
	assert.Equal(t, "", got.Parameters["k"].Endpoint)
}

// TestResolveEndpoint_EmptyDefaultsToChat keeps the legacy 2-arg
// Resolve identical to ResolveEndpoint(_, _, "chat").
func TestResolveEndpoint_EmptyEndpointEqualsChat(t *testing.T) {
	sc := ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Parameters: map[string]string{"k": "base"},
			Endpoints: map[string]EndpointOverlay{
				"chat": {Parameters: map[string]string{"k": "chat_ov"}},
			},
		},
	}

	gotEmpty := sc.ResolveEndpoint("", "", "")
	gotChat := sc.ResolveEndpoint("", "", "chat")
	gotLegacy := sc.Resolve("", "")

	assert.Equal(t, "chat_ov", gotEmpty.Parameters["k"].Value)
	assert.Equal(t, gotChat.Parameters["k"], gotEmpty.Parameters["k"])
	assert.Equal(t, gotChat.Parameters["k"], gotLegacy.Parameters["k"])
}

// TestResolveEndpoint_OverlayEnvironmentToo proves the overlay applies
// to environment vars too — same precedence, same provenance.
func TestResolveEndpoint_OverlayCoversEnvironment(t *testing.T) {
	sc := ServiceConfig{
		Defaults: &AppDefaultsConfig{
			Environment: map[string]string{"E": "base"},
			Endpoints: map[string]EndpointOverlay{
				"embeddings": {Environment: map[string]string{"E": "ov"}},
			},
		},
	}

	got := sc.ResolveEndpoint("", "", "embeddings")
	rv := got.Environment["E"]
	assert.Equal(t, "ov", rv.Value)
	assert.Equal(t, "embeddings", rv.Endpoint)
}

// TestIsEndpointAware checks the auto-detection: any endpoints[E] block
// at any tier turns the provider endpoint-aware; absence keeps it
// single-instance-per-model.
func TestIsEndpointAware(t *testing.T) {
	bare := ServiceConfig{Defaults: &AppDefaultsConfig{Parameters: map[string]string{"k": "v"}}}
	assert.False(t, bare.IsEndpointAware())

	def := ServiceConfig{Defaults: &AppDefaultsConfig{
		Endpoints: map[string]EndpointOverlay{"embeddings": {Parameters: map[string]string{"x": "y"}}},
	}}
	assert.True(t, def.IsEndpointAware())

	mod := ServiceConfig{Models: map[string]ModelSpec{
		"m1": {Endpoints: map[string]EndpointOverlay{"embeddings": {}}},
	}}
	assert.True(t, mod.IsEndpointAware())

	node := ServiceConfig{Nodes: map[string]NodeSpec{
		"n1": {Endpoints: map[string]EndpointOverlay{"embeddings": {}}},
	}}
	assert.True(t, node.IsEndpointAware())

	nm := ServiceConfig{Nodes: map[string]NodeSpec{
		"n1": {Models: map[string]NodeModelSpec{
			"m1": {Endpoints: map[string]EndpointOverlay{"embeddings": {}}},
		}},
	}}
	assert.True(t, nm.IsEndpointAware())
}

// A glob scores below zero when it has few literal characters, and the
// best-match search used to start at -1, so "qwen*" never matched at any
// tier and its parameters were silently dropped.
func TestMatchSpec_ShortGlobMatches(t *testing.T) {
	sc := ServiceConfig{
		Models: map[string]ModelSpec{"qwen*": {Parameters: map[string]string{"threads": "8"}}},
		Nodes: map[string]NodeSpec{"n1": {Models: map[string]NodeModelSpec{
			"q*": {Parameters: map[string]string{"batch-size": "512"}},
		}}},
	}
	got := sc.Resolve("n1", "Qwen3-8B").Parameters
	assert.Equal(t, "8", got["threads"].Value)
	assert.Equal(t, TierModel, got["threads"].Tier)
	assert.Equal(t, "512", got["batch-size"].Value)
	assert.Equal(t, TierNodeModel, got["batch-size"].Tier)
}

// Two equally specific globs were chosen by map order.
func TestMatchSpec_TieIsDeterministic(t *testing.T) {
	specs := map[string]ModelSpec{
		"qwen*-a": {Parameters: map[string]string{"k": "a"}},
		"qwen*-b": {Parameters: map[string]string{"k": "b"}},
		"qwe*n-a": {Parameters: map[string]string{"k": "c"}},
	}
	for range 50 {
		_, spec, ok := matchSpec(specs, "qwen-a")
		if assert.True(t, ok) {
			// "qwe*n-a" and "qwen*-a" score alike; '*' sorts first.
			assert.Equal(t, "c", spec.Parameters["k"])
		}
	}
}

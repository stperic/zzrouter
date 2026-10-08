package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every place a key can be set is a site: four tiers, each with its
// endpoint overlays. Missing one would let a reference hide from a
// "who uses this?" question.
func TestParameterSites_CoversEveryTierAndOverlay(t *testing.T) {
	p := func(v string) map[string]string { return map[string]string{"k": v} }
	ov := func(v string) map[string]EndpointOverlay {
		return map[string]EndpointOverlay{"emb": {Parameters: p(v)}}
	}
	sc := &ServiceConfig{
		Defaults: &AppDefaultsConfig{Parameters: p("d"), Endpoints: ov("de")},
		Models:   map[string]ModelSpec{"m": {Parameters: p("m"), Endpoints: ov("me")}},
		Nodes: map[string]NodeSpec{"n": {
			Parameters: p("n"), Endpoints: ov("ne"),
			Models: map[string]NodeModelSpec{"m": {Parameters: p("nm"), Endpoints: ov("nme")}},
		}},
	}
	got := map[string]string{}
	endpoints := map[string]string{}
	for _, site := range sc.ParameterSites() {
		got[site.Path] = site.Parameters["k"]
		endpoints[site.Path] = site.Endpoint
	}
	assert.Equal(t, map[string]string{
		"defaults.parameters":                       "d",
		"defaults.endpoints.emb.parameters":         "de",
		"models.m.parameters":                       "m",
		"models.m.endpoints.emb.parameters":         "me",
		"nodes.n.parameters":                        "n",
		"nodes.n.endpoints.emb.parameters":          "ne",
		"nodes.n.models.m.parameters":               "nm",
		"nodes.n.models.m.endpoints.emb.parameters": "nme",
	}, got)
	assert.Equal(t, "emb", endpoints["nodes.n.models.m.endpoints.emb.parameters"])
	assert.Empty(t, endpoints["models.m.parameters"])
}

func TestParameterSites_SkipsEmptyAndOrders(t *testing.T) {
	sc := &ServiceConfig{
		Models: map[string]ModelSpec{"b": {Parameters: map[string]string{"k": "1"}}, "a": {Parameters: map[string]string{"k": "1"}}, "empty": {}},
	}
	sites := sc.ParameterSites()
	if assert.Len(t, sites, 2) {
		assert.Equal(t, "models.a.parameters", sites[0].Path)
		assert.Equal(t, "models.b.parameters", sites[1].Path)
	}
	assert.Empty(t, (&ServiceConfig{}).ParameterSites())
}

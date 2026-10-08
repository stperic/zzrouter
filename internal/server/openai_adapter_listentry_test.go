package server

import (
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stretchr/testify/require"
)

// TestOpenAIAdapter_ListEntry_QualifiesClusterNodeID pins the
// /v1/models id-format contract for cluster-resident models: the id
// is `name@node` so the same model on multiple nodes surfaces as
// distinct, directly-routable entries (the @suffix form is already
// accepted by the routing layer).
func TestOpenAIAdapter_ListEntry_QualifiesClusterNodeID(t *testing.T) {
	a := NewOpenAIAdapter(func() *pkgConfig.AppsConfig { return nil })
	m := &cache.CachedModel{
		Name:     "smollm:135m",
		Node:     "macbook-pro",
		Provider: "ollama",
		IsCloud:  false,
	}
	got := a.ListEntry(m)
	obj, ok := got.(OpenAIModelObject)
	require.True(t, ok, "ListEntry must return OpenAIModelObject")
	require.Equal(t, "smollm:135m@macbook-pro", obj.ID)
	require.NotNil(t, obj.ZZRouter)
	require.Equal(t, "macbook-pro", obj.ZZRouter.Node)
}

// TestOpenAIAdapter_ListEntry_BareIDForCloud locks in the cloud
// exception: cloud entries are brokered through the coord but not
// served by it. Qualifying with @<coord-hostname> would make the id
// unstable across coord renames and misleads agents into thinking
// the cloud relay is node-bound.
func TestOpenAIAdapter_ListEntry_BareIDForCloud(t *testing.T) {
	a := NewOpenAIAdapter(func() *pkgConfig.AppsConfig { return nil })
	m := &cache.CachedModel{
		Name:     "openai/text-embedding-3-small",
		Node:     "macbook-pro",
		Provider: "openai",
		IsCloud:  true,
	}
	got := a.ListEntry(m)
	obj, ok := got.(OpenAIModelObject)
	require.True(t, ok)
	require.Equal(t, "openai/text-embedding-3-small", obj.ID, "cloud entries must keep bare name")
}

// TestOpenAIAdapter_ListEntry_BareIDWhenNoNode covers the legacy
// unattributed case (cache entries that predate node tracking, or
// loopback-only setups). Qualifying with "@" alone would emit a
// trailing-@ id that no router accepts.
func TestOpenAIAdapter_ListEntry_BareIDWhenNoNode(t *testing.T) {
	a := NewOpenAIAdapter(func() *pkgConfig.AppsConfig { return nil })
	m := &cache.CachedModel{Name: "stray", Provider: "vllm"}
	got := a.ListEntry(m)
	obj, ok := got.(OpenAIModelObject)
	require.True(t, ok)
	require.Equal(t, "stray", obj.ID)
}

// TestQualifyID_Helper exhaustively pins the qualifyID rule the
// /v1/models handler uses for SourceID alias entries (which the
// adapter doesn't compute directly).
func TestQualifyID_Helper(t *testing.T) {
	cases := []struct {
		name string
		in   *cache.CachedModel
		nm   string
		want string
	}{
		{"cluster", &cache.CachedModel{Node: "w1", IsCloud: false}, "x", "x@w1"},
		{"cloud_with_coord_node", &cache.CachedModel{Node: "coord", IsCloud: true}, "x", "x"},
		{"no_node", &cache.CachedModel{}, "x", "x"},
		{"nil_model", nil, "x", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, qualifyID(tc.nm, tc.in))
		})
	}
}

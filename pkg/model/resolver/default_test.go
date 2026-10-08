package resolver

import (
	"context"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stretchr/testify/require"
)

func TestDefaultReplicaSelection(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		replicas             []*cache.CachedModel
		hint, node, provider string
		ambiguous            bool
	}{
		{name: "worker", replicas: []*cache.CachedModel{{Name: "m", Node: "worker", Provider: "llamacpp"}}, node: "worker", provider: "llamacpp"},
		{name: "local wins", replicas: []*cache.CachedModel{{Name: "m", Node: "worker", Provider: "llamacpp"}, {Name: "m", Node: "coord", Provider: "llamacpp"}}, provider: "llamacpp"},
		{name: "two workers", replicas: []*cache.CachedModel{{Name: "m", Node: "two", Provider: "llamacpp"}, {Name: "m", Node: "one", Provider: "llamacpp"}}, ambiguous: true},
		{name: "cloud", replicas: []*cache.CachedModel{{Name: "m", Node: "coord", Provider: "openai", IsCloud: true}}, provider: "openai"},
		{name: "unknown"},
		{name: "explicit uncatalogued", hint: "worker", node: "worker"},
		{name: "same node two providers", replicas: []*cache.CachedModel{{Name: "m", Node: "worker", Provider: "llamacpp"}, {Name: "m", Node: "worker", Provider: "vllm"}}, node: "worker", provider: "llamacpp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig{
				Parse:    func(s string) (string, string, string) { return s, "", "" },
				Nodename: func() string { return "coord" }, IsLocalNode: func(s string) bool { return s == "coord" || s == "" || s == "localhost" }, CanRoute: func() bool { return true },
				Replicas: func(_ context.Context, name, provider, node string) ([]*cache.CachedModel, error) {
					require.Equal(t, "m", name)
					require.Equal(t, tc.hint, node)
					return tc.replicas, nil
				},
			}
			got, err := NewDefault(cfg).Resolve(t.Context(), Reference{Model: "m", Node: tc.hint})
			if tc.ambiguous {
				var wire httperr.Error
				require.ErrorAs(t, err, &wire)
				require.Equal(t, 409, wire.Status)
				require.Equal(t, string(httperr.CodeModelNodeAmbiguous), wire.Code)
				require.Contains(t, wire.Message, "m@one, m@two")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.node, got.Node)
			require.Equal(t, tc.provider, got.Provider)
		})
	}
}

func TestDefaultCarriesProviderAndSharedVariant(t *testing.T) {
	cfg := DefaultConfig{Parse: func(s string) (string, string, string) { require.Equal(t, "alias", s); return s, "", "" }, Nodename: func() string { return "coord" }, IsLocalNode: func(s string) bool { return s == "coord" }, CanRoute: func() bool { return true }, Replicas: func(_ context.Context, name, provider, node string) ([]*cache.CachedModel, error) {
		require.Equal(t, "alias", name)
		require.Equal(t, "llamacpp", provider)
		require.Equal(t, "worker", node)
		return []*cache.CachedModel{{Name: "base+chat", Model: "base", Provider: provider, Node: node}}, nil
	}}
	got, err := NewDefault(cfg).Resolve(t.Context(), Reference{Model: "alias", Provider: "llamacpp", Node: "worker"})
	require.NoError(t, err)
	require.Equal(t, "base", got.ModelName)
	require.Equal(t, "base+chat", got.RequestModel)
	require.Equal(t, "llamacpp", got.Provider)
	require.Equal(t, "worker", got.Node)
}

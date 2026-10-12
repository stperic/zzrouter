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
				Replicas: func(_ context.Context, name, provider, node, _ string, _ ...cache.ReplicaSelection) ([]*cache.CachedModel, error) {
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
	cfg := DefaultConfig{Parse: func(s string) (string, string, string) { require.Equal(t, "alias", s); return s, "", "" }, Nodename: func() string { return "coord" }, IsLocalNode: func(s string) bool { return s == "coord" }, CanRoute: func() bool { return true }, Replicas: func(_ context.Context, name, provider, node, _ string, _ ...cache.ReplicaSelection) ([]*cache.CachedModel, error) {
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

func TestInferenceBindingContract(t *testing.T) {
	for _, tc := range []struct {
		name, node                       string
		inference, exact, warm, populate bool
		registry, source                 string
	}{
		{name: "local cold", node: "coord", inference: true, populate: true},
		{name: "worker cold", node: "worker", inference: true, populate: true},
		{name: "local warm", node: "coord", inference: true, warm: true},
		{name: "unpinned warm", inference: true, warm: true},
		{name: "remote", node: "remote", inference: true},
		{name: "management", node: "coord"},
		{name: "exact", node: "coord", inference: true, exact: true},
		{name: "registry", node: "coord", inference: true, registry: "hf"},
		{name: "source", node: "coord", inference: true, source: "org/m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, warms := 0, 0
			localName := "coord"
			if tc.name == "worker cold" {
				localName = "worker"
			}
			cfg := DefaultConfig{Parse: func(s string) (string, string, string) { return s, "", "" }, Nodename: func() string { return localName }, IsLocalNode: func(s string) bool { return s == "" || s == localName }, CanRoute: func() bool { return localName == "coord" },
				WarmLocalIdentity: func(model, provider string) (string, string, bool) {
					warms++
					require.Equal(t, "m", model)
					return "m", "gateway", tc.warm
				},
				Replicas: func(_ context.Context, name, provider, node, registry string, options ...cache.ReplicaSelection) ([]*cache.CachedModel, error) {
					calls++
					require.Equal(t, tc.populate, options[0].Populate)
					return nil, nil
				},
			}
			got, err := NewDefault(cfg).Resolve(t.Context(), Reference{Model: "m", Node: tc.node, Inference: tc.inference, Exact: tc.exact, Registry: tc.registry, SourceID: tc.source})
			require.NoError(t, err)
			require.Equal(t, 1, calls, "one authoritative replica selection")
			if tc.warm {
				require.Equal(t, "gateway", got.Provider)
				require.Nil(t, got.Replica)
				require.Equal(t, "m", got.RequestModel)
			}
			eligible := tc.inference && !tc.exact && tc.registry == "" && tc.source == "" && (tc.node == "" || tc.node == localName)
			require.Equal(t, map[bool]int{true: 1, false: 0}[eligible], warms)
			if tc.name == "remote" {
				require.Equal(t, "remote", got.Node)
			}
		})
	}
}

func TestInferenceBindingContractCatalogAuthority(t *testing.T) {
	for _, provider := range []string{"", "declared"} {
		for _, ambiguous := range []bool{false, true} {
			t.Run(provider+map[bool]string{false: "/unique", true: "/ambiguous"}[ambiguous], func(t *testing.T) {
				replica := &cache.CachedModel{Name: "base", Model: "base", SourceRepo: "hf", SourceID: "org/base", Node: "coord", Provider: provider}
				cfg := DefaultConfig{Parse: func(s string) (string, string, string) { return s, "", "" }, Nodename: func() string { return "coord" }, IsLocalNode: func(s string) bool { return s == "coord" || s == "" }, CanRoute: func() bool { return true },
					WarmLocalIdentity: func(model, provider string) (string, string, bool) {
						if model == "alias" {
							return "", "", false
						}
						return model, "warm", true
					},
					Replicas: func(context.Context, string, string, string, string, ...cache.ReplicaSelection) ([]*cache.CachedModel, error) {
						if ambiguous {
							other := *replica
							other.SourceID = "other/base"
							return []*cache.CachedModel{replica, &other}, nil
						}
						return []*cache.CachedModel{replica}, nil
					},
				}
				got, err := NewDefault(cfg).Resolve(t.Context(), Reference{Model: "alias", Inference: true})
				if ambiguous {
					var wire httperr.Error
					require.ErrorAs(t, err, &wire)
					require.Equal(t, 409, wire.Status)
					return
				}
				require.NoError(t, err)
				require.Same(t, replica, got.Replica)
				require.Equal(t, "base", got.ModelName)
				require.Equal(t, "base", got.RequestModel)
				want := provider
				if want == "" {
					want = "warm"
				}
				require.Equal(t, want, got.Provider)
			})
		}
	}
}

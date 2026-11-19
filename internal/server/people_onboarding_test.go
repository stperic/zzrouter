package server

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func TestObservedOnboardingReadiness(t *testing.T) {
	local := func(state prov_apps.LifecycleState) *mockClusterState {
		return &mockClusterState{localApps: []prov_apps.LocalProviderInfo{{Key: "ollama", Name: "Ollama", Node: "local", State: state}}}
	}
	worker := func(status mesh.EndpointStatus, observed bool) *mockClusterState {
		endpoint := &mesh.Endpoint{NodeName: "worker", Status: status, Snapshot: mesh.EndpointSnapshot{HealthReport: mesh.HealthReport{
			Apps: []prov_apps.LocalProviderInfo{{Key: "ollama", Node: "ignored-wire-node", State: prov_apps.StateLive}},
		}}}
		if observed {
			endpoint.Snapshot.CollectedAt = utils.Now()
		}
		return &mockClusterState{endpoints: []*mesh.Endpoint{endpoint}}
	}
	model := func(node, provider string, incomplete bool) []*cache.CachedModel {
		return []*cache.CachedModel{{Name: "qwen", Node: node, Provider: provider, Incomplete: incomplete}}
	}
	for _, tc := range []struct {
		name          string
		state         ClusterState
		models        []*cache.CachedModel
		providerReady bool
		modelReady    bool
	}{
		{name: "unavailable inventory"},
		{name: "dormant provider", state: local(prov_apps.StateDormant), models: model("local", "ollama", false)},
		{name: "installing provider", state: local(prov_apps.StateInstalling), models: model("local", "ollama", false)},
		{name: "unknown provider", state: local(""), models: model("local", "ollama", false)},
		{name: "provider without published models", state: local(prov_apps.StateLive), providerReady: true},
		{name: "complete local model", state: local(prov_apps.StateLive), models: model("local", "ollama", false), providerReady: true, modelReady: true},
		{name: "incomplete weights", state: local(prov_apps.StateLive), models: model("local", "ollama", true), providerReady: true},
		{name: "different node", state: local(prov_apps.StateLive), models: model("worker", "ollama", false), providerReady: true},
		{name: "different provider", state: local(prov_apps.StateLive), models: model("local", "vllm", false), providerReady: true},
		{name: "display name is not a key", state: local(prov_apps.StateLive), models: model("local", "Ollama", false), providerReady: true},
		{name: "cloud availability", state: local(prov_apps.StateCloudAvailable), models: model("local", "ollama", false), providerReady: true, modelReady: true},
		{name: "healthy observed worker", state: worker(mesh.StatusUp, true), models: model("worker", "ollama", false), providerReady: true, modelReady: true},
		{name: "missing authenticated observation", state: worker(mesh.StatusUp, false), models: model("worker", "ollama", false)},
		{name: "unknown worker", state: worker(mesh.StatusUnknown, true), models: model("worker", "ollama", false)},
		{name: "down worker retained inventory", state: worker(mesh.StatusDown, true), models: model("worker", "ollama", false)},
		{name: "degraded worker retained inventory", state: worker(mesh.StatusDegraded, true), models: model("worker", "ollama", false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providerReady, modelReady := observedOnboardingReadiness(tc.state, tc.models, "")
			require.Equal(t, tc.providerReady, providerReady)
			require.Equal(t, tc.modelReady, modelReady)
		})
	}
}

func TestOnboardingReadinessStaysOnSelectedComputer(t *testing.T) {
	state := &mockClusterState{localApps: []prov_apps.LocalProviderInfo{{Key: "ollama", Node: "coordinator", State: prov_apps.StateLive}}, endpoints: []*mesh.Endpoint{
		{NodeName: "worker", Status: mesh.StatusUp, Snapshot: mesh.EndpointSnapshot{CollectedAt: utils.Now(), HealthReport: mesh.HealthReport{Apps: []prov_apps.LocalProviderInfo{{Key: "vllm", State: prov_apps.StateLive}}}}},
	}}
	localModels := []*cache.CachedModel{{Name: "qwen", Node: "coordinator", Provider: "ollama"}}
	providerReady, modelReady := observedOnboardingReadiness(state, localModels, "worker")
	require.True(t, providerReady)
	require.False(t, modelReady, "coordinator model cannot complete worker setup")
	workerModels := append(localModels, &cache.CachedModel{Name: "qwen", Node: "worker", Provider: "vllm"})
	providerReady, modelReady = observedOnboardingReadiness(state, workerModels, "worker")
	require.True(t, providerReady)
	require.True(t, modelReady)
	state.endpoints[0].Status = mesh.StatusDown
	providerReady, modelReady = observedOnboardingReadiness(state, workerModels, "worker")
	require.False(t, providerReady)
	require.False(t, modelReady)
	providerReady, modelReady = observedOnboardingReadiness(state, workerModels, "missing")
	require.False(t, providerReady)
	require.False(t, modelReady)
}

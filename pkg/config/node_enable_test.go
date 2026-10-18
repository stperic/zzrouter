package config

import (
	"os"
	"path/filepath"
	"testing"

	configschema "github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func enabledFor(t *testing.T, dir, node, provider string) bool {
	t.Helper()
	store, err := newAppsConfigStore(dir, node)
	require.NoError(t, err)
	sc, ok := store.Config().LookupApp(provider)
	require.True(t, ok)
	return sc.IsEnabled()
}

func declaredVLLM(t *testing.T, dir string) OnDemandProvider {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "on-demand", "vllm", "config.yaml"))
	require.NoError(t, err)
	var provider OnDemandProvider
	require.NoError(t, yaml.Unmarshal(body, &provider))
	return provider
}

func TestNodeEnableChoiceOverridesProviderDefault(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	require.False(t, enabledFor(t, dir, "worker-a", "vllm"), "the template ships vllm disabled")

	store, err := newAppsConfigStore(dir, "coordinator")
	require.NoError(t, err)
	_, err = store.SetProviderEnabled("vllm", "worker-a", true)
	require.NoError(t, err)
	require.True(t, enabledFor(t, dir, "worker-a", "vllm"))
	require.False(t, enabledFor(t, dir, "worker-b", "vllm"), "another node keeps the default")
	require.False(t, enabledFor(t, dir, "coordinator", "vllm"))
	declared := declaredVLLM(t, dir)
	require.False(t, *declared.Enabled, "the default is untouched")
	require.True(t, *declared.Nodes["worker-a"].Enabled)

	// A node's choice holds against a later default, both ways.
	_, err = store.SetProviderEnabled("vllm", "", true)
	require.NoError(t, err)
	_, err = store.SetProviderEnabled("vllm", "worker-a", false)
	require.NoError(t, err)
	require.True(t, enabledFor(t, dir, "worker-b", "vllm"))
	require.False(t, enabledFor(t, dir, "worker-a", "vllm"))
}

func TestNodeEnableChoiceMatchingTheNodeWritesNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := newAppsConfigStore(dir, "coordinator")
	require.NoError(t, err)
	_, err = store.SetProviderEnabled("vllm", "", true)
	require.NoError(t, err)
	notified := 0
	store.OnChange("count", func(*AppsConfig) ReloadDisposition {
		notified++
		return DispositionApplied
	})
	changed, err := store.SetProviderEnabled("vllm", "worker-a", true)
	require.NoError(t, err)
	require.False(t, changed)
	require.Zero(t, notified)
	require.Empty(t, declaredVLLM(t, dir).Nodes, "the node already runs with the default")
}

// The store edits declared values, so a mutation on a node with its own
// choice never writes that choice over the default.
func TestNodeBoundStoreSavesDeclaredEnable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	owner, err := newAppsConfigStore(dir, "coordinator")
	require.NoError(t, err)
	_, err = owner.SetProviderEnabled("vllm", "worker-a", true)
	require.NoError(t, err)

	worker, err := newAppsConfigStore(dir, "worker-a")
	require.NoError(t, err)
	require.NoError(t, worker.SetProviderPinnedVersion("vllm", "0.11.0"))
	declared := declaredVLLM(t, dir)
	require.False(t, *declared.Enabled)
	require.Equal(t, "0.11.0", declared.PinnedVersion)
	require.True(t, enabledFor(t, dir, "worker-a", "vllm"))
}

func TestNodeEnableChoiceSurvivesItsLastParameter(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := newAppsConfigStore(dir, "coordinator")
	require.NoError(t, err)
	_, err = store.SetProviderEnabled("vllm", "worker-a", true)
	require.NoError(t, err)
	require.NoError(t, store.ApplyParameterPatch("vllm", func(sc *ServiceConfig) error {
		DeleteParameter(sc.Nodes["worker-a"].Parameters, "max-model-len")
		CleanupNodeSpecFor(sc, "worker-a")
		return nil
	}))
	require.True(t, enabledFor(t, dir, "worker-a", "vllm"))
}

func TestProvidersWithoutANodeTierKeepTheirOwnEnable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := newAppsConfigStore(dir, "coordinator")
	require.NoError(t, err)
	_, err = store.SetProviderEnabled("openai", "coordinator", true)
	require.NoError(t, err)
	require.True(t, store.Config().GetCloud("openai").IsEnabled())
	require.True(t, enabledFor(t, dir, "worker-a", "openai"))
}

func TestNodeEnableChoiceLoadsStrictly(t *testing.T) {
	require.NoError(t, configschema.ValidateOnDemand([]byte(nodeProviderBody+"nodes:\n  worker-a:\n    enabled: true\n")))
	require.Error(t, configschema.ValidateOnDemand([]byte(nodeProviderBody+"nodes:\n  worker-a:\n    enabled: \"yes\"\n")))
	external := "protocol: openai\nruntime:\n  endpoint: http://127.0.0.1:8080\nnodes:\n  worker-a:\n    enabled: false\n"
	require.NoError(t, configschema.ValidateExternal([]byte(external)))
}

// Wire endpoints are checked whenever any node runs the provider, not only
// when the default enables it.
func TestWireEndpointsValidatedWhenOnlyANodeEnables(t *testing.T) {
	off, on := false, true
	provider := ExternalProvider{
		Name:         "remote",
		Protocol:     ProtocolOpenAI,
		Enabled:      &off,
		Capabilities: &AppCapabilities{WireEndpoints: []string{"messages_compat"}},
		Runtime:      ExternalRuntime{Endpoint: "http://127.0.0.1:8080"},
	}
	require.NoError(t, provider.Validate(), "a provider no node runs is not checked")
	provider.Nodes = map[string]NodeSpec{"worker-a": {Enabled: &on}}
	require.Error(t, provider.Validate())
}

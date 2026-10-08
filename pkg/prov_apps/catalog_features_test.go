package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogFeatureFilesAndServingSnapshot(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t)
	source := map[string]any{"family": "test"}
	state := func(model string) string {
		return m.ModelFeatureDetails("vllm", model, source)["features"].(map[string]string)["vision"]
	}
	assert.Equal(t, "unknown", state("variant"))
	landProjector(t, dir, "digest")
	assert.Equal(t, "present", state("variant"))
	assert.NotContains(t, source, "features")
	inst := instance.NewInstance("catalog", "vllm", "variant", 8100, 0, 0)
	inst.Config = instance.Config{Provider: "vllm", Model: "variant"}
	inst.SetResolved(instance.Resolved{Parameters: map[string]string{}, WireEndpoints: []string{"messages_compat"}})
	inst.MarkRunning()
	require.NoError(t, m.instances.Register(inst))
	assert.Equal(t, "available", state("variant"))
	assert.Equal(t, []string{"messages_compat"}, m.ModelFeatureDetails("vllm", "variant", nil)["wire_endpoints"])
	inst.SetResolved(instance.Resolved{Parameters: map[string]string{"mmproj": filepath.Join(dir, "a-projector.gguf")}, WireEndpoints: []string{"messages"}})
	assert.Equal(t, "present", state("variant"))
	require.NoError(t, m.instances.Remove(inst.ID))
	svc.Models["variant"] = config.ModelSpec{From: "vendor/model", Parameters: map[string]string{"mmproj": "auto"}}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
	assert.Equal(t, "available", state("variant"))
	assert.Equal(t, "present", state("vendor/model"))
}

func TestCatalogRuntimeFeatureStates(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t)
	svc.Features = map[string]config.Feature{"vision": {Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"messages"}}}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
	fake := newFakeInstaller("test-runtime")
	m.installs.dispatcher.Register("test-runtime", fake)
	state := func() string {
		return m.ModelFeatureDetails("vllm", "variant", nil)["features"].(map[string]string)["vision"]
	}
	assert.Equal(t, "unknown", state())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0600))
	assert.Equal(t, "unsupported", state())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	assert.Equal(t, "available", state())
	fake.installed.Store(true)
	assert.Equal(t, "present", state())
	assert.Equal(t, []string{"messages"}, m.ModelFeatureDetails("vllm", "variant", nil)["wire_endpoints"])
	inst := instance.NewInstance("old-runtime", "vllm", "variant", 8100, 0, 0)
	inst.Config = instance.Config{Provider: "vllm", Model: "variant"}
	inst.SetResolved(instance.Resolved{WireEndpoints: []string{"messages_compat"}})
	inst.MarkRunning()
	require.NoError(t, m.instances.Register(inst))
	assert.Equal(t, "available", state())
	require.NoError(t, m.instances.Remove(inst.ID))
}

func TestCatalogBuiltinAndCloudEvidence(t *testing.T) {
	cfg := testAppsConfig()
	require.NoError(t, cfg.AddApp("cloud", config.ServiceConfig{Enabled: new(false), Mode: "cloud", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{Endpoint: "https://example.test", API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "test-token"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	require.NoError(t, cfg.UpdateApp("vllm", func(s *config.ServiceConfig) error { s.Features = map[string]config.Feature{"vision": {}}; return nil }))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	assert.Equal(t, "unknown", m.ModelFeatureDetails("cloud", "llava-vision", nil)["features"].(map[string]string)["vision"])
	for _, tc := range []struct {
		details map[string]any
		want    string
	}{{nil, "unknown"}, {map[string]any{"vision": true}, "present"}, {map[string]any{"vision": false}, "unsupported"}} {
		assert.Equal(t, tc.want, m.ModelFeatureDetails("vllm", "unnamed", tc.details)["features"].(map[string]string)["vision"])
	}
}

func TestCatalogLifecycleInvalidation(t *testing.T) {
	calls := 0
	m, err := NewProviderAppManager(testAppsConfig(), WithCatalogChanged(func() { calls++ }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	// A saturated audit channel drops every event. Invalidation must still run.
	for i := 0; i < cap(m.auditEvents); i++ {
		m.auditEvents <- Event{Type: EventInstallProgress}
	}
	for _, event := range []EventType{EventInstanceRunning, EventInstanceStopped, EventInstanceFailed} {
		m.emitEvent(Event{Type: event})
	}
	assert.Equal(t, 3, calls)
	c := m.installs
	for _, event := range []EventType{EventInstallCompleted, EventUpgradeCompleted, EventUninstallCompleted} {
		c.emit(Event{Type: event})
	}
	assert.Equal(t, 6, calls)
	c.emit(Event{Type: EventInstallProgress})
	assert.Equal(t, 6, calls)
}

func TestCatalogInvalidationAfterPhysicalUpgradeAndUninstall(t *testing.T) {
	for _, operation := range []string{"upgrade", "uninstall"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%v", operation, fail), func(t *testing.T) {
				coord, fake, _, cleanup := newAsyncTestCoord(t, "vllm")
				defer cleanup()
				calls := 0
				coord.catalogChanged = func() { calls++ }
				fake.release()
				if fail {
					if operation == "upgrade" {
						fake.upgradeErr.Store(errors.New("failed"))
					} else {
						fake.uninstallErr.Store(errors.New("failed"))
					}
				}
				h := coord.openJob(operation, "vllm")
				require.NotNil(t, h)
				var err error
				if operation == "upgrade" {
					err = coord.runUpgrade(h, "vllm", "1.0.0", fake)
				} else {
					err = coord.runUninstall(h, "vllm", fake)
				}
				if fail {
					require.Error(t, err)
					assert.Zero(t, calls)
					h.Fail(err)
				} else {
					require.NoError(t, err)
					assert.Equal(t, 1, calls)
					h.Done()
				}
			})
		}
	}
}

// A provider that declares nothing about features has said nothing, so its
// models are unknown and never refused; one that declares a block without
// vision has said it has none.
func TestCatalogUndeclaredProviderIsUnknown(t *testing.T) {
	cfg := testAppsConfig()
	nas := config.NewOllamaConnectProvider("ollama-nas", "http://nas.lan:11434", "")
	require.NoError(t, cfg.AddApp("ollama-nas", config.ServiceConfig{Enabled: new(true), Name: "ollama-nas", Mode: "external",
		Protocol: nas.Protocol, Runtime: &config.AppRuntimeConfig{Endpoint: nas.Runtime.Endpoint}, Capabilities: nas.Capabilities}))
	require.NoError(t, cfg.UpdateApp("vllm", func(s *config.ServiceConfig) error { s.Features = map[string]config.Feature{}; return nil }))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })

	vision := func(provider string) string {
		return m.ModelFeatureDetails(provider, "llava:13b", nil)["features"].(map[string]string)["vision"]
	}
	assert.Equal(t, "unknown", vision("ollama-nas"))
	assert.Equal(t, "unsupported", vision("vllm"))
}

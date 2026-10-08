package prov_apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func featureLaunchFixture(t *testing.T) (*ProviderAppManager, config.ServiceConfig, string) {
	t.Helper()
	root := t.TempDir()
	previousRoot, err := modelregistry.GetModelsRootDir()
	require.NoError(t, err)
	modelregistry.SetModelsRootDirOverride(root)
	t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(previousRoot) })
	dir := filepath.Join(root, "vendor/model")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "weights.gguf"), []byte("weights"), 0600))
	svc := tierServiceConfig()
	svc.Runtime.Execution.Args = []string{"--model", "${MODEL_PATH}"}
	svc.Features = map[string]config.Feature{"vision": {Files: []string{"a-projector.gguf"}, Flag: "mmproj"}}
	svc.Models = map[string]config.ModelSpec{"variant": {From: "vendor/model"}}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	return m, svc, dir
}

func landProjector(t *testing.T, dir, digest string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a-projector.gguf"), []byte("projector"), 0600))
	require.NoError(t, integrity.RecordFiles(dir, "vendor/model", "", integrity.FileChecksum{RelativePath: "a-projector.gguf", Size: 9, SHA256: digest, Feature: "vision"}))
}

func TestFeatureLaunchPresenceDigestVariantAndStaleness(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t)
	req := LaunchRequest{Provider: "vllm", Model: "variant"}
	before, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	assert.NotContains(t, before.launch.Args, "--mmproj")
	inst := instance.NewInstance("feature-test", "vllm", "variant", 8100, 0, 0)
	inst.Config = instance.Config{Provider: "vllm", Model: "variant", Port: 8100}
	inst.Endpoint = "chat"
	inst.SetResolved(before.resolved)
	landProjector(t, dir, "manifest-digest")
	after, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	assert.Contains(t, after.launch.Args, filepath.Join(dir, "a-projector.gguf"))
	assert.Equal(t, filepath.Join(dir, "weights.gguf"), after.launch.ModelPath)
	assert.Equal(t, "manifest-digest", after.resolved.Files["mmproj"], "use manifest digest, never rehash")
	assert.Equal(t, instance.ParametersStale, m.ParametersStatus(inst).State)
	inst.SetResolved(after.resolved)
	require.NoError(t, integrity.RecordFiles(dir, "vendor/model", "", integrity.FileChecksum{RelativePath: "a-projector.gguf", Size: 9, SHA256: "replacement-digest", Feature: "vision"}))
	assert.Equal(t, []string{"parameters.mmproj"}, m.ParametersStatus(inst).Changed)
}

func TestFeatureLaunchExplicitOptOut(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t)
	landProjector(t, dir, "digest")
	for _, value := range []string{"auto", "false", "operator-path"} {
		t.Run(value, func(t *testing.T) {
			for _, tier := range []string{"config", "node", "endpoint", "request"} {
				t.Run(tier, func(t *testing.T) {
					copy := svc
					req := LaunchRequest{Provider: "vllm", Model: "vendor/model"}
					m.nodename = func() string { return "feature-node" }
					if tier == "node" {
						copy.Nodes = map[string]config.NodeSpec{"feature-node": {Parameters: map[string]string{"mmproj": value}}}
					} else if tier == "endpoint" {
						copy.Models = map[string]config.ModelSpec{"vendor/model": {Endpoints: map[string]config.EndpointOverlay{"chat": {Parameters: map[string]string{"mmproj": value}}}}}
					} else if tier == "config" {
						copy.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"mmproj": value}}
					} else {
						req.Parameters = map[string]string{"mmproj": value}
					}
					p, err := m.prepareLaunch(copy, "vllm", "chat", req, 8100)
					require.NoError(t, err)
					assert.NotContains(t, p.launch.Args, filepath.Join(dir, "a-projector.gguf"))
					assert.NotContains(t, p.resolved.Files, "mmproj")
					if value == "auto" || value == "false" {
						assert.NotContains(t, p.launch.Args, "--mmproj")
					} else {
						assert.Contains(t, p.launch.Args, "operator-path")
					}
				})
			}
		})
	}
}

func TestFeatureLaunchLegacyAndMissingFiles(t *testing.T) {
	_, svc, dir := featureLaunchFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a-projector.gguf"), []byte("legacy"), 0600))
	p, err := LocalizeModelFeatures(svc, "vendor/model", nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "a-projector.gguf"), p.Params["mmproj"])
	assert.Equal(t, "", p.Files["mmproj"])
	assert.Equal(t, "feature:vision", p.Sources["mmproj"])
	require.NoError(t, os.Remove(filepath.Join(dir, "a-projector.gguf")))
	p, err = LocalizeModelFeatures(svc, "vendor/model", nil)
	require.NoError(t, err)
	assert.Empty(t, p.Sources)
}

func TestFeatureLaunchRuntimeSelectionAndStaleness(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	svc.Features = map[string]config.Feature{"vision": {Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3", Args: []string{"-m", "vision.server", "--model", "${MODEL_PATH}"}, WireModel: config.WireModelPath}, WireEndpoints: []string{"chat_completions", "responses", "messages"}, ExcludeParameters: []string{"chat-template"}}}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
	fake := newFakeInstaller("test-runtime")
	m.installs.dispatcher.Register("test-runtime", fake)
	req := LaunchRequest{Provider: "vllm", Model: "variant", Parameters: map[string]string{"chat-template": "named-asset"}}
	before, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	assert.NotContains(t, before.launch.Args, "vision.server")
	isolateProviderRoot(t)
	python := fsroot.ProviderVenvPython("test-runtime")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, nil, 0700))
	fake.installed.Store(true)
	after, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	assert.Contains(t, after.launch.Args, "vision.server")
	assert.Equal(t, python, after.launch.Command, "use the selected runtime venv")
	assert.NotContains(t, after.launch.Args, "--chat-template")
	assert.Equal(t, after.launch.ModelPath, after.launch.WireModel)
	assert.Equal(t, []string{"chat_completions", "responses", "messages"}, after.resolved.WireEndpoints)
	inst := instance.NewInstance("runtime-test", "vllm", "variant", 8100, 0, 0)
	inst.Config = instance.Config{Provider: "vllm", Model: "variant", Port: 8100, Parameters: req.Parameters}
	inst.Endpoint = "chat"
	inst.SetResolved(before.resolved)
	assert.Contains(t, m.ParametersStatus(inst).Changed, "runtime.execution")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0600))
	text, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	assert.NotContains(t, text.launch.Args, "vision.server")
	assert.Equal(t, svc.Runtime.Execution.Command, text.config.Runtime.Execution.Command)
}

func TestFeatureLaunchManagedExecutableChangeIsStale(t *testing.T) {
	m, svc, _ := featureLaunchFixture(t)
	isolateProviderRoot(t)
	req := LaunchRequest{Provider: "vllm", Model: "vendor/model"}
	before, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	inst := instance.NewInstance("executable-test", "vllm", req.Model, 8100, 0, 0)
	inst.Config = instance.Config{Provider: "vllm", Model: req.Model, Port: 8100}
	inst.Endpoint = "chat"
	inst.SetResolved(before.resolved)
	python := fsroot.ProviderVenvPython("vllm")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, nil, 0700))
	after, err := m.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	assert.Equal(t, python, after.launch.Command)
	assert.NotEqual(t, before.resolved.Execution, after.resolved.Execution)
	assert.Contains(t, m.ParametersStatus(inst).Changed, "runtime.execution")
}

func TestFeatureLaunchWithoutReadableConfig(t *testing.T) {
	for _, installed := range []bool{false, true} {
		for _, body := range []string{"missing", "unreadable", "invalid", `{"vision_config":null}`} {
			t.Run(fmt.Sprintf("installed=%t/%s", installed, body), func(t *testing.T) {
				m, svc, dir := featureLaunchFixture(t)
				svc.Features = map[string]config.Feature{"vision": {Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3", Args: []string{"-m", "vision.server"}}, WireEndpoints: []string{"chat_completions"}}}
				fake := newFakeInstaller("test-runtime")
				fake.installed.Store(installed)
				m.installs.dispatcher.Register("test-runtime", fake)
				if body == "unreadable" {
					require.NoError(t, os.Mkdir(filepath.Join(dir, "config.json"), 0700))
				} else if body != "missing" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600))
				}
				// Resolve through the configured provider to exercise preview's real path.
				cfg := &config.AppsConfig{}
				require.NoError(t, cfg.AddApp("vllm", svc))
				m.ReloadConfig(cfg)
				got, err := m.ResolveLaunchParameters(LaunchRequest{Provider: "vllm", Model: "vendor/model"})
				require.NoError(t, err)
				assert.Empty(t, got.RuntimeKey)
				assert.Equal(t, svc.Runtime.Execution, got.Config.Runtime.Execution)
				if body != `{"vision_config":null}` {
					require.Error(t, m.EnsureModelFeatures(context.Background(), "vllm", dir, []string{"vision"}, nil, nil))
				}
			})
		}
	}
}

package prov_apps

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// declaredFragments merges the parameters of yaml schema fragments.
func declaredFragments(fragments ...string) (*schema.YAMLSchema, error) {
	declared := &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{}, Endpoints: map[string]schema.YAMLEndpoint{}}
	for _, fragment := range fragments {
		parsed, err := schema.LoadYAMLSchema([]byte(fragment))
		if err != nil {
			return nil, err
		}
		maps.Copy(declared.Parameters, parsed.Parameters)
		maps.Copy(declared.Endpoints, parsed.Endpoints)
	}
	return declared, nil
}

// declaredSchema resolves every provider to its Go spine and the run
// settings plus the parameters each yaml fragment declares.
func declaredSchema(t *testing.T, fragments ...string) Option {
	t.Helper()
	declared, err := declaredFragments(fragments...)
	require.NoError(t, err)
	return WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		merged, errs := schema.Merge(provider, declared)
		return merged, errors.Join(errs...)
	})
}

// requireRefused asserts err refuses exactly one parameter and returns it.
func requireRefused(t *testing.T, err error, key string, code httperr.ParamErrorCode) *ParameterError {
	t.Helper()
	require.ErrorIs(t, err, ErrParameterValidation)
	var refused *ParameterError
	require.ErrorAs(t, err, &refused)
	require.Len(t, refused.Issues, 1, refused.Error())
	assert.Equal(t, key, refused.Issues[0].Key)
	assert.Equal(t, string(code), refused.Issues[0].Code)
	return refused
}

// checkedEngine is a provider whose schema declares ctx-size, flash-attn and
// its embeddings overlay's embeddings; its own engine takes none of exclude.
func checkedEngine(t *testing.T, exclude ...string) *ProviderAppManager {
	t.Helper()
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("engine", config.ServiceConfig{
		Enabled: new(true), Name: "engine", Protocol: config.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &config.AppRuntimeConfig{PortRange: []int{8100, 8105}, BasePort: 8100, Execution: config.ExecutionConfig{Type: "cli", Command: "engine"}, ExcludeParameters: exclude},
		Defaults: &config.AppDefaultsConfig{
			Parameters: map[string]string{"ctx-size": "4096", "flash-attn": "true"},
			Metadata:   &config.AppMetadata{ContextParam: "ctx-size"},
			Endpoints:  map[string]config.EndpointOverlay{string(EndpointEmbeddings): {Parameters: map[string]string{"embeddings": "true"}}},
		},
		Models: map[string]config.ModelSpec{
			"configured": {Parameters: map[string]string{"context-length": "8192"}},
			"stored":     {Parameters: map[string]string{"context_length": "8192"}},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions", "embeddings"}},
	}))
	m, err := NewProviderAppManager(cfg, declaredSchema(t, `
parameters:
  ctx-size: {type: int, min: 1}
  flash-attn: {type: bool}
endpoints:
  embeddings:
    parameters:
      embeddings: {type: bool}
`))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	return m
}

func resolveEngine(t *testing.T, m *ProviderAppManager, model string, endpoint Endpoint, params map[string]string) error {
	t.Helper()
	_, err := m.ResolveLaunchParameters(t.Context(), EnsureRequest{Provider: "engine", Model: model, Endpoint: endpoint, Parameters: params})
	return err
}

func TestLaunchChecksEveryParameterItPasses(t *testing.T) {
	m := checkedEngine(t)
	resolve := func(model string, endpoint Endpoint, params map[string]string) error {
		return resolveEngine(t, m, model, endpoint, params)
	}

	refused := requireRefused(t, resolve("m", EndpointChat, map[string]string{"context-length": "32768"}), "context-length", httperr.CodeUnknownFlag)
	assert.Contains(t, refused.Issues[0].Message, "request tier")
	assert.Equal(t, "ctx-size", refused.ContextParameter)
	assert.False(t, refused.Served)
	requireRefused(t, resolve("m", EndpointChat, map[string]string{"ctx-size": "many"}), "ctx-size", httperr.CodeWrongType)
	requireRefused(t, resolve("m", EndpointChat, map[string]string{"ctx-size": "0"}), "ctx-size", httperr.CodeOutOfRange)
	requireRefused(t, resolve("m", EndpointChat, map[string]string{"flash-attn": "on"}), "flash-attn", httperr.CodeWrongType)
	requireRefused(t, resolve("m", EndpointChat, map[string]string{"embeddings": "true"}), "embeddings", httperr.CodeUnknownFlag)
	configured := requireRefused(t, resolve("configured", EndpointChat, nil), "context-length", httperr.CodeUnknownFlag)
	assert.Contains(t, configured.Issues[0].Message, "model tier")
	requireRefused(t, resolve("stored", EndpointChat, nil), "context_length", httperr.CodeUnknownFlag)

	require.NoError(t, resolve("m", EndpointEmbeddings, nil), "the embeddings overlay declares its own flag")
	require.NoError(t, resolve("m", EndpointChat, map[string]string{"ctx-size": "auto"}), "auto leaves a declared key to the engine, so it is not passed")
	requireRefused(t, resolve("m", EndpointChat, map[string]string{"context_length": "auto"}), "context_length", httperr.CodeUnknownFlag)
}

// ctx_size and ctx-size are one flag: a request's overrides the configured
// tier and argv carries it once.
func TestLaunchKeysParametersByFlag(t *testing.T) {
	m := checkedEngine(t)
	preview, err := m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "engine", Model: "m", Parameters: map[string]string{"ctx_size": "8192"}}, 8100)
	require.NoError(t, err)
	flags := 0
	for _, arg := range preview.Args {
		if arg == "--ctx-size" {
			flags++
		}
	}
	assert.Equal(t, 1, flags, "argv %q", preview.Args)
	assert.Contains(t, preview.Args, "8192")
	assert.NotContains(t, preview.Args, "4096")
	assert.Equal(t, config.TierRequest.String(), preview.Sources["ctx-size"])

	refused := requireRefused(t, resolveEngine(t, m, "m", EndpointChat, map[string]string{"context_length": "32768"}), "context_length", httperr.CodeUnknownFlag)
	assert.Contains(t, refused.Issues[0].Message, "request tier")
	for range 50 {
		require.ErrorAs(t, resolveEngine(t, m, "m", EndpointChat, map[string]string{"ctx_size": "1", "ctx-size": "2"}), &refused)
		require.Len(t, refused.Issues, 2, "one entry per spelling, so each can be removed by name")
		assert.Equal(t, 1, strings.Count(refused.Error(), "spell one flag"), "the summary states it once")
		for i, key := range []string{"ctx-size", "ctx_size"} {
			assert.Equal(t, key, refused.Issues[i].Key)
			assert.Equal(t, string(httperr.CodeDuplicateFlag), refused.Issues[i].Code)
			assert.Contains(t, refused.Issues[i].Message, "ctx-size and ctx_size")
			assert.Contains(t, refused.Issues[i].Message, "request tier")
		}
	}
	require.NoError(t, resolveEngine(t, m, "m", EndpointChat, map[string]string{"ctx_size": "2", "ctx-size": "2"}))
}

// A runtime takes none of what it excludes: a configured value is policy
// across engines and is dropped, sources and all; a request's would not
// take effect and is refused with its own code, naming the runtime.
func TestBaseRuntimeExclusions(t *testing.T) {
	m := checkedEngine(t, "flash-attn")
	preview, err := m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "engine", Model: "m"}, 8100)
	require.NoError(t, err)
	assert.NotContains(t, preview.Args, "--flash-attn")
	assert.NotContains(t, preview.Params, "flash-attn")
	assert.NotContains(t, preview.Sources, "flash-attn", "a dropped key claims no tier")
	refused := requireRefused(t, resolveEngine(t, m, "m", EndpointChat, map[string]string{"flash_attn": "true"}), "flash_attn", httperr.CodeExcludedByRuntime)
	assert.Contains(t, refused.Issues[0].Message, "runtime engine")
}

func TestFeatureRuntimeExclusions(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t, "parameters:\n  chat-template: {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	svc.Features = map[string]config.Feature{"vision": {Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3", Args: []string{"-m", "vision.server", "--model", "${MODEL_PATH}"}}, WireEndpoints: []string{"chat_completions"}, ExcludeParameters: []string{"chat-template"}}}
	svc.Defaults.Parameters["chat-template"] = "configured"
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
	fake := newFakeInstaller("test-runtime")
	m.installs.dispatcher.Register("test-runtime", fake)
	req := EnsureRequest{Provider: "vllm", Model: "variant"}

	base, err := processEngine{m}.prepare(t.Context(), svc, "vllm", req, 8100)
	require.NoError(t, err)
	assert.Contains(t, base.launch.Args, "--chat-template", "the base runtime takes it")

	isolateProviderRoot(t)
	python := fsroot.ProviderVenvPython("test-runtime")
	require.NoError(t, os.MkdirAll(filepath.Dir(python), 0700))
	require.NoError(t, os.WriteFile(python, nil, 0700))
	fake.installed.Store(true)
	prepared, err := processEngine{m}.prepare(t.Context(), svc, "vllm", req, 8100)
	require.NoError(t, err)
	assert.Contains(t, prepared.launch.Args, "vision.server")
	assert.NotContains(t, prepared.launch.Args, "--chat-template")
	req.Parameters = map[string]string{"chat-template": "requested"}
	_, err = processEngine{m}.prepare(t.Context(), svc, "vllm", req, 8100)
	refused := requireRefused(t, err, "chat-template", httperr.CodeExcludedByRuntime)
	assert.Contains(t, refused.Issues[0].Message, "runtime test-runtime")
}

// A run recorded with a key its provider no longer declares is refused on
// restart, the same as a new launch with that key.
func TestRestartRefusesARecordedUndeclaredParameter(t *testing.T) {
	declared := `
parameters:
  budget: {type: float}
  seed: {type: int}
`
	var current atomic.Pointer[schema.YAMLSchema]
	parsed, err := schema.LoadYAMLSchema([]byte(declared))
	require.NoError(t, err)
	current.Store(parsed)
	r := newContractRig(t, false, WithProviderSchema(func(provider string) (*schema.ProviderSchema, error) {
		merged, errs := schema.Merge(provider, current.Load())
		return merged, errors.Join(errs...)
	}))
	run := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "m", Parameters: map[string]string{"seed": "7"}})})[0]
	require.NoError(t, run.err)
	awaitRunning(t, run.result.Run)

	withoutSeed, err := schema.LoadYAMLSchema([]byte("parameters:\n  budget: {type: float}\n"))
	require.NoError(t, err)
	current.Store(withoutSeed)
	_, err = r.m.RestartInstance(t.Context(), run.result.Run.ID)
	refused := requireRefused(t, err, "seed", httperr.CodeUnknownFlag)
	assert.Contains(t, refused.Issues[0].Message, "request tier")
}

// A served daemon owns its model settings: a run passes it nothing but
// the run settings, so any other request parameter is refused.
func TestServedRunRefusesParametersItCannotPass(t *testing.T) {
	r := newServedRig(t)
	_, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "ollama", Model: "p", Parameters: map[string]string{"temperature": "0.7"}})
	refused := requireRefused(t, err, "temperature", httperr.CodeUnknownFlag)
	assert.True(t, refused.Served)
	assert.Empty(t, r.m.ListInstances(""))

	_, err = r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "ollama", Model: "p", Parameters: map[string]string{"num-ctx": "4096"}}, 0)
	requireRefused(t, err, "num-ctx", httperr.CodeUnknownFlag)
	r.running(t, EnsureRequest{Provider: "ollama", Model: "p", Parameters: map[string]string{"keep-alive": "-1"}})
}

// A served run reads its run settings from the configured tiers, so they
// meet the launch check there: two spellings of keep-alive that disagree
// are refused, not settled by sort order.
func TestServedRunChecksTheRunSettingsItReads(t *testing.T) {
	r := newServedRig(t)
	r.edit(t, func(svc *config.ServiceConfig) {
		svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"keep_alive": "5m", "keep-alive": "10m"}}
	})
	_, err := r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "ollama", Model: "p"}, 0)
	var refused *ParameterError
	require.ErrorAs(t, err, &refused)
	require.Len(t, refused.Issues, 2)
	for i, key := range []string{"keep-alive", "keep_alive"} {
		assert.Equal(t, key, refused.Issues[i].Key)
		assert.Equal(t, string(httperr.CodeDuplicateFlag), refused.Issues[i].Code)
		assert.Contains(t, refused.Issues[i].Message, "default tier")
	}
	_, err = r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "ollama", Model: "p", Parameters: map[string]string{"keep-alive": "-1"}}, 0)
	require.NoError(t, err, "the request's keep-alive settles it")
}

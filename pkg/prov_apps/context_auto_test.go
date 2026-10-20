package prov_apps

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextAutoLaunchIntentAndOverrides(t *testing.T) {
	for _, engine := range []struct{ key, spelling string }{{"max-model-len", "auto"}, {"ctx-size", "0"}} {
		for _, tc := range []struct{ name, configured, explicit, argv string }{
			{"config auto", "auto", "", engine.spelling},
			{"request auto", "8192", "auto", engine.spelling},
			{"request number", "auto", "16384", "16384"},
		} {
			t.Run(engine.key+"/"+tc.name, func(t *testing.T) {
				r := newContractRig(t, false)
				declared := &schema.ProviderSchema{Parameters: map[string]schema.ParamShape{engine.key: {Kind: schema.ParamInt, Auto: engine.spelling}}}
				r.m.providerSchema = func(string) (*schema.ProviderSchema, error) { return declared, nil }
				svc, _ := r.m.AppsConfig().LookupApp("generic")
				svc.Defaults.Parameters = map[string]string{engine.key: tc.configured}
				cfg := &config.AppsConfig{}
				require.NoError(t, cfg.AddApp("generic", svc))
				r.m.ReloadConfig(cfg)
				req := EnsureRequest{Provider: "generic", Model: "model"}
				want := tc.configured
				if tc.explicit != "" {
					req.Parameters = map[string]string{engine.key: tc.explicit}
					want = tc.explicit
				}
				preview, err := r.m.PreviewLaunch(t.Context(), req, 8740)
				require.NoError(t, err)
				assert.Equal(t, want, preview.Params[engine.key])
				assert.Contains(t, preview.Args, tc.argv)
				result, err := r.m.EnsureRun(t.Context(), req)
				require.NoError(t, err)
				awaitRunning(t, result.Run)
				assert.Equal(t, want, result.Run.Resolved().Parameters[engine.key])
				assert.Equal(t, want, result.Run.ToInfo("").ResolvedParameters[engine.key])
				status := r.m.ParametersStatus(t.Context(), result.Run)
				assert.Equal(t, instance.ParametersCurrent, status.State)
				assert.Empty(t, status.Changed)
				if tc.explicit == "" {
					assert.Empty(t, status.Overridden)
				} else {
					assert.Equal(t, []string{"parameters." + engine.key}, status.Overridden)
				}
			})
		}
	}
}

func TestContextAutoUsesFreeMemoryMinusMargin(t *testing.T) {
	m, svc, dir := featureLaunchFixture(t)
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-A")
	require.NoError(t, os.Truncate(filepath.Join(dir, "weights.gguf"), 1<<30))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"num_hidden_layers":32,"num_key_value_heads":8,"num_attention_heads":32,"hidden_size":4096,"torch_dtype":"float16"}`), 0600))
	svc.Defaults.Metadata = &config.AppMetadata{ContextParam: "max-model-len"}
	svc.Defaults.Parameters = map[string]string{"max-model-len": "auto", "gpu-memory-utilization": "auto"}
	m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
		declared, errs := schema.Merge(provider, &schema.YAMLSchema{
			Parameters: map[string]schema.YAMLParam{"max-model-len": {Auto: "auto"}},
			Diagnostics: &schema.Diagnostics{Runtime: "test", Runtimes: map[string]schema.RuntimeChecks{"test": {Kernels: "unknown"}},
				Memory: &schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "gpu-memory-utilization", AutoSafetyMarginMiB: 1000}},
		}, config.ServiceConfig{})
		require.Empty(t, errs)
		return declared, nil
	}
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 9500}}, nil
	}
	req := EnsureRequest{Provider: "vllm", Model: "vendor/model"}
	model := process.ResolveModel(svc, req.Model)
	need, known := process.ModelMemoryNeed(model, svc, svc.Defaults.Parameters, schema.AutoContext{}, nil)
	assert.False(t, known)
	assert.Zero(t, need.Total())
	numeric := maps.Clone(svc.Defaults.Parameters)
	numeric["max-model-len"] = "2048"
	_, known = process.ModelMemoryNeed(model, svc, numeric, schema.AutoContext{}, nil)
	require.True(t, known, "the dense fixture must be sizeable with a numeric context")
	run := instance.NewInstance("auto-context", "vllm", req.Model, 8100, 0, 0)
	defer run.Cancel()
	run.Endpoint = "chat"
	prepared, err := m.prepareRun(t.Context(), run, req, launchSelection{key: "vllm", config: svc, model: model}, requireLaunchSchema(t, m, "vllm"))
	require.NoError(t, err)
	assert.Contains(t, prepared.args, "0.8500")
	assert.Equal(t, "auto", run.Resolved().Parameters["max-model-len"])
}

func TestParametersStatusRequestRepairsInvalidConfigOnlyBudget(t *testing.T) {
	r := reservationRig(t, false)
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Defaults.Parameters["gpu-reserve-mib"] = "invalid"
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", svc))
	r.m.ReloadConfig(cfg)
	req := EnsureRequest{Provider: "generic", Parameters: map[string]string{"gpu-reserve-mib": "6000"}}
	result, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	_, err = r.m.modelLaunchParameters(t.Context(), svc, process.ResolveModel(svc, ""), EnsureRequest{Provider: "generic"}, "chat", requireLaunchSchema(t, r.m, "generic"))
	require.Error(t, err, "config-only preparation cannot replace the raw fallback")
	status := r.m.ParametersStatus(t.Context(), result.Run)
	assert.Equal(t, instance.ParametersCurrent, status.State)
	assert.Contains(t, status.Overridden, "parameters.gpu-reserve-mib")
}

func TestFilterLaunchAutoUsesEndpointShape(t *testing.T) {
	m := tierManager(t, "")
	m.providerSchema = func(string) (*schema.ProviderSchema, error) {
		declared, errs := schema.Merge("generic", &schema.YAMLSchema{
			Endpoints: map[string]schema.YAMLEndpoint{"embeddings": {Parameters: map[string]schema.YAMLParam{"context": {Auto: "auto"}}}},
			Diagnostics: &schema.Diagnostics{Runtime: "test", Runtimes: map[string]schema.RuntimeChecks{"test": {Kernels: "unknown"}},
				Memory: &schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "budget"}},
		}, config.ServiceConfig{})
		require.Empty(t, errs)
		return declared, nil
	}
	input := map[string]string{"context": "auto", "budget": "auto"}
	chat := filterLaunchAuto(requireLaunchSchema(t, m, "vllm"), "chat", input)
	assert.Equal(t, map[string]string{"budget": "auto"}, chat)
	embed := filterLaunchAuto(requireLaunchSchema(t, m, "vllm"), "embeddings", input)
	assert.Equal(t, map[string]string{"context": "auto", "budget": "auto"}, embed)
	assert.Equal(t, map[string]string{"context": "auto", "budget": "auto"}, input)
}

func TestLaunchPreparationResolvesSchemaOnce(t *testing.T) {
	var calls atomic.Int32
	declared, errs := schema.Merge("generic", &schema.YAMLSchema{Parameters: map[string]schema.YAMLParam{"context": {Type: "int", Auto: "0"}, "budget": {}}}, config.ServiceConfig{})
	require.Empty(t, errs)
	r := newContractRig(t, true, WithProviderSchema(func(string) (*schema.ProviderSchema, error) {
		calls.Add(1)
		return declared, nil
	}), WithParamLocalizer(func(_, _, _ string, params map[string]string, shapes map[string]schema.ParamShape) (Localized, error) {
		assert.Equal(t, "0", shapes["context"].Auto)
		return Localized{Params: params}, nil
	}))
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Defaults.Parameters["context"] = "auto"
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", svc))
	r.m.ReloadConfig(cfg)
	req := EnsureRequest{Provider: "generic", Model: "model"}
	preview, err := r.m.PreviewLaunch(t.Context(), req, 8740)
	require.NoError(t, err)
	assert.Contains(t, preview.Args, "0")
	assert.Equal(t, int32(1), calls.Swap(0), "preview shares the schema across all preparation steps")
	result, err := r.m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Swap(0), "pre-stop and admission preparation each resolve the schema once")
	assert.Equal(t, instance.StatusStarting, result.Run.GetStatus())
	assert.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), result.Run).State)
	assert.Equal(t, int32(1), calls.Swap(0), "status shares the schema with the config-only comparison")
}

func TestBuildLaunchAutoUsesEndpointShape(t *testing.T) {
	m := tierManager(t, "")
	m.providerSchema = func(string) (*schema.ProviderSchema, error) {
		return &schema.ProviderSchema{
			Parameters: map[string]schema.ParamShape{"context": {Auto: "0"}},
			Endpoints: map[string]schema.EndpointSchema{"embeddings": {Parameters: map[string]schema.ParamShape{
				"context": {Auto: "-1"},
			}}},
		}, nil
	}
	svc, _ := m.AppsConfig().LookupApp("vllm")
	input := map[string]string{"context": "auto"}
	for endpoint, spelling := range map[string]string{"chat": "0", "embeddings": "-1"} {
		launch, _, err := m.buildLaunch(svc, "vllm", endpoint, process.ResolveModel(svc, "llama3"), 8100, input, requireLaunchSchema(t, m, "vllm"))
		require.NoError(t, err)
		assert.Contains(t, launch.Args, spelling)
		assert.NotContains(t, launch.Args, "auto")
	}
	assert.Equal(t, map[string]string{"context": "auto"}, input)
}

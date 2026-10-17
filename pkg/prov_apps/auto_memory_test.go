package prov_apps

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// memoryBudgetSchema declares resolve's budget, its parameters, what the
// shared fixtures set (tierParameters, budget) and any further fragments.
func memoryBudgetSchema(resolve func(string) (*schema.MemoryBudget, error), fragments ...string) func(string) (*schema.ProviderSchema, error) {
	return func(provider string) (*schema.ProviderSchema, error) {
		budget, err := resolve(provider)
		declared, parseErr := declaredFragments(append([]string{tierParameters, "parameters:\n  budget: {}\n"}, fragments...)...)
		if parseErr != nil {
			return nil, parseErr
		}
		declared.Diagnostics = &schema.Diagnostics{Runtime: "test", Runtimes: map[string]schema.RuntimeChecks{"test": {Kernels: "unknown"}}, Memory: budget}
		if budget != nil {
			for _, name := range []string{budget.Parameter, budget.DeviceCountParameter, budget.ComplementParameter} {
				if name != "" {
					declared.Parameters[name] = schema.YAMLParam{}
				}
			}
		}
		merged, errs := schema.Merge(provider, declared)
		return merged, errors.Join(append(errs, err)...)
	}
}

func requireLaunchSchema(t *testing.T, m *ProviderAppManager, provider string) *schema.ProviderSchema {
	t.Helper()
	declared, err := m.launchSchema(provider)
	require.NoError(t, err)
	return declared
}

func TestAutoMemoryNativeLaunchReceivesNumericArgument(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver and short-lived child fixture")
	}
	isolateProviderRoot(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte("#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 6000'; echo 'GPU-B, 1, 5000, 2500' ;;\nesac\n"), 0700))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-A,GPU-B")
	engine := filepath.Join(root, "engine")
	require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.argv.tmp\"\n/bin/mv \"$0.argv.tmp\" \"$0.argv\"\n"), 0700))
	cfg := &config.AppsConfig{}
	svc := config.ServiceConfig{Enabled: new(true), Name: "generic", Protocol: config.ProtocolOpenAI, Mode: "on-demand", Runtime: &config.AppRuntimeConfig{PortRange: []int{8640, 8642}, BasePort: 8640, Execution: config.ExecutionConfig{Type: "cli", Command: engine}}, Defaults: &config.AppDefaultsConfig{Parameters: map[string]string{"memory-fraction": "auto", "dtype": "auto"}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}
	require.NoError(t, cfg.AddApp("generic", svc))
	mgr, err := NewProviderAppManager(cfg, WithProviderSchema(memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "memory-fraction", DeviceCountParameter: "tensor-parallel-size", AutoSafetyMarginMiB: 1000}, nil
	}, "parameters:\n  dtype: {}\n")))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	preview, err := processEngine{mgr}.prepare(t.Context(), svc, "generic", EnsureRequest{Provider: "generic", Model: "registry-model"}, 8640)
	require.NoError(t, err)
	assert.Equal(t, "auto", preview.resolved.Parameters["memory-fraction"])
	assert.NotContains(t, preview.resolved.Parameters, "dtype")
	runResult, err := mgr.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "registry-model", Parameters: map[string]string{"memory_fraction": "auto", "tensor_parallel_size": "2"}})
	run := runResult.Run
	require.NoError(t, err)
	assert.Equal(t, "auto", run.Resolved().Parameters["memory-fraction"])
	assert.NotContains(t, run.Resolved().Parameters, "memory_fraction")
	// The fixture publishes only after printf finishes, so existence means complete argv.
	marker := engine + ".argv"
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, time.Second, 10*time.Millisecond)
	arguments, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Contains(t, string(arguments), "--memory-fraction\n0.3000\n")
	assert.NotContains(t, string(arguments), "auto")
	require.NoError(t, mgr.StopInstance(t.Context(), run.ID))
}

func TestAutoMemoryIntentPreservesParameterStatusWithoutDiscovery(t *testing.T) {
	isolateProviderRoot(t)
	mgr, svc, _ := featureLaunchFixture(t)
	mgr.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "memory-fraction"}, nil
	})
	svc.Defaults.Parameters["memory-fraction"] = "auto"
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	mgr.ReloadConfig(cfg)
	req := EnsureRequest{Provider: "vllm", Model: "vendor/model"}
	prepared, err := processEngine{mgr}.prepare(t.Context(), svc, "vllm", req, 8100)
	require.NoError(t, err)
	run := instance.NewInstance("test", "vllm", "vendor/model", 8100, 0, 0)
	run.Config = instance.Config{Provider: "vllm", Model: "vendor/model", Port: 8100}
	run.SetResolved(prepared.resolved)
	// No driver fixture exists; status must compare intent without probing memory.
	assert.Equal(t, instance.ParametersCurrent, mgr.ParametersStatus(t.Context(), run).State)
	numerical := run.Resolved()
	numerical.Parameters["memory-fraction"] = "0.5"
	run.SetResolved(numerical)
	assert.Contains(t, mgr.ParametersStatus(t.Context(), run).Changed, "parameters.memory-fraction", "numeric-to-auto must remain visible")
}

func TestAutoMemoryPrepareRunUsesModelNeedInArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix GPU observation fixture")
	}
	isolateProviderRoot(t)
	mgr, svc, dir := featureLaunchFixture(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte("#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 9500' ;;\nesac\n"), 0700))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-A")
	require.NoError(t, os.Truncate(filepath.Join(dir, "weights.gguf"), 1024*1024*1024))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"num_hidden_layers":32,"num_key_value_heads":8,"num_attention_heads":32,"hidden_size":4096,"torch_dtype":"float16"}`), 0600))
	svc.Defaults.Metadata = &config.AppMetadata{ContextParam: "max-model-len"}
	svc.Defaults.Parameters["max-model-len"] = "2048"
	svc.Defaults.Parameters["memory-fraction"] = "auto"
	mgr.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "memory-fraction"}, nil
	})
	mgr.localizeParams = func(_, _, _ string, params map[string]string, _ map[string]schema.ParamShape) (Localized, error) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "later.gguf"), []byte("later weights"), 0600))
		return Localized{Params: params}, nil
	}
	req := EnsureRequest{Provider: "vllm", Model: "variant"}
	inst := instance.NewInstance("need-argv", "vllm", req.Model, 8100, 0, 0)
	inst.Endpoint = "chat"
	run, err := mgr.prepareRun(t.Context(), inst, req, launchSelection{key: "vllm", config: svc, model: process.ResolveModel(svc, req.Model)}, requireLaunchSchema(t, mgr, "vllm"))
	require.NoError(t, err)
	assert.Contains(t, run.args, filepath.Join(dir, "weights.gguf"))
	require.Contains(t, run.args, "--memory-fraction")
	for i, arg := range run.args {
		if arg == "--memory-fraction" {
			require.Less(t, i+1, len(run.args))
			assert.Equal(t, "0.5914", run.args[i+1])
		}
	}
	assert.Equal(t, "auto", inst.Resolved().Parameters["memory-fraction"])
}

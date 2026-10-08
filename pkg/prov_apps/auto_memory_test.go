package prov_apps

import (
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
	mgr, err := NewProviderAppManager(cfg, WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "memory-fraction", DeviceCountParameter: "tensor-parallel-size", AutoSafetyMarginMiB: 1000}, nil
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	preview, err := mgr.prepareLaunch(svc, "generic", "chat", LaunchRequest{Provider: "generic", Model: "registry-model"}, 8640)
	require.NoError(t, err)
	assert.Equal(t, "auto", preview.resolved.Parameters["memory-fraction"])
	assert.NotContains(t, preview.resolved.Parameters, "dtype")
	run, err := mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: "registry-model", Parameters: map[string]string{"memory_fraction": "auto", "tensor_parallel_size": "2"}})
	require.NoError(t, err)
	assert.Equal(t, "0.3000", run.Resolved().Parameters["memory-fraction"])
	assert.Equal(t, "memory-fraction", run.Resolved().AutoMemory)
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
	mgr.memoryBudget = func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "memory-fraction"}, nil
	}
	svc.Defaults.Parameters["memory-fraction"] = "auto"
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	mgr.ReloadConfig(cfg)
	req := LaunchRequest{Provider: "vllm", Model: "vendor/model"}
	prepared, err := mgr.prepareLaunch(svc, "vllm", "chat", req, 8100)
	require.NoError(t, err)
	prepared.resolved.Parameters["memory-fraction"] = "0.5"
	run := instance.NewInstance("test", "vllm", "vendor/model", 8100, 0, 0)
	run.Config = instance.Config{Provider: "vllm", Model: "vendor/model", Port: 8100}
	run.SetResolved(prepared.resolved)
	// No driver fixture exists; status must compare intent without probing memory.
	assert.Equal(t, instance.ParametersCurrent, mgr.ParametersStatus(run).State)
	numerical := run.Resolved()
	numerical.AutoMemory = ""
	run.SetResolved(numerical)
	assert.Contains(t, mgr.ParametersStatus(run).Changed, "parameters.memory-fraction", "numeric-to-auto must remain visible")
}

func TestAutoMemoryRejectsAmbiguousSpellingsAndNormalizesDeviceCount(t *testing.T) {
	mgr := &ProviderAppManager{memoryBudget: func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "memory-fraction", DeviceCountParameter: "tensor-parallel-size"}, nil
	}}
	for _, params := range []map[string]string{
		{"memory-fraction": "auto", "memory_fraction": "0.9"},
		{"memory-fraction": "0.9", "memory_fraction": "auto"},
		{"memory-fraction": "auto", "tensor-parallel-size": "1", "tensor_parallel_size": "2"},
	} {
		_, _, err := mgr.filterLaunchAuto("generic", params)
		require.ErrorContains(t, err, "ambiguous")
		require.ErrorIs(t, err, process.ErrMemoryBudgetInvalid)
	}
	params, key, err := mgr.filterLaunchAuto("generic", map[string]string{"memory-fraction": "auto", "tensor_parallel_size": "2"})
	require.NoError(t, err)
	assert.Equal(t, "memory-fraction", key)
	assert.Equal(t, "2", params["tensor-parallel-size"])
	assert.NotContains(t, params, "tensor_parallel_size")
}

func TestAutoMemoryNumericAliasesRemainUnchanged(t *testing.T) {
	mgr := &ProviderAppManager{memoryBudget: func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "gpu-memory-utilization", DeviceCountParameter: "tensor-parallel-size"}, nil
	}}
	original := map[string]string{"gpu_memory_utilization": "0.7", "tensor_parallel_size": "2"}
	params, key, err := mgr.filterLaunchAuto("generic", original)
	require.NoError(t, err)
	assert.Empty(t, key)
	assert.Equal(t, original, params)
	assert.NotContains(t, params, "gpu-memory-utilization")
}

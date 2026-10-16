package prov_apps

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/discovery/hardware"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func capacityFixture(t *testing.T) (*ProviderAppManager, EnsureRequest, string) {
	t.Helper()
	m, svc, dir := featureLaunchFixture(t)
	t.Setenv("CUDA_VISIBLE_DEVICES", "GPU-A")
	require.NoError(t, os.Truncate(filepath.Join(dir, "weights.gguf"), 1<<30))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"num_hidden_layers":32,"num_key_value_heads":8,"num_attention_heads":32,"hidden_size":4096,"torch_dtype":"float16"}`), 0600))
	svc.Defaults.Metadata = &config.AppMetadata{ContextParam: "max-model-len"}
	svc.Defaults.Parameters = map[string]string{"max-model-len": "2048", "gpu-memory-utilization": "auto"}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
	m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "gpu-memory-utilization"}, nil
	})
	m.memoryInventory = func(context.Context) []gpu.CardMetrics {
		return []gpu.CardMetrics{{UUID: "GPU-A", TotalMemoryMiB: 10000}}
	}
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 0}}, nil
	}
	return m, EnsureRequest{Provider: "vllm", Model: "vendor/model"}, dir
}

func TestPreviewCapacityHasNoSideEffects(t *testing.T) {
	m, req, _ := capacityFixture(t)
	m.idleEvictionDefault = true
	observations := 0
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		observations++
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 0}}, nil
	}
	held := instance.NewInstance("held", "vllm", "other", 0, -1, 1)
	held.SetStatus(instance.StatusRunning)
	held.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 9000}})
	require.NoError(t, m.instances.Register(held))
	m.seams.beforeLaunch = func() { t.Error("preview launched a process") }
	req.Parameters = map[string]string{"gpu-memory-utilization": "auto"}
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}
	params, env := maps.Clone(req.Parameters), maps.Clone(req.EnvVars)
	before, err := json.Marshal(m.AppsConfig())
	require.NoError(t, err)
	preview, err := m.PreviewLaunch(t.Context(), req, 8100)
	require.NoError(t, err)
	assert.Equal(t, "fit", preview.Assessment.Status)
	assert.Equal(t, m.AssessCapacity(t.Context(), req), preview.Assessment)
	assert.Equal(t, "auto", preview.Params["gpu-memory-utilization"])
	assert.Equal(t, params, req.Parameters)
	assert.Equal(t, env, req.EnvVars)
	after, err := json.Marshal(m.AppsConfig())
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
	assert.Len(t, m.instances.List(), 1)
	assert.Equal(t, instance.StatusRunning, held.GetStatus())
	assert.Equal(t, map[string]int64{"GPU-A": 9000}, held.Resolved().Commitments)
	assert.Empty(t, m.memoryAdmission)
	assert.Zero(t, observations, "capacity reads use cached totals")
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	_, err = m.admitMemory(t.Context(), features)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit, "capacity fit must not bypass current-load admission")
	assert.Equal(t, 1, observations)
}

func TestPreviewLaunchPreservesPartialResultOnPrepareError(t *testing.T) {
	m, req, _ := capacityFixture(t)
	cause := errors.New("schema unavailable")
	m.providerSchema = func(string) (*schema.ProviderSchema, error) { return nil, cause }
	m.memoryInventory = func(context.Context) []gpu.CardMetrics {
		t.Fatal("failed preparation must not assess capacity")
		return nil
	}
	preview, err := m.PreviewLaunch(t.Context(), req, 8100)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, req.Model, preview.Model)
	assert.Empty(t, preview.Command)
	assert.Empty(t, preview.Args)
	assert.Empty(t, preview.WorkingDir)
	assert.Nil(t, preview.Params)
	assert.Nil(t, preview.Sources)
	assert.Nil(t, preview.Environment)
	assert.Empty(t, preview.Assessment.Status)
}

func TestCapacityCPUOnlyUnknownDocumented(t *testing.T) {
	m, req, _ := capacityFixture(t)
	m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryLayers, Parameter: "gpu-layers"}, nil
	})
	m.memoryInventory = func(context.Context) []gpu.CardMetrics { return nil }
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) { return nil, process.ErrMemoryObservation }
	m.memorySystem = func(context.Context) *hardware.HardwareInfo { return nil }
	assessment := m.AssessCapacity(t.Context(), req)
	assert.Equal(t, "unknown", assessment.Status)
	assert.Equal(t, process.MemoryEvidenceUnavailable, assessment.UnknownCause)
	assert.Equal(t, int64(1024), assessment.WeightsMiB)
	assert.Zero(t, assessment.RequiredMiB)
	assert.Zero(t, assessment.CapacityMiB)
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	assessment, err = m.checkMemory(t.Context(), features, false)
	require.NoError(t, err, "CPU launch keeps its existing observation bypass")
	assert.Equal(t, "unknown", assessment.Status)
}

func TestCapacityMissingUnifiedTotalIsUnknown(t *testing.T) {
	m, req, _ := capacityFixture(t)
	m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryUnified}, nil
	})
	m.memorySystem = func(context.Context) *hardware.HardwareInfo { return nil }
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "unified"}
	assert.Equal(t, "unknown", m.AssessCapacity(t.Context(), req).Status)
}

func TestCompatibleNodesCachedOversizeNotAffirmative(t *testing.T) {
	m, req, dir := capacityFixture(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "config.json")))
	require.NoError(t, os.Truncate(filepath.Join(dir, "weights.gguf"), 10001*1024*1024))
	assessment := m.AssessCapacity(t.Context(), req)
	assert.Equal(t, "no_fit", assessment.Status)
	assert.Equal(t, int64(10001), assessment.RequiredMiB)
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	_, err = m.admitMemory(t.Context(), features)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
	assert.Equal(t, "reachable memory", fit.Device)
	assert.Empty(t, m.instances.List())
}

func TestCapacityUnknownPreparationAndObservation(t *testing.T) {
	m, req, _ := capacityFixture(t)
	m.providerSchema = func(string) (*schema.ProviderSchema, error) { return nil, errors.New("schema missing") }
	assessment := m.AssessCapacity(t.Context(), req)
	assert.Equal(t, "unknown", assessment.Status)
	assert.Equal(t, process.AssessmentFailed, assessment.UnknownCause)
	m.providerSchema = func(string) (*schema.ProviderSchema, error) { return &schema.ProviderSchema{}, nil }
	assessment = m.AssessCapacity(t.Context(), req)
	assert.Equal(t, "unknown", assessment.Status)
	assert.Equal(t, process.BudgetNotSized, assessment.UnknownCause)
	assert.Equal(t, int64(1024), assessment.WeightsMiB)
	m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "gpu-memory-utilization"}, nil
	})
	m.memoryInventory = func(context.Context) []gpu.CardMetrics { return nil }
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) { return nil, errors.New("measurement missing") }
	assessment = m.AssessCapacity(t.Context(), req)
	assert.Equal(t, "unknown", assessment.Status)
	assert.Equal(t, process.MemoryEvidenceUnavailable, assessment.UnknownCause)
	assert.Equal(t, int64(1024), assessment.WeightsMiB)
	assert.Equal(t, int64(2048), assessment.ContextTokens)
	m.memorySystem = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{TotalRAM: 512 * 1024 * 1024}
	}
	m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryUnified}, nil
	})
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "unified"}
	assert.Equal(t, "no_fit", m.AssessCapacity(t.Context(), req).Status)
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	_, err = m.admitMemory(t.Context(), features)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
}

func TestCancelledAssessmentRetainsCollectedEvidence(t *testing.T) {
	m, req, _ := capacityFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.memoryInventory = func(context.Context) []gpu.CardMetrics {
		cancel()
		return []gpu.CardMetrics{{UUID: "GPU-A", TotalMemoryMiB: 10000}}
	}
	assessment := m.AssessCapacity(ctx, req)
	assert.Equal(t, "unknown", assessment.Status)
	assert.Equal(t, process.AssessmentFailed, assessment.UnknownCause)
	assert.Equal(t, int64(1024), assessment.WeightsMiB)
	assert.Equal(t, int64(2048), assessment.ContextTokens)
	assert.Empty(t, m.instances.List())
}

func TestCapacityIncompleteSelectedObservationIsUnknown(t *testing.T) {
	m, req, _ := capacityFixture(t)
	for _, cards := range [][]gpu.CardMetrics{
		{{UUID: "GPU-A", TotalMemoryMiB: 10000}, {UUID: "GPU-B"}},
		{{UUID: "GPU-A", TotalMemoryMiB: 10000}, {TotalMemoryMiB: 10000}},
	} {
		m.memoryInventory = func(context.Context) []gpu.CardMetrics { return cards }
		req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A,GPU-B"}
		req.Parameters = map[string]string{"tensor-parallel-size": "2"}
		m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
			return &schema.MemoryBudget{Kind: schema.MemoryFractionTotal, Parameter: "gpu-memory-utilization", DeviceCountParameter: "tensor-parallel-size"}, nil
		})
		assert.Equal(t, "unknown", m.AssessCapacity(t.Context(), req).Status)
		req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-A"}
		req.Parameters = map[string]string{"tensor-parallel-size": "1"}
		assert.Equal(t, "fit", m.AssessCapacity(t.Context(), req).Status)
	}
}

func TestCapacityLayersUseUnifiedPool(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("Apple unified pool")
	}
	m, req, _ := capacityFixture(t)
	m.memoryInventory = func(context.Context) []gpu.CardMetrics { return nil }
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) { return nil, process.ErrMemoryObservation }
	m.memorySystem = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{TotalRAM: 512 * 1024 * 1024}
	}
	m.providerSchema = memoryBudgetSchema(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: schema.MemoryLayers, Parameter: "gpu-layers"}, nil
	})
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "unified"}
	assessment := m.AssessCapacity(t.Context(), req)
	assert.Equal(t, "no_fit", assessment.Status)
	assert.Equal(t, int64(1024), assessment.RequiredMiB)
	assert.Equal(t, int64(512), assessment.CapacityMiB)
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	_, err = m.admitMemory(t.Context(), features)
	var fit *process.MemoryFitError
	require.ErrorAs(t, err, &fit)
}

func TestCapacityPreservesEnvironmentValidation(t *testing.T) {
	m, req, _ := capacityFixture(t)
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	features.Environment = map[string]string{"LD_PRELOAD": "injected"}
	for _, capacity := range []bool{false, true} {
		assessment, err := m.checkMemory(t.Context(), features, capacity)
		require.ErrorIs(t, err, process.ErrDangerousEnvVar)
		assert.NotErrorIs(t, err, process.ErrMemoryObservation)
		assert.Equal(t, "unknown", assessment.Status)
	}
}

func autoCapacityFixture(t *testing.T) (*ProviderAppManager, EnsureRequest) {
	t.Helper()
	m, req, dir := capacityFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"num_hidden_layers":32,"num_key_value_heads":8,"num_attention_heads":32,"hidden_size":4096,"torch_dtype":"float16","max_position_embeddings":131072}`), 0600))
	base := m.providerSchema
	m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
		declared, err := base(provider)
		shape := declared.Parameters["max-model-len"]
		shape.Auto = "auto"
		shape.AutoMinContextTokens = 4096
		declared.Parameters["max-model-len"] = shape
		shape.AutoMinContextTokens = 8192
		declared.Endpoints = map[string]schema.EndpointSchema{"embeddings": {Parameters: map[string]schema.ParamShape{"max-model-len": shape}}}
		return declared, err
	}
	req.Parameters = map[string]string{"max-model-len": "auto", "gpu-memory-utilization": "auto"}
	return m, req
}

func TestMinimumContextUsesEndpointSchema(t *testing.T) {
	m, req := autoCapacityFixture(t)
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, nil
	}
	for _, tc := range []struct {
		endpoint Endpoint
		tokens   int64
	}{{"chat", 4096}, {"embeddings", 8192}} {
		req.Endpoint = tc.endpoint
		features, err := m.ResolveLaunchParameters(t.Context(), req)
		require.NoError(t, err)
		preview, err := m.PreviewLaunch(t.Context(), req, 8100)
		require.NoError(t, err)
		admitted, err := m.checkMemory(t.Context(), features, false)
		require.NoError(t, err)
		assert.Equal(t, tc.tokens, preview.Assessment.ContextTokens)
		assert.Equal(t, preview.Assessment, admitted)
		assert.Equal(t, "auto", preview.Params["max-model-len"])
		assert.Equal(t, "auto", preview.Params["gpu-memory-utilization"])
		assert.Equal(t, "auto", features.Params["max-model-len"])
		assert.Equal(t, "0.7952", features.Params["gpu-memory-utilization"])
	}
}
func TestAutoContextCapacityAndAdmissionDifferOnlyByObservation(t *testing.T) {
	m, req := autoCapacityFixture(t)
	capacity := m.AssessCapacity(t.Context(), req)
	require.Equal(t, "fit", capacity.Status)
	features, err := m.ResolveLaunchParameters(t.Context(), req)
	require.NoError(t, err)
	admission, err := m.checkMemory(t.Context(), features, false)
	require.Error(t, err)
	assert.Equal(t, "no_fit", admission.Status)
	assert.Equal(t, capacity.ContextTokens, admission.ContextTokens)
	assert.Equal(t, "auto", features.Params["gpu-memory-utilization"])
}
func TestFixedBudgetShortfallDoesNotEvictOrLaunch(t *testing.T) {
	for _, context := range []string{"auto", "4096"} {
		t.Run(context, func(t *testing.T) {
			m, req := autoCapacityFixture(t)
			m.idleEvictionDefault = true
			req.Parameters["gpu-memory-utilization"] = "0.6"
			req.Parameters["max-model-len"] = context
			held := instance.NewInstance("idle-holder", "vllm", "other-model", 0, 0, 1)
			held.SetStatus(instance.StatusRunning)
			held.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 9000}})
			require.NoError(t, m.instances.Register(held))
			m.seams.beforeLaunch = func() { t.Error("refused launch started a process") }
			features, err := m.ResolveLaunchParameters(t.Context(), req)
			require.NoError(t, err)
			before := maps.Clone(features.Params)
			evicted, err := m.admitMemory(t.Context(), features)
			require.Error(t, err)
			assert.Empty(t, evicted)
			assert.Equal(t, before, features.Params)
			assert.Equal(t, instance.StatusRunning, held.GetStatus())
			_, err = m.EnsureRun(t.Context(), req)
			var refusal *process.MemoryFitError
			require.ErrorAs(t, err, &refusal)
			assert.True(t, refusal.Sized)
			assert.Equal(t, instance.StatusRunning, held.GetStatus())
		})
	}
}

func TestUnifiedProviderMetadataFraction(t *testing.T) {
	m, req := autoCapacityFixture(t)
	svc, _ := m.AppsConfig().LookupApp("vllm")
	svc.Defaults.Metadata.ContextParam = ""
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("vllm", svc))
	m.ReloadConfig(cfg)
	fraction := 0.75
	m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
		declared, errs := schema.Merge(provider, &schema.YAMLSchema{AutoContext: schema.AutoContext{AutoMinContextTokens: 4096}, MemoryUsableFraction: &fraction, Diagnostics: &schema.Diagnostics{Runtime: "test", Runtimes: map[string]schema.RuntimeChecks{"test": {Kernels: "unknown"}}, Memory: &schema.MemoryBudget{Kind: schema.MemoryUnified}}})
		require.Empty(t, errs)
		return declared, nil
	}
	m.memorySystem = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{TotalRAM: 10000 * 1024 * 1024}
	}
	req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "unified"}
	var originalEnvironment map[string]string
	for _, f := range []float64{0.75, 0.6} {
		fraction = f
		preview, err := m.PreviewLaunch(t.Context(), req, 8100)
		require.NoError(t, err)
		features, err := m.ResolveLaunchParameters(t.Context(), req)
		require.NoError(t, err)
		if originalEnvironment == nil {
			originalEnvironment = maps.Clone(features.Environment)
		}
		assert.Equal(t, originalEnvironment, features.Environment, "schema fraction never changes recorded engine environment")
		admission, err := m.checkMemory(t.Context(), features, false)
		assert.Equal(t, preview.Assessment, admission)
		assert.Equal(t, "auto", admission.ContextMode)
		assert.Equal(t, int64(4096), admission.ContextTokens)
		if fraction == 0.75 {
			require.NoError(t, err)
			assert.Equal(t, "fit", admission.Status)
		} else {
			require.Error(t, err)
			assert.Equal(t, "no_fit", admission.Status)
			m.seams.beforeLaunch = func() { t.Error("unified context refusal started a process") }
			_, err = m.EnsureRun(t.Context(), req)
			var refusal *process.MemoryFitError
			require.ErrorAs(t, err, &refusal)
			assert.True(t, refusal.Sized)
		}
	}
}

func TestNativeContextAdmissionAllowsEngineAdaptation(t *testing.T) {
	m, req, dir := capacityFixture(t)
	var b bytes.Buffer
	write := func(v any) { require.NoError(t, binary.Write(&b, binary.LittleEndian, v)) }
	text := func(v string) { write(uint64(len(v))); b.WriteString(v) }
	for _, v := range []any{uint32(0x46554747), uint32(3), uint64(0), uint64(6)} {
		write(v)
	}
	text("general.architecture")
	write(uint32(8))
	text("llama")
	for key, value := range map[string]uint32{"block_count": 32, "context_length": 32768, "embedding_length": 4096, "attention.head_count": 32, "attention.head_count_kv": 8} {
		text("llama." + key)
		write(uint32(4))
		write(value)
	}
	f, err := os.OpenFile(filepath.Join(dir, "weights.gguf"), os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteAt(b.Bytes(), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	base := m.providerSchema
	m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
		declared, err := base(provider)
		declared.Diagnostics.Memory = &schema.MemoryBudget{Kind: schema.MemoryLayers}
		shape := declared.Parameters["max-model-len"]
		shape.Auto, shape.AutoContextSource = "auto", "native"
		declared.Parameters["max-model-len"] = shape
		return declared, err
	}
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, nil
	}
	m.memorySystem = func(context.Context) *hardware.HardwareInfo {
		return &hardware.HardwareInfo{TotalRAM: 1024 * 1024}
	}
	for _, context := range []string{"auto", "32768"} {
		req.Parameters = map[string]string{"max-model-len": context, "gpu-memory-utilization": "0.95"}
		preview, err := m.PreviewLaunch(t.Context(), req, 8100)
		require.NoError(t, err)
		features, err := m.ResolveLaunchParameters(t.Context(), req)
		require.NoError(t, err)
		assessment, err := m.checkMemory(t.Context(), features, false)
		assert.Equal(t, preview.Assessment, assessment)
		if context == "auto" {
			require.NoError(t, err)
			assert.Equal(t, "unknown", assessment.Status)
			assert.Contains(t, assessment.Reason, "engine may adapt")
			_, err = m.admitMemory(t.Context(), features)
			require.NoError(t, err)
		} else {
			assert.Equal(t, "no_fit", assessment.Status)
			var fit *process.MemoryFitError
			require.ErrorAs(t, err, &fit)
			assert.True(t, fit.Sized)
			m.seams.beforeLaunch = func() { t.Error("numeric native-context shortfall launched a process") }
			_, err = m.EnsureRun(t.Context(), req)
			require.ErrorAs(t, err, &fit)
		}
	}
}

func TestKVCacheRolesReachPreviewAndAdmission(t *testing.T) {
	m, req := autoCapacityFixture(t)
	base := m.providerSchema
	m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
		declared, err := base(provider)
		declared.Parameters["kv-cache-dtype"] = schema.ParamShape{Kind: schema.ParamString, KVCacheDtype: "both"}
		return declared, err
	}
	m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 10000}}, nil
	}
	for _, dtype := range []string{"auto", "fp8", "future-dtype"} {
		req.Parameters = map[string]string{"max-model-len": "4096", "gpu-memory-utilization": "0.6", "kv-cache-dtype": dtype}
		preview, err := m.PreviewLaunch(t.Context(), req, 8100)
		require.NoError(t, err)
		features, err := m.ResolveLaunchParameters(t.Context(), req)
		require.NoError(t, err)
		assessment, err := m.checkMemory(t.Context(), features, false)
		assert.Equal(t, preview.Assessment, assessment)
		switch dtype {
		case "auto":
			require.Error(t, err)
			assert.Equal(t, "no_fit", assessment.Status)
		case "fp8":
			require.NoError(t, err)
			assert.Equal(t, "fit", assessment.Status)
		default:
			require.NoError(t, err)
			assert.Equal(t, "unknown", assessment.Status)
		}
	}
}

func TestHybridAssessmentMatchesPreviewAndAdmission(t *testing.T) {
	for _, kind := range []string{schema.MemoryFractionTotal, schema.MemoryLayers, schema.MemoryUnified} {
		t.Run(kind, func(t *testing.T) {
			m, req, dir := capacityFixture(t)
			layers := make([]string, 64)
			for i := range layers {
				layers[i] = "linear_attention"
				if (i+1)%4 == 0 {
					layers[i] = "full_attention"
				}
			}
			data, err := json.Marshal(map[string]any{"num_hidden_layers": 64, "num_attention_heads": 24,
				"num_key_value_heads": 4, "head_dim": 256, "dtype": "bfloat16", "max_position_embeddings": 262144,
				"linear_conv_kernel_dim": 4, "linear_key_head_dim": 128, "linear_value_head_dim": 128,
				"linear_num_key_heads": 16, "linear_num_value_heads": 48, "layer_types": layers, "full_attention_interval": 4})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0600))
			fraction := .75
			m.providerSchema = func(provider string) (*schema.ProviderSchema, error) {
				y, err := schema.LoadYAMLSchema([]byte("recurrent_state_rows: 2\nparameters:\n  max-model-len: {auto: auto, auto_min_context_tokens: 4096}\n"))
				require.NoError(t, err)
				y.Diagnostics = &schema.Diagnostics{Runtime: "test", Runtimes: map[string]schema.RuntimeChecks{"test": {Kernels: "unknown"}}, Memory: &schema.MemoryBudget{Kind: kind, Parameter: "gpu-memory-utilization"}}
				if kind == schema.MemoryUnified {
					y.MemoryUsableFraction = &fraction
				}
				declared, errs := schema.Merge(provider, y)
				require.Empty(t, errs)
				return declared, nil
			}
			total := int64(10000)
			m.memoryInventory = func(context.Context) []gpu.CardMetrics {
				return []gpu.CardMetrics{{UUID: "GPU-A", TotalMemoryMiB: total}}
			}
			m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
				return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: total, FreeMiB: total}}, nil
			}
			m.memorySystem = func(context.Context) *hardware.HardwareInfo {
				return &hardware.HardwareInfo{TotalRAM: total * 1024 * 1024}
			}
			req.Parameters = map[string]string{"max-model-len": "4096", "gpu-memory-utilization": "0.75"}
			if kind == schema.MemoryUnified {
				req.EnvVars = map[string]string{"CUDA_VISIBLE_DEVICES": "unified"}
			}
			for _, want := range []string{"fit", "no_fit"} {
				if want == "no_fit" {
					total = 3000
				}
				preview, err := m.PreviewLaunch(t.Context(), req, 8100)
				require.NoError(t, err)
				assert.Equal(t, want, preview.Assessment.Status)
				assert.Equal(t, m.AssessCapacity(t.Context(), req), preview.Assessment)
				features, err := m.ResolveLaunchParameters(t.Context(), req)
				require.NoError(t, err)
				admission, err := m.checkMemory(t.Context(), features, false)
				assert.Equal(t, preview.Assessment, admission)
				if want == "fit" {
					require.NoError(t, err)
				} else {
					var fit *process.MemoryFitError
					require.ErrorAs(t, err, &fit)
					assert.True(t, fit.Sized)
					m.seams.beforeLaunch = func() { t.Error("hybrid refusal launched a process") }
					_, err = m.EnsureRun(t.Context(), req)
					require.ErrorAs(t, err, &fit)
				}
			}
		})
	}
}

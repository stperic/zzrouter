package prov_apps

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/host/inventory"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/port"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func reservationRig(t *testing.T, eviction bool) *contractRig {
	t.Helper()
	r := newContractRig(t, false, WithIdleMemoryEviction(eviction), WithMemoryBudget(func(provider string) (*schema.MemoryBudget, error) {
		if provider == "generic" {
			return &schema.MemoryBudget{Kind: "reserve_mib", Parameter: "gpu-reserve-mib", ComplementParameter: "reserve-vram"}, nil
		}
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	}))
	fresh := &config.AppsConfig{}
	for _, name := range []string{"generic", "other"} {
		svc, _ := r.m.AppsConfig().LookupApp(name)
		if name == "generic" {
			svc.RunScope, svc.Protocol, svc.Capabilities = config.RunScopeNode, "", nil
			svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"gpu-reserve-mib": "9000"}}
		}
		require.NoError(t, fresh.AddApp(name, svc))
	}
	r.m.ReloadConfig(fresh)
	r.m.memoryInventory = func(context.Context) []gpu.CardMetrics {
		return []gpu.CardMetrics{{UUID: "GPU-A", TotalMemoryMiB: 24576}}
	}
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		device := gpu.MemoryDevice{UUID: "GPU-A", TotalMiB: 24576, FreeMiB: 24576}
		if run, ok := r.m.instances.GetByKey(instance.NewKey("generic", "", "")); ok && run.GetProcessID() != 0 {
			device.FreeMiB = 22528
			device.Processes = []gpu.MemoryProcess{{PID: run.GetProcessID(), MemoryMiB: new(int64(2048))}}
		}
		return []gpu.MemoryDevice{device}, nil
	}
	return r
}

func TestReservationLaunchPreviewAndStaleness(t *testing.T) {
	r := reservationRig(t, false)
	preview, err := r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "generic"}, 8740)
	require.NoError(t, err)
	require.NotContains(t, preview.Args, "--gpu-reserve-mib")
	require.True(t, slices.Contains(preview.Args, "--reserve-vram"))
	require.Contains(t, preview.Args, "15")
	require.Equal(t, "GPU-A", preview.Environment["CUDA_VISIBLE_DEVICES"])
	result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
	require.NoError(t, err)
	awaitRunning(t, result.Run)
	resolved := result.Run.Resolved()
	require.Equal(t, preview.Params, resolved.Parameters)
	require.Equal(t, map[string]int64{"GPU-A": 9000}, resolved.Commitments)
	require.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), result.Run).State)
	require.NoError(t, r.m.StopInstance(t.Context(), result.Run.ID))
	require.Empty(t, result.Run.Resolved().Commitments)
}

func TestReservationProtectsBudgetAndReclaimsIdleTenant(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "evicted"}[enabled], func(t *testing.T) {
			r := reservationRig(t, enabled)
			studio, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
			require.NoError(t, err)
			awaitRunning(t, studio.Run)
			llm, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "other", Model: "llm", Parameters: map[string]string{"budget": "0.75"}})
			if !enabled {
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				require.Equal(t, int64(15576), fit.FreeMiB)
				require.Contains(t, fit.Error(), studio.Run.ID)
				return
			}
			require.NoError(t, err)
			awaitRunning(t, llm.Run)
			require.Equal(t, []instance.EvictedRun{{RunID: studio.Run.ID, Provider: "generic", FreedMiB: 9000}}, llm.Run.ToInfo("node").EvictedRuns)
			require.Equal(t, instance.StatusStopped, studio.Run.GetStatus())
		})
	}
}

func TestReservationReclaimsLLMHoldersOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "evicted"}[enabled], func(t *testing.T) {
			f := newReclaimFixture(t, enabled)
			f.mgr.memoryBudget = func(string) (*schema.MemoryBudget, error) {
				return &schema.MemoryBudget{Kind: "reserve_mib", Parameter: "gpu-reserve-mib"}, nil
			}
			f.features.Params = map[string]string{"gpu-reserve-mib": "6000"}
			f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
				device := f.device
				device.Processes = nil
				for _, run := range []*instance.Instance{f.older, f.newer} {
					if run.GetStatus() == instance.StatusStopped {
						device.FreeMiB += 4500
					} else {
						device.Processes = append(device.Processes, gpu.MemoryProcess{PID: run.GetProcessID(), MemoryMiB: new(int64(4500))})
					}
				}
				return []gpu.MemoryDevice{device}, nil
			}
			evicted, err := f.admit(t)
			if !enabled {
				require.Error(t, err)
				require.Contains(t, err.Error(), f.older.ID)
				require.Contains(t, err.Error(), f.newer.ID)
				require.Empty(t, evicted)
			} else {
				require.NoError(t, err)
				require.Len(t, evicted, 2)
			}
		})
	}
}

func TestNodeProviderLaunchRestrictions(t *testing.T) {
	r := reservationRig(t, false)
	fresh := &config.AppsConfig{}
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Platforms = []config.InstallPlatform{{OS: "unsupported"}}
	require.NoError(t, fresh.AddApp("generic", svc))
	r.m.ReloadConfig(fresh)
	_, err := r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "generic"}, 8740)
	require.ErrorIs(t, err, ErrUnsupportedPlatform)
	require.Contains(t, err.Error(), "not supported")
	svc.Platforms = nil
	svc.Requirements = &config.AppRequirements{GPU: &config.GPURequirement{Vendor: "nvidia", Policy: "required"}}
	fresh = &config.AppsConfig{}
	require.NoError(t, fresh.AddApp("generic", svc))
	r.m.ReloadConfig(fresh)
	emptyTools := t.TempDir()
	t.Setenv("PATH", emptyTools)
	_, err = r.m.PreviewLaunch(t.Context(), EnsureRequest{Provider: "generic"}, 8740)
	require.ErrorIs(t, err, ErrUnsupportedPlatform)
	require.Contains(t, err.Error(), "GPU")
}

func TestQueuedReservationEndsWithoutLaunch(t *testing.T) {
	for _, scenario := range []string{"weights fail", "cancelled after admission"} {
		t.Run(scenario, func(t *testing.T) {
			r := reservationRig(t, false)
			req := EnsureRequest{Provider: "generic"}
			if scenario == "weights fail" {
				root := t.TempDir()
				old, err := modelregistry.GetModelsRootDir()
				require.NoError(t, err)
				modelregistry.SetModelsRootDirOverride(root)
				t.Cleanup(func() { modelregistry.SetModelsRootDirOverride(old) })
				dir := filepath.Join(root, "incomplete")
				require.NoError(t, os.MkdirAll(dir, 0700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0600))
				req.Model = "incomplete"
				fresh := &config.AppsConfig{}
				svc, _ := r.m.AppsConfig().LookupApp("generic")
				svc.RunScope, svc.Protocol = "", config.ProtocolOpenAI
				svc.Capabilities = &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}
				svc.Runtime.Execution.Args = []string{"--model", "${MODEL_PATH}"}
				require.NoError(t, fresh.AddApp("generic", svc))
				r.m.ReloadConfig(fresh)
			}
			selection, err := r.m.resolveLaunch(&req)
			require.NoError(t, err)
			run, err := r.m.registerRun(t.Context(), req, selection)
			require.NoError(t, err)
			if scenario == "cancelled after admission" {
				r.m.seams.admitted = func() { require.NotEmpty(t, run.Resolved().Commitments); run.Cancel() }
			}
			if scenario == "weights fail" {
				_, err := r.m.prepareRun(t.Context(), run, req, selection)
				require.ErrorIs(t, err, process.ErrModelIncomplete)
				require.Empty(t, run.Resolved().Commitments)
			}
			r.m.awaitAdmission(run, req)
			require.Zero(t, run.GetProcessID())
			require.Empty(t, run.Resolved().Commitments)
			for _, ref := range r.m.memoryRuns() {
				require.Empty(t, ref.Commitments)
			}
			if scenario == "weights fail" {
				require.ErrorIs(t, run.FailureCause(), process.ErrModelIncomplete)
			}
		})
	}
}

func TestUnknownBudgetIgnoresOtherDeviceCommitment(t *testing.T) {
	f := newReclaimFixture(t, true)
	f.mgr.memoryBudget = func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "layers", Parameter: "gpu-layers"}, nil
	}
	f.older.SetResolved(instance.Resolved{Commitments: map[string]int64{"GPU-A": 4000}})
	f.features.Environment = map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-B"}
	f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 24000, FreeMiB: 24000}, {UUID: "GPU-B", TotalMiB: 24000, FreeMiB: 24000}}, nil
	}
	evicted, err := f.admit(t)
	require.NoError(t, err)
	require.Empty(t, evicted)
	require.Equal(t, instance.StatusRunning, f.older.GetStatus())
}

func TestComfySingletonPortRefusalNamesProvider(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "assigned", true: "external"}[external], func(t *testing.T) {
			r := reservationRig(t, false)
			svc, _ := r.m.AppsConfig().LookupApp("generic")
			svc.Name = "comfyui"
			svc.Runtime.PortRange = []int{8188, 8188}
			svc.Runtime.BasePort = 8188
			fresh := &config.AppsConfig{}
			require.NoError(t, fresh.AddApp("comfyui", svc))
			r.m.ReloadConfig(fresh)
			require.NoError(t, r.m.ports.EnsurePool("comfyui", svc))
			if external {
				listener, err := net.Listen("tcp", "127.0.0.1:8188")
				require.NoError(t, err)
				defer listener.Close()
				if !port.IsPortInUse(8188) {
					t.Skip("sandbox hides listening processes from gopsutil")
				}
			} else {
				allocated, err := r.m.ports.AllocateForProvider("comfyui", 0)
				require.NoError(t, err)
				require.Equal(t, 8188, allocated)
				defer r.m.ports.ReleaseForProvider("comfyui", allocated)
			}
			_, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "comfyui"})
			require.ErrorContains(t, err, "comfyui")
			require.ErrorContains(t, err, "8188")
			require.ErrorContains(t, err, "no available ports")
		})
	}
}

func TestReservationPreparationUsesCachedTotals(t *testing.T) {
	r := reservationRig(t, false)
	tools := t.TempDir()
	counter := filepath.Join(tools, "calls")
	script := "#!/bin/sh\nprintf 'call\n' >> '" + counter + "'\nprintf 'GPU-A, 00000000:01:00.0, 24576, 0, 24576, 0\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(tools, "nvidia-smi"), []byte(script), 0700))
	t.Setenv("PATH", tools)
	cache := inventory.NewCache(time.Hour)
	r.m.memoryInventory = cache.GetGPUCardMetrics
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		t.Fatal("preparation queried live admission memory")
		return nil, nil
	}
	fresh := &config.AppsConfig{}
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Requirements = &config.AppRequirements{GPU: &config.GPURequirement{Vendor: "nvidia", Policy: "required"}}
	require.NoError(t, fresh.AddApp("generic", svc))
	r.m.ReloadConfig(fresh)
	ctx := gpu.WithCachedInventory(t.Context(), gpu.Inventory{Vendors: map[gpu.Vendor]gpu.Detection{gpu.VendorNVIDIA: {Vendor: gpu.VendorNVIDIA, State: gpu.StateDriverOK}}})
	req := EnsureRequest{Provider: "generic"}
	for range 3 {
		preview, err := r.m.PreviewLaunch(ctx, req, 8740)
		require.NoError(t, err)
		require.Equal(t, "15", preview.Params["reserve-vram"])
		require.Equal(t, "GPU-A", preview.Environment["CUDA_VISIBLE_DEVICES"])
	}
	prepared, err := processEngine{r.m}.prepare(ctx, svc, "generic", req, 8740)
	require.NoError(t, err)
	run, err := r.m.newRun(ctx, req, svc, 8740)
	require.NoError(t, err)
	run.SetResolved(prepared.resolved)
	run.MarkRunning(false, nil)
	defer run.Cancel()
	for range 3 {
		status := r.m.ParametersStatus(ctx, run)
		require.Equal(t, instance.ParametersCurrent, status.State, status.Error)
	}
	calls, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(calls), "call"))
}

func TestReservationPreparationPropagatesContext(t *testing.T) {
	r := reservationRig(t, false)
	type operationKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), operationKey{}, "operation"))
	defer cancel()
	r.m.memoryInventory = func(got context.Context) []gpu.CardMetrics {
		require.Equal(t, "operation", got.Value(operationKey{}))
		cancel()
		return []gpu.CardMetrics{{UUID: "GPU-A", TotalMemoryMiB: 24576}}
	}
	_, err := r.m.PreviewLaunch(ctx, EnsureRequest{Provider: "generic"}, 8740)
	require.ErrorIs(t, err, context.Canceled)
}

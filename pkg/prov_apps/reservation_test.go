package prov_apps

import (
	"context"
	"slices"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
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
	preview, err := r.m.PreviewLaunch(EnsureRequest{Provider: "generic"}, 8740)
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
	require.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(result.Run).State)
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
	_, err := r.m.PreviewLaunch(EnsureRequest{Provider: "generic"}, 8740)
	require.ErrorIs(t, err, ErrUnsupportedPlatform)
	require.Contains(t, err.Error(), "not supported")
	svc.Platforms = nil
	svc.Requirements = &config.AppRequirements{GPU: &config.GPURequirement{Vendor: "nvidia", Policy: "required"}}
	fresh = &config.AppsConfig{}
	require.NoError(t, fresh.AddApp("generic", svc))
	r.m.ReloadConfig(fresh)
	emptyTools := t.TempDir()
	t.Setenv("PATH", emptyTools)
	_, err = r.m.PreviewLaunch(EnsureRequest{Provider: "generic"}, 8740)
	require.ErrorIs(t, err, ErrUnsupportedPlatform)
	require.Contains(t, err.Error(), "GPU")
}

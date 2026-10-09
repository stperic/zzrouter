package prov_apps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryAdmissionRefusesBeforeProcessAndReleasesResources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver/process fixture")
	}
	root := t.TempDir()
	smi := filepath.Join(root, "nvidia-smi")
	writeFree := func(free string) {
		t.Helper()
		require.NoError(t, os.WriteFile(smi, []byte("#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, "+free+"' ;;\n*) echo 'GPU-A, 42, 3000' ;;\nesac\n"), 0o700))
	}
	writeFree("6000")
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	marker := filepath.Join(root, "started")
	engine := filepath.Join(root, "engine")
	require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{
		Enabled: new(true), Name: "generic", Protocol: config.ProtocolOpenAI, Mode: "on-demand",
		Runtime:      &config.AppRuntimeConfig{PortRange: []int{8620, 8621}, BasePort: 8620, Execution: config.ExecutionConfig{Type: "cli", Command: engine}},
		Defaults:     &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.95"}},
		Nodes:        map[string]config.NodeSpec{"worker-1": {Parameters: map[string]string{"budget": "0.55"}}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	mgr, err := NewProviderAppManager(cfg, WithNodename(func() string { return "worker-1" }), WithMemoryBudget(func(provider string) (*schema.MemoryBudget, error) {
		assert.Equal(t, "generic", provider)
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	_, err = mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: "arbitrary", Parameters: map[string]string{"budget": "0.95"}})
	var failure *process.MemoryFitError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, int64(9500), failure.RequiredMiB)
	assert.Empty(t, mgr.instances.ListAll())
	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "refused launch must never start the engine")
	// Node tier fits without request overrides and admission releases its refused port.
	inst, err := mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: "arbitrary", Port: 8620})
	require.NoError(t, err)
	assert.Equal(t, "0.55", inst.Resolved().Parameters["budget"])
	require.NoError(t, mgr.StopInstance(t.Context(), inst.ID))
	// A subsequent model sees the changed live measurement, not a cached free total.
	writeFree("4000")
	_, err = mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: "second", Port: 8621})
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, int64(5500), failure.RequiredMiB)
	assert.Equal(t, int64(4000), failure.FreeMiB)
}

// queueFixture is a manager whose engine stays starting until release is
// called, under a fraction_total budget of 0.55 of a 10000 MiB GPU.
type queueFixture struct {
	mgr     *ProviderAppManager
	engine  string
	setFree func(free string)
}

func newQueueFixture(t *testing.T) queueFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver/process fixture")
	}
	root := t.TempDir()
	smi := filepath.Join(root, "nvidia-smi")
	setFree := func(free string) {
		require.NoError(t, os.WriteFile(smi, []byte("#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, "+free+"' ;;\nesac\n"), 0o700))
	}
	setFree("6000")
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	engine := filepath.Join(root, "engine")
	require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\ntouch \"$0.started\"\nwhile [ ! -e \"$0.ready\" ]; do /bin/sleep 0.01; done\nprintf 'READY\\n'\nexec /bin/sleep 60\n"), 0o700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{
		Enabled: new(true), Name: "generic", Protocol: config.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8630, 8633}, BasePort: 8630, Execution: config.ExecutionConfig{Type: "cli", Command: engine},
			HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{
				Timeout: "5s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}},
			}},
		},
		Defaults:     &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55"}},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	mgr, err := NewProviderAppManager(cfg, WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	return queueFixture{mgr: mgr, engine: engine, setFree: setFree}
}

// release lets every engine become ready and waits for runs to serve, so
// cleanup never stops a run between admission and its process start.
func (f queueFixture) release(t *testing.T, runs ...*instance.Instance) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.engine+".ready", nil, 0o600))
	for _, run := range runs {
		eventually(t, func() bool { return run.GetStatus() == instance.StatusRunning }, run.Model+" ready")
	}
}

func (f queueFixture) launch(t *testing.T, model string) *instance.Instance {
	t.Helper()
	inst, err := f.mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: model})
	require.NoError(t, err)
	return inst
}

// queueHoldWindow is long enough for a queued run's waiter to have run.
const queueHoldWindow = 300 * time.Millisecond

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, 10*time.Millisecond, msg)
}

// A launch behind one still claiming GPU memory is accepted at once and
// admitted when that one leaves starting, instead of being refused.
func TestMemoryAdmissionQueuesBehindStartingRun(t *testing.T) {
	f := newQueueFixture(t)
	first := f.launch(t, "one")
	require.True(t, first.Admitted())
	second := f.launch(t, "two")
	assert.Equal(t, instance.StatusStarting, second.GetStatus())
	assert.Never(t, second.Admitted, queueHoldWindow, 10*time.Millisecond, "measured only once the first claim is visible")
	assert.Equal(t, first.ID, second.ToInfo("").WaitingOn)
	assert.Empty(t, first.ToInfo("").EvictedRuns, "nothing was evicted")

	f.release(t, first, second)
	assert.Empty(t, second.WaitingOn())
}

// The memory gate held by another admission queues the launch instead of
// blocking the caller; a cancelled caller still gets its cancellation.
func TestMemoryAdmissionQueuesWhileAdmissionLocked(t *testing.T) {
	f := newQueueFixture(t)
	f.mgr.memoryAdmission <- struct{}{}
	inst := f.launch(t, "one")
	assert.False(t, inst.Admitted())
	<-f.mgr.memoryAdmission
	eventually(t, inst.Admitted, "admitted once the gate is free")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.mgr.LaunchInstance(ctx, LaunchRequest{Provider: "generic", Model: "two"})
	assert.ErrorIs(t, err, context.Canceled)
	f.release(t, inst)
}

// A queued run its admission later refuses stays listed as failed with the
// refusal itself, so callers classify it like an inline refusal; its
// engine never starts.
func TestQueuedRefusalFailsRunWithItsError(t *testing.T) {
	f := newQueueFixture(t)
	first := f.launch(t, "one")
	second := f.launch(t, "two")
	f.setFree("4000")
	f.release(t, first)
	eventually(t, func() bool { return second.GetStatus() == instance.StatusFailed }, "refused at admission")
	var fit *process.MemoryFitError
	require.ErrorAs(t, second.FailureCause(), &fit)
	assert.Contains(t, second.GetErrorMessage(), "cannot fit")
	assert.Zero(t, second.GetProcessID(), "a refused run never starts its engine")
	require.NoError(t, f.mgr.StopInstance(t.Context(), second.ID))
}

// Stopping a queued run ends its wait and frees its port; the run it
// waited on carries on.
func TestStopQueuedRun(t *testing.T) {
	f := newQueueFixture(t)
	first := f.launch(t, "one")
	second := f.launch(t, "two")
	require.NoError(t, f.mgr.StopInstance(t.Context(), second.ID))
	_, listed := f.mgr.instances.Get(second.ID)
	assert.False(t, listed)
	assert.False(t, second.Admitted())
	third := f.launch(t, "three")
	assert.Equal(t, second.Port, third.Port, "the stopped run's port went back once")
	f.release(t, first, third)
}

// An install smoke cannot queue: its install holds the gate a queued run waits for.
func TestSmokeDoesNotQueue(t *testing.T) {
	f := newQueueFixture(t)
	first := f.launch(t, "one")
	_, err := f.mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: "two", Runtime: "generic", installSmoke: true})
	require.ErrorIs(t, err, errAdmissionBusy)
	assert.Len(t, f.mgr.instances.ListAll(), 1, "a refused smoke leaves nothing registered")
	f.release(t, first)
}

// Stopping a queued run never waits for another run's admission to finish.
func TestStopQueuedRunWhileAdmissionBusy(t *testing.T) {
	f := newQueueFixture(t)
	f.mgr.memoryAdmission <- struct{}{}
	defer func() { <-f.mgr.memoryAdmission }()
	inst := f.launch(t, "one")
	eventually(t, func() bool { return len(f.mgr.memoryAdmission) == 1 && !inst.Admitted() }, "queued behind the held slot")
	ctx, cancel := context.WithTimeout(t.Context(), queueHoldWindow)
	defer cancel()
	require.NoError(t, f.mgr.StopInstance(ctx, inst.ID), "the waiter gives up the slot wait when stopped")
}

// A launch that evicts an idle run names it on its own run record and in
// its job result, so the caller learns of the eviction where it looks.
func TestEvictingLaunchNamesEvictedRuns(t *testing.T) {
	f := newQueueFixture(t)
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(reg.Stop)
	f.mgr.jobsRegistry = reg
	f.mgr.evictIdleOnMemoryPressure = true
	idle := instance.NewInstance("idle", "generic", "idle-model", 0, -1, 10)
	idle.SetStatus(instance.StatusRunning)
	idle.SetProcessInfo(9000001, 0, nil)
	require.NoError(t, f.mgr.instances.Register(idle))
	f.mgr.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		free := int64(1000)
		if idle.GetStatus() == instance.StatusStopped {
			free = 6000
		}
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: free, Processes: []gpu.MemoryProcess{{PID: 9000001, MemoryMiB: new(int64(5000))}}}}, nil
	}

	inst := f.launch(t, "big")
	want := []instance.EvictedRun{{RunID: idle.ID, Model: idle.Model, FreedMiB: 5000}}
	assert.Equal(t, want, inst.ToInfo("").EvictedRuns)
	f.release(t, inst)
	ev := subscribeTerminal(t, reg, inst.StreamJobID, 5*time.Second)
	assert.Equal(t, jobs.PhaseDone, ev.Phase)
	assert.Equal(t, want, ev.Meta["evicted_runs"])
}

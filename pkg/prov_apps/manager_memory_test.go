package prov_apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryAdmissionBusyGateDoesNotWait(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			mgr := &ProviderAppManager{memoryBudget: func(string) (*schema.MemoryBudget, error) { return nil, nil }}
			mgr.memoryLaunchMu.Lock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				_, err := mgr.launchInstanceLocked(ctx, LaunchRequest{})
				done <- err
			}()
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			var err error
			select {
			case err = <-done:
				mgr.memoryLaunchMu.Unlock()
			case <-timer.C:
				t.Error("busy admission must refuse immediately, including cancelled callers")
				cancel()
				mgr.memoryLaunchMu.Unlock()
				err = <-done
			}
			if cancelled {
				assert.ErrorIs(t, err, context.Canceled)
			} else {
				assert.ErrorIs(t, err, ErrAtCapacity)
			}
		})
	}
}

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

func TestMemoryAdmissionSerializesDistinctStartingModels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix driver/process fixture")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte("#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 6000' ;;\nesac\n"), 0o700))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CUDA_VISIBLE_DEVICES", "0")
	engine := filepath.Join(root, "engine")
	require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\nwhile [ ! -e \"$0.ready\" ]; do /bin/sleep 0.01; done\nprintf 'READY\\n'\nexec /bin/sleep 60\n"), 0o700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{
		Enabled: new(true), Name: "generic", Protocol: config.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8630, 8632}, BasePort: 8630, Execution: config.ExecutionConfig{Type: "cli", Command: engine},
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
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, model := range []string{"one", "two"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "generic", Model: model})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	accepted, deferred := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, ErrAtCapacity)
			deferred++
		}
	}
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 1, deferred)
	runs := mgr.instances.ListAll()
	require.Len(t, runs, 1)
	// Keep admission in Starting, then await full launch publication before cleanup.
	require.NoError(t, os.WriteFile(engine+".ready", nil, 0o600))
	require.Eventually(t, func() bool {
		return runs[0].GetStatus() == instance.StatusRunning
	}, 5*time.Second, 10*time.Millisecond)
}

package prov_apps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

func TestSmokeFailureUsesExistingRollbackBeforeFinalization(t *testing.T) {
	for _, failure := range []string{"chat", "cleanup", "verify", "success", "busy", "fit", "rollback_blocked", "finalize"} {
		t.Run(failure, func(t *testing.T) {
			isolateProviderRoot(t)
			coord, _, _, cleanup := newAsyncTestCoord(t, "vllm")
			defer cleanup()
			calls := []string{}
			active, previous := filepath.Join(t.TempDir(), "active"), filepath.Join(t.TempDir(), "previous")
			require.NoError(t, os.WriteFile(active, []byte("new"), 0600))
			require.NoError(t, os.WriteFile(previous, []byte("old"), 0600))
			plan := &install.Plan{Provider: "vllm", Action: "install", Recipe: &install.RecipeSnapshot{}, Rollback: func() error { calls = append(calls, "rollback"); return os.Rename(previous, active) }}
			plan.Steps = []install.Step{{Number: 1, Verify: install.StepVerify{PreVerify: func(context.Context) error {
				calls = append(calls, "verify")
				if failure == "verify" {
					return errors.New("bad verify")
				}
				return nil
			}}}}
			coord.smoke = func(ctx context.Context, provider, runtime, model string) error {
				assert.Equal(t, "vllm", runtime)
				assert.Equal(t, "registry/model", model)
				calls = append(calls, "smoke")
				if failure == "rollback_blocked" {
					return errSmokeRollbackBlocked
				}
				if failure == "busy" {
					return errSmokeBusy
				}
				if failure == "fit" {
					return &process.MemoryFitError{Device: "GPU", RequiredMiB: 4000, FreeMiB: 2000}
				}
				if failure != "success" && failure != "finalize" {
					return errors.New(failure)
				}
				return nil
			}
			h := coord.openJob("install", "vllm")
			require.NotNil(t, h)
			defer h.Done()
			err := coord.runResolvedPlan(h, "vllm", plan, install.PlanOptions{Smoke: &install.SmokeOptions{Model: "registry/model"}}, -1, func() error {
				calls = append(calls, "finalize")
				if failure == "finalize" {
					return errors.New("finalize failed")
				}
				return nil
			})
			if failure == "rollback_blocked" {
				require.ErrorIs(t, err, errSmokeRollbackBlocked)
				assert.NotContains(t, calls, "rollback")
				assert.NotContains(t, calls, "finalize")
				h.Fail(err)
				event, getErr := coord.jobs.Get(h.ID())
				require.NoError(t, getErr)
				assert.Equal(t, "rollback_blocked", event.Meta["smoke_status"])
				for path, want := range map[string]string{active: "new", previous: "old"} {
					data, readErr := os.ReadFile(path)
					require.NoError(t, readErr)
					assert.Equal(t, want, string(data))
				}
			} else if failure == "busy" {
				require.NoError(t, err)
				h.Done() // ExecuteResolvedAsync publishes the terminal event after a nil result.
				assert.Contains(t, calls, "finalize")
				assert.NotContains(t, calls, "rollback")
				event, getErr := coord.jobs.Get(h.ID())
				require.NoError(t, getErr)
				assert.Equal(t, jobs.PhaseDone, event.Phase)
				assert.Empty(t, event.Err)
				assert.Equal(t, "skipped_busy", event.Meta["smoke_status"])
				assert.Equal(t, true, event.Meta["installed"])
				data, readErr := os.ReadFile(active)
				require.NoError(t, readErr)
				assert.Equal(t, "new", string(data))
				wire, marshalErr := json.Marshal(event)
				require.NoError(t, marshalErr)
				var fields map[string]any
				require.NoError(t, json.Unmarshal(wire, &fields))
				assert.Contains(t, fields["warning"], "Smoke skipped")
			} else if failure == "success" {
				require.NoError(t, err)
				assert.Equal(t, []string{"verify", "verify", "smoke", "finalize"}, calls)
				h.Done()
				event, getErr := coord.jobs.Get(h.ID())
				require.NoError(t, getErr)
				assert.Equal(t, "passed", event.Meta["smoke_status"])
			} else {
				require.Error(t, err)
				if failure != "verify" && failure != "finalize" {
					assert.Contains(t, err.Error(), "install smoke:")
				}
				if failure == "fit" {
					var fit *process.MemoryFitError
					assert.ErrorAs(t, err, &fit)
				}
				h.Fail(err)
				event, getErr := coord.jobs.Get(h.ID())
				require.NoError(t, getErr)
				if failure == "verify" {
					assert.NotContains(t, event.Meta, "smoke_status")
				} else if failure == "finalize" {
					assert.Equal(t, "passed", event.Meta["smoke_status"], "smoke passed before finalization failed")
				} else {
					assert.Equal(t, "failed", event.Meta["smoke_status"])
				}
				if failure != "finalize" {
					assert.NotContains(t, calls, "finalize")
				}
				assert.Equal(t, "rollback", calls[len(calls)-1])
			}
			if failure == "verify" {
				assert.NotContains(t, calls, "smoke")
			}
		})
	}
}

type smokeDrainContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *smokeDrainContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSmokeStrictStopRetainsUnconfirmedRun(t *testing.T) {
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{Name: "generic", Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Enabled: new(true), Mode: "on-demand", Protocol: config.ProtocolOpenAI, Runtime: &config.AppRuntimeConfig{BasePort: 8600, PortRange: []int{8600, 8602}, Execution: config.ExecutionConfig{Type: "cli", Command: "test-only"}}}))
	mgr, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	run := instance.NewInstance("unreaped", "generic", "model", 8600, time.Minute, 0)
	run.SetResolved(instance.Resolved{Runtime: "generic"})
	run.StopUnconfirmed.Store(true)
	run.MarkFailed("readiness failed before cleanup")
	run.TrackGoroutine()
	var done sync.Once
	release := func() { done.Do(run.GoroutineDone) }
	defer release()
	require.NoError(t, mgr.instances.Register(run))
	replacement := instance.NewInstance("replacement", "generic", "model", 8601, time.Minute, 0)
	assert.ErrorIs(t, mgr.instances.Register(replacement), instance.ErrModelAlreadyLoaded)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &smokeDrainContext{Context: parent, entered: make(chan struct{})}
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.StopInstance(ctx, run.ID) }()
	select {
	case <-ctx.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not reach process drain")
	}
	assert.ErrorIs(t, run.Context().Err(), context.Canceled, "monitor stays canceled when draining fails")
	run.MarkFailed("late readiness failure while cleanup is pending")
	assert.True(t, run.StopUnconfirmed.Load())
	assert.ErrorIs(t, mgr.instances.Register(replacement), instance.ErrModelAlreadyLoaded)
	assert.True(t, mgr.installs.runtimeInUse("generic"))
	assert.Equal(t, 1, mgr.instances.CountActiveByProvider("generic"))
	cancel()
	assert.ErrorIs(t, <-stopped, context.Canceled)
	run.FailedAt = time.Now().Add(-2 * instance.FailedInstanceTTL)
	assert.Zero(t, mgr.instances.CleanupExpiredFailures(), "unconfirmed engines cannot expire from the registry")
	release()
	require.NoError(t, mgr.StopInstance(t.Context(), run.ID))
	assert.False(t, run.StopUnconfirmed.Load())
	assert.True(t, run.GetStatus().IsTerminal())
	assert.Empty(t, mgr.instances.ListAll())
}

func TestSmokeOptionsBoundAndRestricted(t *testing.T) {
	base := install.PlanOptions{}
	one := install.PlanOptions{Smoke: &install.SmokeOptions{Model: "registry/one"}}
	two := install.PlanOptions{Smoke: &install.SmokeOptions{Model: "registry/two"}}
	assert.NotEqual(t, install.BoundPlanID("raw", "install", base), install.BoundPlanID("raw", "install", one))
	assert.NotEqual(t, install.BoundPlanID("raw", "install", one), install.BoundPlanID("raw", "install", two))
	isolateProviderRoot(t)
	coord, _, _, cleanup := newAsyncTestCoord(t, "vllm")
	defer cleanup()
	for _, model := range []string{"", "/tmp/model", "../model", "https://host/model", `C:\model`} {
		_, err := coord.PrepareInstallPlan(t.Context(), "vllm", "1", install.PlanOptions{Smoke: &install.SmokeOptions{Model: model}})
		require.Error(t, err, model)
	}
	plan := &install.Plan{Provider: "vllm", Recipe: &install.RecipeSnapshot{}}
	for _, opts := range []install.PlanOptions{one, {Smoke: one.Smoke, Disposable: true}} {
		_, err := coord.ExecuteResolvedAsync(t.Context(), "vllm", plan, opts, 1, nil)
		require.Error(t, err)
	}
}

func TestSmokeRuntimeOccupancyDoesNotBlockSibling(t *testing.T) {
	coord := &InstallCoordinator{instances: instance.NewRegistry()}
	run := instance.NewInstance("id", "mlx", "model", 8100, 0, 0)
	run.SetResolved(instance.Resolved{Runtime: "mlx-vlm"})
	require.NoError(t, coord.instances.Register(run))
	assert.True(t, coord.runtimeInUse("mlx-vlm"))
	assert.False(t, coord.runtimeInUse("mlx"))
	run.SetStatus(instance.StatusStopped)
	assert.False(t, coord.runtimeInUse("mlx-vlm"))
}

func TestSmokeChatRequiresNonEmptyActualReply(t *testing.T) {
	for _, response := range []struct {
		name, body string
		status     int
		pass       bool
	}{
		{"reply", `{"choices":[{"message":{"content":"OK"}}]}`, 200, true},
		{"reasoning", `{"choices":[{"message":{"content":"","reasoning_content":"Thinking"}}]}`, 200, true},
		{"empty reasoning", `{"choices":[{"message":{"reasoning_content":"  "}}]}`, 200, false},
		{"empty", `{"choices":[{"message":{"content":"  "}}]}`, 200, false},
		{"no choices", `{"choices":[]}`, 200, false},
		{"malformed", `{`, 200, false},
		{"http failure", `{"error":"failed"}`, 500, false},
	} {
		t.Run(response.name, func(t *testing.T) {
			mgr := &ProviderAppManager{httpClient: &http.Client{Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "127.0.0.1:8123", req.URL.Host)
				assert.Equal(t, "/v1/chat/completions", req.URL.Path)
				assert.Empty(t, req.Header.Get("X-API-Key"))
				assert.Empty(t, req.Header.Get("Authorization"))
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				assert.Contains(t, string(body), `"model":"wire-model"`)
				assert.Contains(t, string(body), `"max_tokens":128`)
				return &http.Response{StatusCode: response.status, Body: io.NopCloser(strings.NewReader(response.body)), Header: http.Header{}}, nil
			})}}
			run := instance.NewInstance("id", "any", "registry/model", 8123, time.Minute, 0)
			run.WireModel = "wire-model"
			err := mgr.smokeChat(t.Context(), run)
			if response.pass {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestSmokeChatUsesProviderDeadlineInsteadOfSharedClientTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	client := &http.Client{Timeout: time.Nanosecond, Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
		actual, present := req.Context().Deadline()
		assert.True(t, present)
		assert.Equal(t, deadline, actual)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: http.Header{}}, nil
	})}
	mgr := &ProviderAppManager{httpClient: client}
	run := instance.NewInstance("id", "any", "registry/model", 8123, time.Minute, 0)
	require.NoError(t, mgr.smokeChat(ctx, run))
	assert.Equal(t, time.Nanosecond, client.Timeout, "shared health client is unchanged")
}

func TestSmokeAdmissionBusyRetryAndExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	for _, mode := range []string{"pending", "fit", "retry", "expire-between"} {
		t.Run(mode, func(t *testing.T) {
			engine := filepath.Join(t.TempDir(), "engine")
			require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\nprintf 'READY\\n'\nexec /bin/sleep 0.4\n"), 0700))
			timeout := "25ms"
			if mode == "retry" || mode == "expire-between" {
				timeout = "2s"
			}
			cfg := &config.AppsConfig{}
			require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{Name: "generic", Enabled: new(true), Mode: "on-demand", Protocol: config.ProtocolOpenAI, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Runtime: &config.AppRuntimeConfig{PortRange: []int{8690, 8693}, BasePort: 8690, Execution: config.ExecutionConfig{Type: "cli", Command: engine}, HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: timeout, LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}}}}))
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			mgr, err := NewProviderAppManager(cfg, WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
				calls++
				if mode == "expire-between" && calls > 1 {
					cancel()
					return nil, context.Canceled
				}
				if mode == "fit" {
					return nil, &process.MemoryFitError{}
				}
				if mode == "pending" || calls == 1 {
					return nil, ErrAtCapacity
				}
				return nil, nil
			}), WithHTTPClient(&http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: http.Header{}}, nil
			})}))
			require.NoError(t, err)
			cleanupProviderManager(t, mgr)
			err = mgr.smokeInstalledRuntime(ctx, "generic", "generic", "model")
			if mode == "retry" {
				require.NoError(t, err)
				assert.Greater(t, calls, 1)
			} else if mode == "fit" {
				var fit *process.MemoryFitError
				require.ErrorAs(t, err, &fit)
				assert.NotErrorIs(t, err, errSmokeBusy)
				assert.ErrorContains(t, err, "smoke model does not fit this GPU")
				assert.Equal(t, 1, calls, "an oversized model is not retried as contention")
			} else {
				require.ErrorIs(t, err, errSmokeBusy)
			}
			assert.Empty(t, mgr.instances.ListAll())
		})
	}
}

type smokeStopObserver struct{ onClose func() }

func (p smokeStopObserver) Write(b []byte) (int, error) { return len(b), nil }
func (p smokeStopObserver) Close() error {
	p.onClose()
	return nil
}

func TestSmokeCancellationStopsOwnedRunWithoutHoldingOtherRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix short-lived engine fixture")
	}
	isolateProviderRoot(t)
	for _, mode := range []string{"success", "cancel", "timeout", "removed"} {
		t.Run(mode, func(t *testing.T) {
			engine := filepath.Join(t.TempDir(), "engine")
			require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\nprintf 'READY\\n'\nexec /bin/sleep 0.4\n"), 0700))
			cfg := &config.AppsConfig{}
			timeout := "2s"
			if mode == "timeout" {
				timeout = "30ms"
			}
			require.NoError(t, cfg.AddApp("generic", config.ServiceConfig{Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}, Name: "generic", Protocol: config.ProtocolOpenAI, Enabled: new(true), Mode: "on-demand", Runtime: &config.AppRuntimeConfig{PortRange: []int{8610, 8612}, BasePort: 8610, Execution: config.ExecutionConfig{Type: "cli", Command: engine}, HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: timeout, LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}}}}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var once sync.Once
			var mgr *ProviderAppManager
			var err error
			mgr, err = NewProviderAppManager(cfg, WithHTTPClient(&http.Client{Transport: serviceProbeTransport(func(req *http.Request) (*http.Response, error) {
				if mode == "removed" && req.URL.Path == "/v1/chat/completions" {
					once.Do(func() {
						runs := mgr.instances.ListAll()
						require.Len(t, runs, 1)
						require.True(t, process.ProcessHasMarker(int32(runs[0].GetProcessID())))
						closed := false
						runs[0].SetStdinPipe(smokeStopObserver{onClose: func() {
							closed = true
							assert.ErrorIs(t, runs[0].Context().Err(), context.Canceled, "monitor must be canceled before termination")
						}})
						require.NoError(t, mgr.StopInstance(t.Context(), runs[0].ID))
						assert.True(t, closed, "stop must exercise process termination")
					})
				}
				if mode == "cancel" {
					once.Do(cancel)
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: http.Header{}}, nil
			})}))
			require.NoError(t, err)
			cleanupProviderManager(t, mgr)
			// The real install keeps this gate held; smoke must bypass only this one.
			unlock, err := mgr.installs.acquireProvider(t.Context(), "generic")
			require.NoError(t, err)
			defer unlock()
			sibling, err := mgr.installs.acquireProvider(t.Context(), "sibling")
			require.NoError(t, err)
			sibling()
			err = mgr.smokeInstalledRuntime(ctx, "generic", "generic", "registry-model")
			if mode == "success" || mode == "removed" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			assert.Empty(t, mgr.instances.ListAll(), "smoke cleanup must finish even with a canceled context")
		})
	}
}

func TestSmokeFrozenFeatureSelectionRefusesPredicateDrift(t *testing.T) {
	isolateProviderRoot(t)
	mgr, svc, dir := featureLaunchFixture(t)
	svc.Features = map[string]config.Feature{"vision": {Runtime: "test-runtime", When: "vision_config", Execution: &config.ExecutionConfig{Type: "python", Command: "python3"}, WireEndpoints: []string{"chat_completions"}}}
	fake := newFakeInstaller("test-runtime")
	fake.installed.Store(true)
	mgr.installs.dispatcher.Register("test-runtime", fake)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0600))
	_, selected, _, err := mgr.featureRuntime(svc, "vendor/model")
	require.NoError(t, err)
	require.Equal(t, "test-runtime", selected)
	// Simulate the predicate disappearing while the launch waits on the selected gate.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0600))
	_, err = mgr.modelLaunchParameters(svc, LaunchRequest{Provider: "vllm", Model: "vendor/model", selectedRuntime: selected}, "chat")
	require.ErrorContains(t, err, "selected runtime changed")
}

type smokeGateContext struct {
	context.Context
	selected chan struct{}
	once     sync.Once
}

func (c *smokeGateContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.selected) })
	return c.Context.Done()
}

func TestSmokeAllowsSiblingLaunchWhileSameRuntimeWaits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix short-lived engine fixture")
	}
	isolateProviderRoot(t)
	engine := filepath.Join(t.TempDir(), "engine")
	require.NoError(t, os.WriteFile(engine, []byte("#!/bin/sh\nprintf 'READY\\n'\nexec /bin/sleep 0.4\n"), 0700))
	cfg := &config.AppsConfig{}
	for _, name := range []string{"generic", "sibling"} {
		base := 8650
		if name == "sibling" {
			base = 8660
		}
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{Name: name, Enabled: new(true), Protocol: config.ProtocolOpenAI, Mode: "on-demand", Runtime: &config.AppRuntimeConfig{BasePort: base, PortRange: []int{base, base + 2}, Execution: config.ExecutionConfig{Type: "cli", Command: engine}, HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: "2s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}}}, Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}}}))
	}
	mgr, err := NewProviderAppManager(cfg, WithMemoryBudget(func(string) (*schema.MemoryBudget, error) { return nil, nil }), WithHTTPClient(&http.Client{Transport: serviceProbeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: http.Header{}}, nil
	})}))
	require.NoError(t, err)
	cleanupProviderManager(t, mgr)
	unlock, err := mgr.installs.acquireProvider(t.Context(), "generic")
	require.NoError(t, err)
	defer unlock()
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiterContext := &smokeGateContext{Context: parent, selected: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() {
		_, err := mgr.LaunchInstance(waiterContext, LaunchRequest{Provider: "generic", Model: "registry-model"})
		waiter <- err
	}()
	select {
	case <-waiterContext.selected:
	case <-time.After(time.Second):
		t.Fatal("normal launch did not reach runtime gate")
	}
	acquired := mgr.memoryLaunchMu.TryLock()
	assert.True(t, acquired, "waiting on one runtime must not hold node memory admission")
	if acquired {
		mgr.memoryLaunchMu.Unlock()
	}
	sibling, err := mgr.LaunchInstance(t.Context(), LaunchRequest{Provider: "sibling", Model: "other-model"})
	require.NoError(t, err)
	require.NoError(t, mgr.StopInstance(t.Context(), sibling.ID))
	smoke := make(chan error, 1)
	go func() { smoke <- mgr.smokeInstalledRuntime(t.Context(), "generic", "generic", "registry-model") }()
	select {
	case err := <-smoke:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Error("smoke joined the normal launch waiting on its own gate")
		cancel()
		<-smoke
	}
	cancel()
	require.ErrorIs(t, <-waiter, context.Canceled)
}

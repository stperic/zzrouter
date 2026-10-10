package prov_apps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		protocol   config.AppProtocol
		want       RunLifecycle
	}{
		{"llamacpp", "on-demand", config.ProtocolOpenAI, LifecycleProcess},
		{"vllm", "on-demand", config.ProtocolOpenAI, LifecycleProcess},
		{"mlx", "on-demand", config.ProtocolOpenAI, LifecycleProcess},
		{"on-demand ollama", "on-demand", config.ProtocolOllama, LifecycleProcess},
		{"external ollama", "external", config.ProtocolOllama, LifecycleServed},
		{"service ollama", "service", config.ProtocolOllama, LifecycleServed},
		{"external openai", "external", config.ProtocolOpenAI, LifecycleNone},
		{"cloud", "cloud", config.ProtocolOpenAI, LifecycleNone},
		{"ollama-cloud", "cloud", config.ProtocolOllama, LifecycleNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Lifecycle(config.ServiceConfig{Mode: tc.mode, Protocol: tc.protocol}))
		})
	}
}

func TestDecideStart(t *testing.T) {
	run := instance.NewInstance("held", "fake", "model", 8200, 0, 0)
	for _, tc := range []struct {
		name                                     string
		status                                   instance.Status
		provider, runtime, plan                  string
		force, absent, smoke, empty, unconfirmed bool
		reuse, stop                              bool
		err                                      error
	}{
		{name: "running reuse", status: instance.StatusRunning, provider: "fake", reuse: true},
		{name: "stopping refuses until drain finishes", status: instance.StatusStopping, provider: "fake", err: instance.ErrStopping},
		{name: "force also refuses stopping", status: instance.StatusStopping, provider: "fake", force: true, err: instance.ErrStopping},
		{name: "unhealthy reuse", status: instance.StatusUnhealthy, provider: "fake", reuse: true},
		{name: "restarting reuse", status: instance.StatusRestarting, provider: "fake", reuse: true},
		{name: "running reuse without catalog", status: instance.StatusRunning, provider: "fake", absent: true, reuse: true},
		{name: "smoke unconfirmed drain refuses", status: instance.StatusFailed, provider: "fake", smoke: true, unconfirmed: true, err: ErrInstancesRunning},
		{name: "starting reuse", status: instance.StatusStarting, provider: "fake", reuse: true},
		{name: "provider mismatch", status: instance.StatusRunning, provider: "other", err: ErrRunHeldByOtherProvider},
		{name: "force same provider", status: instance.StatusRunning, provider: "fake", force: true, stop: true},
		{name: "force other provider stops it", status: instance.StatusRunning, provider: "other", force: true, stop: true},
		{name: "runtime mismatch", status: instance.StatusRunning, provider: "fake", runtime: "other", err: ErrInstancesRunning},
		{name: "plan mismatch", status: instance.StatusRunning, provider: "fake", plan: "other", err: ErrInstancesRunning},
		{name: "nothing running", empty: true},
		{name: "missing catalog", empty: true, absent: true, err: ErrModelNotLocal},
		{name: "force missing catalog preserves run", provider: "fake", force: true, absent: true, err: ErrModelNotLocal},
		{name: "smoke with exempt catalog input", empty: true, smoke: true},
		{name: "terminal cleanup", status: instance.StatusFailed, provider: "fake", stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := startSnapshot{local: !tc.absent}
			if !tc.empty {
				snapshot.run = &runSnapshot{run, tc.provider, "chat", tc.runtime, tc.plan, tc.status, tc.unconfirmed}
			}
			result := decideStart(EnsureRequest{Provider: "fake", Model: "model", Endpoint: EndpointChat, Force: tc.force, installSmoke: tc.smoke}, snapshot)
			require.ErrorIs(t, result.err, tc.err)
			if errors.Is(result.err, instance.ErrStopping) {
				var admission *instance.AdmissionError
				require.ErrorAs(t, result.err, &admission)
				assert.Equal(t, instance.AdmissionRetryAfter, admission.RetryAfter)
			}
			assert.Equal(t, tc.reuse, result.reuse != nil)
			assert.Equal(t, tc.stop, result.stop != nil)
			if tc.reuse {
				assert.Same(t, run, result.reuse)
			}
			if tc.stop {
				assert.Same(t, run, result.stop)
			}
		})
	}
}

func TestLifecycleStartPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		lifecycle                     RunLifecycle
		local, reachable, ready, load bool
		err                           error
	}{
		{name: "none in catalog", lifecycle: LifecycleNone, local: true, reachable: true, ready: true},
		{name: "none absent", lifecycle: LifecycleNone, reachable: true, err: ErrModelNotLocal},
		{name: "none unreachable", lifecycle: LifecycleNone, err: ErrProviderUnavailable},
		{name: "served unreachable", lifecycle: LifecycleServed, local: true, err: ErrProviderUnavailable},
		{name: "served unreachable before catalog", lifecycle: LifecycleServed, err: ErrProviderUnavailable},
		{name: "served loads without process catalog", lifecycle: LifecycleServed, reachable: true, load: true},
		{name: "process absent", lifecycle: LifecycleProcess, err: ErrModelNotLocal},
		{name: "process starts", lifecycle: LifecycleProcess, local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A process held by another provider cannot change a non-process decision.
			snapshot := startSnapshot{lifecycle: tc.lifecycle, local: tc.local, reachable: tc.reachable}
			if tc.lifecycle != LifecycleProcess {
				snapshot.run = &runSnapshot{provider: "other", endpoint: "chat", status: instance.StatusStopping}
			}
			decision := decideStart(EnsureRequest{Model: "m", Provider: "requested", Endpoint: EndpointChat, Force: true}, snapshot)
			assert.ErrorIs(t, decision.err, tc.err)
			assert.Equal(t, tc.ready, decision.ready)
			assert.Equal(t, tc.load, decision.load)
		})
	}
}

func TestChooseEvictions(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	candidates := []evictionCandidate{{"new", 4, now}, {"b", 3, now.Add(-time.Hour)}, {"a", 2, now.Add(-time.Hour)}}
	for _, tc := range []struct {
		name      string
		shortfall int64
		want      []string
	}{
		{"no shortage", 0, nil}, {"negative shortage", -1, nil},
		{"oldest and tiebreak", 4, []string{"a", "b"}},
		{"exact cover", 5, []string{"a", "b"}},
		{"one victim", 2, []string{"a"}},
		{"all victims", 9, []string{"a", "b", "new"}},
		{"not enough", 10, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chooseEvictions(tc.shortfall, candidates))
			assert.Equal(t, "new", candidates[0].id)
		})
	}
	assert.Empty(t, chooseEvictions(1, nil))
}

func TestIdleExpired(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		keepAlive, idle time.Duration
		zero, want      bool
	}{
		{name: "never", keepAlive: -1, idle: time.Hour},
		{name: "immediate", want: true},
		{name: "immediate without activity", zero: true, want: true},
		{name: "before", keepAlive: time.Minute, idle: time.Second},
		{name: "boundary", keepAlive: time.Minute, idle: time.Minute},
		{name: "after", keepAlive: time.Minute, idle: time.Minute + time.Nanosecond, want: true},
		{name: "no activity", keepAlive: time.Minute, zero: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			activity := now.Add(-tc.idle)
			if tc.zero {
				activity = time.Time{}
			}
			assert.Equal(t, tc.want, idleExpired(tc.keepAlive, activity, now))
		})
	}
}

func TestRegisteredRunKeepAlive(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"-1", -1}, {"0", 0}} {
		t.Run(tc.value, func(t *testing.T) {
			cfg := aliasTestConfig(t)
			require.NoError(t, cfg.UpdateApp("fake", func(svc *config.ServiceConfig) error { svc.Runtime.KeepAlive = tc.value; return nil }))
			mgr, err := NewProviderAppManager(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, mgr.Stop(context.Background())) })
			svc, _ := cfg.LookupApp("fake")
			run, err := mgr.registerRun(t.Context(), EnsureRequest{Provider: "fake", Model: "m"}, svc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, run.GetKeepAlive())
			keepAlive, activity := run.IdleSnapshot()
			assert.Equal(t, tc.want == 0, idleExpired(keepAlive, activity, utils.Now()))
			// Registry visibility lasts until the reaper actually ends the run.
			assert.Contains(t, mgr.Instances().List(), run)
		})
	}
}

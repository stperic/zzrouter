package prov_apps

import (
	"context"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWatcherTestMgr wires a ProviderAppManager with a live jobs registry
// so watchInstanceReadiness has a real handle to emit into. Includes a
// cleanup that stops both.
func newWatcherTestMgr(t *testing.T) (*ProviderAppManager, *jobs.Registry) {
	t.Helper()
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
	require.NoError(t, err)
	m, err := NewProviderAppManager(testAppsConfig(), WithJobsRegistry(reg))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = m.Stop(context.Background())
		reg.Stop()
	})
	return m, reg
}

// subscribeTerminal drains events until terminal; fails the test on
// timeout so watcher leaks are visible.
func subscribeTerminal(t *testing.T, reg *jobs.Registry, jobID string, timeout time.Duration) jobs.Event {
	t.Helper()
	sub, err := reg.Subscribe(jobID, jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()
	return waitTerminal(t, sub.Events(), timeout)
}

// TestWatcher_ReachesRunning_EmitsDone — the happy path: status flips
// to Running, watcher fires Done and exits.
func TestWatcher_ReachesRunning_EmitsDone(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-1", "vllm", "llama3", 8100, time.Minute, 0)
	require.NoError(t, m.instances.Register(inst))
	h, err := reg.Start(context.Background(), jobs.KindRun, "", jobs.Meta{"run_id": inst.ID})
	require.NoError(t, err)
	inst.StreamJobID = h.ID()

	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		m.watchInstanceReadiness(inst, h)
	}()

	// Simulate the health monitor reaching a good check after a short
	// pause. The watcher's 500ms poll sees the transition.
	time.AfterFunc(100*time.Millisecond, func() {
		inst.SetStatus(instance.StatusRunning)
	})

	ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
	assert.Equal(t, jobs.PhaseDone, ev.Phase)
}

// TestWatcher_Failed_EmitsFail — the process crashed / health sustained
// failure: watcher emits Fail carrying the instance error. Stresses the
// race between MarkFailed (writes ErrorMessage under mu) and the
// watcher reading the message for the Fail payload. Pins H1 from the
// post-code review: direct ErrorMessage access raced with the locked
// write; GetErrorMessage() takes the same mutex.
func TestWatcher_Failed_EmitsFail(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-2", "vllm", "m", 8101, time.Minute, 0)
	require.NoError(t, m.instances.Register(inst))
	h, err := reg.Start(context.Background(), jobs.KindRun, "", nil)
	require.NoError(t, err)
	inst.StreamJobID = h.ID()

	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		m.watchInstanceReadiness(inst, h)
	}()

	// Hammer MarkFailed from a background goroutine so -race has every
	// chance to observe the unsynchronized read if the watcher ever
	// regresses to direct field access.
	stopHammer := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopHammer:
				return
			default:
				inst.MarkFailed("process exited with code 1")
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	defer close(stopHammer)

	ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
	assert.Equal(t, jobs.PhaseFailed, ev.Phase)
	assert.Contains(t, ev.Err, "process exited")
}

// TestWatcher_StoppedBeforeRunning_EmitsFail — reached Stopped without
// ever being Running. Agents subscribing want a terminal event, not a
// hang.
func TestWatcher_StoppedBeforeRunning_EmitsFail(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-3", "vllm", "m", 8102, time.Minute, 0)
	require.NoError(t, m.instances.Register(inst))
	h, err := reg.Start(context.Background(), jobs.KindRun, "", nil)
	require.NoError(t, err)

	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		m.watchInstanceReadiness(inst, h)
	}()

	time.AfterFunc(100*time.Millisecond, func() {
		inst.SetStatus(instance.StatusStopped)
	})

	ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
	assert.Equal(t, jobs.PhaseFailed, ev.Phase)
	assert.Contains(t, ev.Err, "stopped before")
}

// TestWatcher_CancelViaHandleCtx_StopsInstance — DELETE /jobs/:id fires
// the handle's ctx. Watcher must call StopInstance so cancel is not
// cosmetic. The fake instance has no real process, so StopInstance's
// SIGTERM branch is a no-op but the status flips Stopping→Stopped via
// inst.Cancel(), and we verify the watcher emits Fail("cancelled").
func TestWatcher_CancelViaHandleCtx_StopsInstance(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-4", "vllm", "m", 8103, time.Minute, 0)
	require.NoError(t, m.instances.Register(inst))
	h, err := reg.Start(context.Background(), jobs.KindRun, "", nil)
	require.NoError(t, err)

	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		m.watchInstanceReadiness(inst, h)
	}()

	// Let watcher start up, then cancel via registry.
	time.Sleep(80 * time.Millisecond)
	require.NoError(t, reg.Cancel(h.ID()))

	ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
	assert.Equal(t, jobs.PhaseFailed, ev.Phase)
	assert.Contains(t, ev.Err, "cancelled")

	// detachedStop runs in its own goroutine; give it a moment to
	// reach StopInstance before asserting status transitioned.
	require.Eventually(t, func() bool {
		s := inst.GetStatus()
		return s == instance.StatusStopping || s == instance.StatusStopped || s == instance.StatusFailed
	}, 2*time.Second, 20*time.Millisecond,
		"watcher must have called StopInstance on cancel (via detachedStop)")
}

// TestWatcher_HeartbeatKeepsJobAlive — while pending, the 30s heartbeat
// emits Progress events. We can't wait 30s in a unit test, but we can
// verify the initial "launching" Progress event fires immediately so
// subscribers see something before the first status transition.
func TestWatcher_InitialProgressFires(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-5", "vllm", "m", 8104, time.Minute, 0)
	require.NoError(t, m.instances.Register(inst))
	h, err := reg.Start(context.Background(), jobs.KindRun, "", nil)
	require.NoError(t, err)

	sub, err := reg.Subscribe(h.ID(), jobs.SubscribeOptions{})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		m.watchInstanceReadiness(inst, h)
	}()

	// First event should be pending (handle open) followed by a
	// running-phase Progress with Step="launching" (our opening
	// heartbeat). Terminal Done comes after we flip StatusRunning.
	var sawLaunching bool
	deadline := time.After(1 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break loop
			}
			if ev.Step == "launching" {
				sawLaunching = true
				break loop
			}
		case <-deadline:
			break loop
		}
	}

	assert.True(t, sawLaunching, "watcher must emit a 'launching' Progress event on entry")

	// Clean up via Running transition so the watcher exits.
	inst.SetStatus(instance.StatusRunning)
}

// TestKindRun_HasExplicitInactivity — regression guard for the zzgo H2
// finding: KindRun must NOT inherit the 30-min fallback, or slow model
// loads get prematurely reaped by the janitor.
func TestKindRun_HasExplicitInactivity(t *testing.T) {
	t.Parallel()
	got := jobs.KindRun.DefaultInactivityTimeout()
	assert.GreaterOrEqual(t, got, 1*time.Hour, "KindRun inactivity must be generous enough for cold-disk 70B loads")
}

// TestKindRun_IDPrefix — run_ prefix for grep-ability alongside dl_ inst_ upd_.
func TestKindRun_IDPrefix(t *testing.T) {
	t.Parallel()
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "t", Clock: clock.System()})
	require.NoError(t, err)
	defer reg.Stop()
	h, err := reg.Start(context.Background(), jobs.KindRun, "", nil)
	require.NoError(t, err)
	assert.Regexp(t, `^run_[a-f0-9]+$`, h.ID())
}

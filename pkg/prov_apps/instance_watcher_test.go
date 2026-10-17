package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestClassifiedFailureSurvivesRunAndJob(t *testing.T) {
	for _, first := range []string{"readiness", "exit"} {
		t.Run(first, func(t *testing.T) {
			m, reg := newWatcherTestMgr(t)
			declaration, err := templates.AppsFS.ReadFile("files/providers/on-demand/vllm/config.yaml")
			require.NoError(t, err)
			var svc config.ServiceConfig
			require.NoError(t, yaml.Unmarshal(declaration, &svc))
			require.NoError(t, svc.Runtime.FailurePatterns.Compile())
			log, err := os.ReadFile("../httperr/testdata/vllm_mamba_refusal.txt")
			require.NoError(t, err)
			inst := instance.NewInstance("failed", "vllm", "model", 0, 0, 0)
			inst.FailureClassifier = &svc.Runtime.FailurePatterns
			inst.LogFilePath = filepath.Join(t.TempDir(), "engine.log")
			require.NoError(t, os.WriteFile(inst.LogFilePath, log, 0600))
			require.NoError(t, m.instances.Register(inst))
			if first == "readiness" {
				inst.MarkFailed("readiness failed")
			}
			inst.MarkFailedWithExit("process exited", new(1), "")
			inst.MarkFailed("later generic readiness failure")
			info := inst.ToInfo("node")
			var failure *httperr.EngineFailure
			require.ErrorAs(t, inst.FailureCause(), &failure)
			assert.Equal(t, httperr.FailureMemory, failure.Class)
			assert.Equal(t, string(httperr.CodeOutOfRange), failure.HTTPError().Code)
			assert.Equal(t, instance.StatusFailed, info.Status)
			for _, evidence := range []string{"(256)", "(152)", "at most 152"} {
				assert.Contains(t, info.ErrorMessage, evidence)
			}
			require.NotNil(t, info.Failure)
			assert.Equal(t, 1, *info.Failure.ExitCode)
			h, err := reg.Start(t.Context(), jobs.KindRun, "", jobs.Meta{"run_id": inst.ID})
			require.NoError(t, err)
			inst.TrackGoroutine()
			go func() {
				defer inst.GoroutineDone()
				m.watchInstanceReadiness(inst, h)
			}()
			ev := subscribeTerminal(t, reg, h.ID(), 3*time.Second)
			assert.Equal(t, jobs.PhaseFailed, ev.Phase)
			for _, evidence := range []string{"(256)", "(152)", "at most 152"} {
				assert.Contains(t, ev.Err, evidence)
			}
			assert.Equal(t, info.Failure, ev.Meta["failure"])
		})
	}
}

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
	assert.NotContains(t, ev.Meta, "evicted_runs", "a launch that evicted nothing says nothing")
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

// A job that ends by any path but a cancel, a reap for one, leaves its run alone.
func TestWatcher_EndedJobLeavesTheRun(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-reaped", "vllm", "m", 8106, time.Minute, 0)
	require.NoError(t, m.instances.Register(inst))
	h, err := reg.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)

	watched := make(chan struct{})
	inst.TrackGoroutine()
	go func() {
		defer inst.GoroutineDone()
		defer close(watched)
		m.watchInstanceReadiness(inst, h)
	}()
	h.Fail(errors.New("job reaped: inactivity timeout exceeded"))
	select {
	case <-watched:
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher kept watching an ended job")
	}
	require.Never(t, func() bool { return inst.GetStatus() != instance.StatusStarting }, 500*time.Millisecond, 20*time.Millisecond,
		"an ended job does not stop its run")
}

// TestWatcher_HeartbeatKeepsJobAlive — while pending, the 30s heartbeat
// emits Progress events. We can't wait 30s in a unit test, but we can
// verify the initial "launching" Progress event fires immediately so
// subscribers see something before the first status transition.
func TestWatcher_InitialProgressFires(t *testing.T) {
	m, reg := newWatcherTestMgr(t)

	inst := instance.NewInstance("inst-5", "vllm", "m", 8104, time.Minute, 0)
	require.True(t, inst.Admit(), "an inline launch is admitted before its job opens")
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

// A queued run's job says so and names what it waits on, then reports
// "launching" once the run is admitted.
func TestWatcher_QueuedThenAdmitted(t *testing.T) {
	m, reg := newWatcherTestMgr(t)
	inst := instance.NewInstance("inst-6", "vllm", "m", 8105, time.Minute, 0)
	inst.SetWaitingOn("other-run")
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

	next := func(step string) jobs.Event {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case ev := <-sub.Events():
				if ev.Step == step {
					return ev
				}
			case <-deadline:
				t.Fatalf("no %q event", step)
			}
		}
	}
	queuedEv := next("queued")
	assert.Equal(t, "other-run", queuedEv.Meta["waiting_on"])
	require.True(t, inst.Admit())
	assert.Equal(t, "launching", next("launching").Meta["phase"])
	inst.MarkRunning(false, nil)
}

func TestRunJobPublicationPrecedesTerminal(t *testing.T) {
	for _, status := range []instance.Status{instance.StatusRunning, instance.StatusFailed} {
		for _, publicationFails := range []bool{false, true} {
			t.Run(string(status)+"/publicationFails="+fmt.Sprint(publicationFails), func(t *testing.T) {
				m, reg := newWatcherTestMgr(t)
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				m.publishCatalog = func() error {
					close(entered)
					<-release
					if publicationFails {
						return errors.New("publication refused")
					}
					return nil
				}
				inst := instance.NewInstance("management", "vllm", "model", 0, 0, 0)
				inst.SetStatus(status)
				h, err := reg.StartDetached(jobs.KindRun, "", nil)
				require.NoError(t, err)
				go func() {
					defer close(finished)
					m.watchInstanceReadiness(inst, h)
				}()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("terminal job skipped catalog publication")
				}
				ev, err := reg.Get(h.ID())
				require.NoError(t, err)
				require.False(t, ev.Phase.IsTerminal(), "publication must finish before the terminal event")
				close(release)
				<-finished
				ev, err = reg.Get(h.ID())
				require.NoError(t, err)
				want := jobs.PhaseDone
				if status == instance.StatusFailed || publicationFails {
					want = jobs.PhaseFailed
				}
				require.Equal(t, want, ev.Phase)
				if publicationFails {
					require.Contains(t, ev.Err, "publication refused")
				}
			})
		}
	}
}

func TestRunJobDuringShutdownDoesNotPublish(t *testing.T) {
	m, reg := newWatcherTestMgr(t)
	m.publishCatalog = func() error { t.Error("a shutting-down manager must not wait on publication"); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.shutdownCtx = ctx
	inst := instance.NewInstance("shutdown", "vllm", "model", 0, 0, 0)
	h, err := reg.StartDetached(jobs.KindRun, "", nil)
	require.NoError(t, err)
	m.watchInstanceReadiness(inst, h)
	ev, err := reg.Get(h.ID())
	require.NoError(t, err)
	require.Equal(t, jobs.PhaseFailed, ev.Phase)
}

//go:build unix

package prov_apps

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/host"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeStopEngineHelper(t *testing.T) {
	if !slices.Contains(os.Args, "node-stop-engine") {
		return
	}
	dir := os.Args[len(os.Args)-2]
	delay, err := time.ParseDuration(os.Args[len(os.Args)-1])
	if err != nil {
		os.Exit(2)
	}
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	if slices.Contains(os.Args, "spawn-child") {
		child := host.Command(os.Args[0], "-test.run=^TestNodeStopEngineHelper$", "--", "node-stop-engine", filepath.Join(dir, "child"), "24h")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		go func() { _ = child.Wait() }()
		if err := os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(2)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); err != nil {
		os.Exit(2)
	}
	f, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_ = f.Close()
	var finished <-chan time.Time
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	count := byte(0)
	for {
		select {
		case <-signals:
			count++
			if err := os.WriteFile(filepath.Join(dir, "signals"), []byte{count}, 0600); err != nil {
				os.Exit(2)
			}
			if count == 1 && delay > 0 {
				finished = time.After(delay)
			}
		case <-poll.C:
			if count > 0 && delay == 0 {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					finished = time.After(0)
				}
			}
		case <-finished:
			if err := os.WriteFile(filepath.Join(dir, "exited"), nil, 0600); err != nil {
				os.Exit(2)
			}
			syscall.Exit(0)
		}
	}
}

func nodeStopRun(t *testing.T, m *ProviderAppManager, n int, exitDelay time.Duration) (*instance.Instance, string) {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	port, err := m.ports.AllocateForProvider("generic", 0)
	require.NoError(t, err)
	run := instance.NewInstance("stop-"+strconv.Itoa(n), "generic", "model-"+strconv.Itoa(n), port, 0, 8)
	result, err := m.launcher.Launch(t.Context(), run, binary, []string{"-test.run=^TestNodeStopEngineHelper$", "--", "node-stop-engine", dir, exitDelay.String()}, nil, nil)
	require.NoError(t, err)
	run.SetProcessInfo(result.PID, result.ProcessGroupID, result.StartToken, result.Command)
	run.SetStdinPipe(result.StdinPipe)
	run.TrackGoroutine()
	done := make(chan struct{})
	go func() { defer close(done); defer run.GoroutineDone(); _, _ = result.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-result.ProcessGroupID, syscall.SIGKILL); run.CloseStdinPipe(); <-done })
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil }, contractWait, time.Millisecond)
	run.SetStatus(instance.StatusRunning)
	require.NoError(t, m.instances.Register(run))
	return run, dir
}

func TestNodeStopSignalsEveryRunOnceInParallel(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	var dirs []string
	var runs []*instance.Instance
	for n := range 3 {
		run, dir := nodeStopRun(t, r.m, n, 0)
		if n == 0 {
			run.SetKeepAlive(-1)
			r.m.pins.update(EnsureRequest{Provider: run.Provider, Model: run.Model, Parameters: map[string]string{"keep-alive": "-1"}}, true, r.m.AppsConfig())
		}
		dirs = append(dirs, dir)
		runs = append(runs, run)
	}
	pins := readPins(t)
	require.Len(t, pins, 1)
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.Stop(t.Context()) }()
	require.Eventually(t, func() bool {
		for _, dir := range dirs {
			if _, err := os.Stat(filepath.Join(dir, "signals")); err != nil {
				return false
			}
		}
		return true
	}, contractWait, time.Millisecond)
	for i, run := range runs {
		require.NoError(t, syscall.Kill(-run.GetProcessGroupID(), 0), "every group must still be alive when all signals are observed")
		_, err := os.Stat(filepath.Join(dirs[i], "exited"))
		require.True(t, os.IsNotExist(err), "every engine must still be in grace when all signals are observed")
	}
	for _, dir := range dirs {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "release"), nil, 0600))
	}
	require.NoError(t, <-stopped)
	for _, dir := range dirs {
		count, err := os.ReadFile(filepath.Join(dir, "signals"))
		require.NoError(t, err)
		require.Equal(t, []byte{1}, count)
	}
	require.Empty(t, r.m.instances.List())
	require.Equal(t, pins, readPins(t), "node stop must preserve the pinned run record")
}

func TestStopBeforePIDPublicationHonoursGrace(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	r := newContractRig(t, true)
	engine := testproc.Script(`trap 'echo "$$" >> "$0.terms"; /bin/sleep 7; exit 0' TERM INT
: > "$0.spawned"
while kill -0 $test_owner && [ ! -e "$0.ready" ]; do /bin/sleep 0.01; done
kill -0 $test_owner || exit 0
printf 'READY\n'
while kill -0 $test_owner; do /bin/sleep 1 & wait $!; done
`)
	require.NoError(t, os.WriteFile(r.engine, []byte(engine), 0700))
	b := newBarrier()
	r.m.seams.processStarted = func(result *process.LaunchResult) {
		t.Cleanup(func() { _ = syscall.Kill(-result.ProcessGroupID, syscall.SIGKILL) })
		if !assert.Eventually(t, func() bool { _, err := os.Stat(r.engine + ".spawned"); return err == nil }, contractWait, time.Millisecond) {
			return
		}
		b.hold()
	}
	o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "shared"})})[0]
	require.NoError(t, o.err)
	run := o.result.Run
	b.reached(t)
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.StopInstance(t.Context(), run.ID) }()
	waitParkedOn(t, ").endRun(", 1, contractWait)
	require.Zero(t, run.GetProcessID(), "the stop must claim the run before PID publication")
	b.open()
	require.NoError(t, <-stopped, "the lifecycle's TERM grace belongs inside the whole stop budget")
	require.False(t, r.m.instances.Has(run.ID))
	require.False(t, run.StopUnconfirmed.Load())
	require.False(t, r.m.ports.IsAllocatedForProvider(run.Provider, run.Port))
	require.Equal(t, 1, r.terms(t))
	replacement := instance.NewInstance("replacement", run.Provider, run.Model, run.Port, 0, 8)
	require.NoError(t, r.m.instances.Register(replacement))
	require.NoError(t, r.m.instances.Remove(replacement.ID))
}

func TestNodeStopJoinsConcurrentAPIStopAndReportsTimeout(t *testing.T) {
	r := newContractRig(t, false)
	run, dir := nodeStopRun(t, r.m, 0, 400*time.Millisecond)
	run.TrackGoroutine()
	released := false
	defer func() {
		if !released {
			run.GoroutineDone()
		}
	}()
	apiStopped := make(chan error, 1)
	go func() { apiStopped <- r.m.StopInstance(t.Context(), run.ID) }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "signals")); return err == nil }, contractWait, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.Stop(ctx) }()
	select {
	case err := <-stopped:
		t.Fatalf("manager ignored API stop: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	require.ErrorIs(t, <-stopped, context.DeadlineExceeded)
	run.GoroutineDone()
	released = true
	require.NoError(t, <-apiStopped)
	count, err := os.ReadFile(filepath.Join(dir, "signals"))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, count)
}

func TestNodeStopBudgetCoversConfiguredGrace(t *testing.T) {
	r := newContractRig(t, false)
	for hint, want := range map[string]time.Duration{"gpu": constants.EngineMaxGracefulTimeout, "2m": 2 * time.Minute, "8s": 8 * time.Second, "": process.GracefulTimeout} {
		svc, _ := r.m.appsConfig.LookupApp("generic")
		svc.Defaults.Metadata = &config.AppMetadata{ShutdownTimeout: hint}
		require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(dest *config.ServiceConfig) error { *dest = svc; return nil }))
		grace := r.m.resolveShutdownTimeout("generic")
		require.Equal(t, want, grace)
	}
}

func TestSupervisedDaemonCancellationTermsThenKillsDescendant(t *testing.T) {
	r := newContractRig(t, false)
	dir := t.TempDir()
	childDir := filepath.Join(dir, "child")
	require.NoError(t, os.MkdirAll(childDir, 0700))
	binary, err := os.Executable()
	require.NoError(t, err)
	env, err := process.ChildEnvironment(nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state := r.m.serviceState("ollama")
	state.cancel, state.done = cancel, make(chan struct{})
	done := state.done
	go r.m.superviseService(ctx, "ollama", binary, []string{"-test.run=^TestNodeStopEngineHelper$", "--", "node-stop-engine", "spawn-child", dir, "400ms"}, env, state)
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil }, contractWait, time.Millisecond)
	state.mu.Lock()
	leader := state.status.PID
	state.mu.Unlock()
	childPID, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	require.NoError(t, err)
	child, err := strconv.ParseInt(string(childPID), 10, 32)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(-leader, syscall.SIGKILL) })
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(childDir, "ready")); return err == nil }, contractWait, time.Millisecond)
	cancel()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "signals")); return err == nil }, time.Second, time.Millisecond, "the real owned-command callback must send TERM")
	require.Eventually(t, func() bool { state, err := process.StartToken(leader); return err != nil || state == 0 }, 3*time.Second, 10*time.Millisecond)
	select {
	case <-done:
		t.Fatal("service stop returned with a surviving descendant")
	default:
	}
	select {
	case <-done:
	case <-time.After(process.GracefulTimeout + process.ForceTimeout):
		t.Fatal("descendant survived escalation")
	}
	require.False(t, process.ProcessExists(int32(child)), "the real descendant must be gone")
	require.ErrorIs(t, syscall.Kill(-leader, 0), syscall.ESRCH)
	childSignals, err := os.ReadFile(filepath.Join(childDir, "signals"))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, childSignals)
	count, err := os.ReadFile(filepath.Join(dir, "signals"))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, count)
	starts, err := os.ReadFile(filepath.Join(dir, "starts"))
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(filepath.Join(dir, "starts"))
	require.NoError(t, err)
	require.Equal(t, string(starts), string(after), "cancellation must not restart the daemon")
	state.mu.Lock()
	defer state.mu.Unlock()
	require.False(t, state.status.Running)
	require.False(t, state.status.Desired)
}

func TestAPIStopHonoursConfiguredGraceAndNodeStopBoundsIt(t *testing.T) {
	for _, nodeStop := range []bool{false, true} {
		name := "API stop"
		if nodeStop {
			name = "node stop"
		}
		t.Run(name, func(t *testing.T) {
			r := newContractRig(t, false)
			require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
				svc.Defaults.Metadata = &config.AppMetadata{ShutdownTimeout: "60s"}
				return nil
			}))
			run, dir := nodeStopRun(t, r.m, 0, 24*time.Hour)
			groupGone, termSeen := make(chan error, 1), make(chan error, 1)
			go func() {
				timeout := time.NewTimer(contractWait)
				defer timeout.Stop()
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				signaled := false
				for {
					if !signaled {
						if count, err := os.ReadFile(filepath.Join(dir, "signals")); err == nil && len(count) == 1 {
							signaled = true
							termSeen <- nil
						}
					}
					if syscall.Kill(-run.GetProcessGroupID(), 0) == syscall.ESRCH {
						groupGone <- nil
						return
					}
					select {
					case <-ticker.C:
					case <-timeout.C:
						if !signaled {
							termSeen <- context.DeadlineExceeded
						}
						groupGone <- context.DeadlineExceeded
						return
					}
				}
			}()
			synctest.Test(t, func(t *testing.T) {
				// Budget waits must belong to the virtual clock bubble, unlike the real engine.
				r.m.runExpired = make(chan struct{})
				limit := time.Minute
				stopped := make(chan error, 1)
				if nodeStop {
					limit = constants.ShutdownRunTimeout
					ctx, cancel := context.WithTimeout(t.Context(), limit)
					defer cancel()
					go func() { stopped <- r.m.Stop(ctx) }()
				} else {
					go func() { stopped <- r.m.StopInstance(t.Context(), run.ID) }()
				}
				require.NoError(t, <-termSeen, "let the real engine observe TERM before advancing virtual grace")
				time.Sleep(limit - time.Second)
				select {
				case err := <-stopped:
					t.Fatalf("stop cut short its configured grace: %v", err)
				default:
				}
				require.NoError(t, syscall.Kill(-run.GetProcessGroupID(), 0), "engine must still be alive during grace")
				time.Sleep(time.Second)
				require.NoError(t, <-groupGone, "wait for the real OS group exit before advancing virtual force-wait time")
				require.NoError(t, run.WaitForGoroutines(t.Context()), "real process reaper must finish after escalation")
				if nodeStop {
					err := <-stopped
					if err != nil {
						require.ErrorIs(t, err, context.DeadlineExceeded)
					}
				} else {
					require.NoError(t, <-stopped)
				}
			})
			require.ErrorIs(t, syscall.Kill(-run.GetProcessGroupID(), 0), syscall.ESRCH)
			require.Eventually(t, func() bool { return !r.m.instances.Has(run.ID) }, contractWait, time.Millisecond, "the original stop must release its record without another StopInstance")
			require.False(t, r.m.ports.IsAllocatedForProvider(run.Provider, run.Port))
			count, err := os.ReadFile(filepath.Join(dir, "signals"))
			require.NoError(t, err)
			require.Equal(t, []byte{1}, count)
		})
	}
}

func TestStopCallerCancellationPreservesGrace(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "API disconnect"
		if idle {
			name = "idle stop during node shutdown"
		}
		t.Run(name, func(t *testing.T) {
			r := newContractRig(t, false)
			run, dir := nodeStopRun(t, r.m, 0, 400*time.Millisecond)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stopped := make(chan error, 1)
			if idle {
				ctx = r.m.startsCtx
				go func() { stopped <- r.m.stopInstance(ctx, run.ID, obsruns.StopReasonIdle) }()
			} else {
				go func() { stopped <- r.m.StopInstance(ctx, run.ID) }()
			}
			require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "signals")); return err == nil }, contractWait, time.Millisecond)
			if idle {
				require.NoError(t, r.m.Stop(t.Context()))
			} else {
				cancel()
			}
			require.NoError(t, <-stopped)
			require.False(t, r.m.instances.Has(run.ID), "the original stop must remove its record")
			require.False(t, run.StopUnconfirmed.Load())
			require.False(t, r.m.ports.IsAllocatedForProvider(run.Provider, run.Port))
			replacement := instance.NewInstance("replacement", run.Provider, run.Model, run.Port, 0, 8)
			require.NoError(t, r.m.instances.Register(replacement), "the key and port must accept a new run")
			require.NoError(t, r.m.instances.Remove(replacement.ID))
			_, err := os.Stat(filepath.Join(dir, "exited"))
			require.NoError(t, err, "the engine must complete its own graceful exit")
			count, err := os.ReadFile(filepath.Join(dir, "signals"))
			require.NoError(t, err)
			require.Equal(t, []byte{1}, count)
		})
	}
}

func TestNodeStopSignalsBeforeJoiningBackgroundStops(t *testing.T) {
	for _, background := range []string{"reaper", "replacement"} {
		for _, shortBudget := range []bool{false, true} {
			name := background + "/grace"
			if shortBudget {
				name = background + "/run deadline"
			}
			t.Run(name, func(t *testing.T) {
				r := newContractRig(t, false)
				grace, exitDelay := "10s", time.Duration(0)
				if shortBudget {
					grace, exitDelay = "60s", 24*time.Hour
				}
				require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
					svc.Defaults.Metadata = &config.AppMetadata{ShutdownTimeout: grace}
					return nil
				}))
				idle, idleDir := nodeStopRun(t, r.m, 0, exitDelay)
				other, otherDir := nodeStopRun(t, r.m, 1, exitDelay)
				if background == "reaper" {
					idle.SetKeepAlive(time.Millisecond)
					idle.UpdateLastUsed()
				}
				r.m.Start(t.Context())
				require.NotNil(t, r.m.reaperDone)
				var replacing <-chan outcome
				if background == "replacement" {
					replacing = r.ensure(t.Context(), EnsureRequest{Provider: idle.Provider, Model: idle.Model, Force: true})
				}
				require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(idleDir, "signals")); return err == nil }, contractWait, time.Millisecond)
				ctx := t.Context()
				if shortBudget {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
					defer cancel()
				}
				stopped := make(chan error, 1)
				go func() { stopped <- r.m.Stop(ctx) }()
				require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(otherDir, "signals")); return err == nil }, contractWait, time.Millisecond)
				if !shortBudget {
					require.NoError(t, syscall.Kill(-idle.GetProcessGroupID(), 0), "the first stop must still be in grace when the second run is signalled")
					if background == "reaper" {
						select {
						case <-r.m.reaperDone:
							t.Fatal("node stop waited for the reaper before signalling the other run")
						default:
						}
					}
					for _, dir := range []string{idleDir, otherDir} {
						require.NoError(t, os.WriteFile(filepath.Join(dir, "release"), nil, 0600))
					}
				}
				err := <-stopped
				if shortBudget {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.NoError(t, err)
				}
				if replacing != nil {
					require.ErrorIs(t, (<-replacing).err, ErrShutdown)
				}
				for i, run := range []*instance.Instance{idle, other} {
					require.Eventually(t, func() bool { return !r.m.instances.Has(run.ID) }, contractWait, time.Millisecond)
					require.ErrorIs(t, syscall.Kill(-run.GetProcessGroupID(), 0), syscall.ESRCH)
					dir := []string{idleDir, otherDir}[i]
					count, err := os.ReadFile(filepath.Join(dir, "signals"))
					require.NoError(t, err)
					require.Equal(t, []byte{1}, count)
					_, err = os.Stat(filepath.Join(dir, "exited"))
					if shortBudget {
						require.True(t, os.IsNotExist(err), "every run must be escalated at the run deadline")
					} else {
						require.NoError(t, err, "every engine must finish its own grace")
					}
				}
			})
		}
	}
}

func TestNodeStopSweepsRunRegisteredDuringFirstPass(t *testing.T) {
	r := newContractRig(t, false)
	r.m.Start(t.Context())
	first, firstDir := nodeStopRun(t, r.m, 0, 0)
	r.m.onDemandStarts.Add(1)
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.Stop(t.Context()) }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(firstDir, "signals")); return err == nil }, contractWait, time.Millisecond)
	late, lateDir := nodeStopRun(t, r.m, 1, 400*time.Millisecond)
	r.m.onDemandStarts.Done()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(lateDir, "signals")); return err == nil }, contractWait, time.Millisecond)
	require.NoError(t, syscall.Kill(-first.GetProcessGroupID(), 0), "late registration must be signalled while the first pass is in grace")
	_, err := os.Stat(filepath.Join(firstDir, "exited"))
	require.True(t, os.IsNotExist(err), "the sweep must not wait for first-pass results")
	require.NoError(t, os.WriteFile(filepath.Join(firstDir, "release"), nil, 0600))
	require.NoError(t, <-stopped)
	require.False(t, r.m.instances.Has(late.ID), "the second pass must stop a run registered after the first snapshot")
	count, err := os.ReadFile(filepath.Join(lateDir, "signals"))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, count)
}

func TestNodeStopDoesNotRetryFailedFirstPass(t *testing.T) {
	r := newContractRig(t, false)
	run, dir := nodeStopRun(t, r.m, 0, 100*time.Millisecond)
	run.TrackGoroutine()
	require.NoError(t, r.m.runEngine(run).down(t.Context(), run))
	budget := r.m.stopBudget
	r.m.stopBudget = func(string) time.Duration { return -1 }
	defer func() {
		run.GoroutineDone()
		r.m.stopBudget = budget
		require.NoError(t, r.m.StopInstance(context.Background(), run.ID))
	}()
	r.m.reaperDone = make(chan struct{}) // Hold the sweep until the first stop fails.
	defer close(r.m.reaperDone)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.Stop(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "signals")); return err == nil }, contractWait, time.Millisecond)
	require.Eventually(t, func() bool { return run.GetStatus() == instance.StatusStopping }, contractWait, time.Millisecond)
	firstErr := run.WaitForEnd(t.Context())
	require.ErrorIs(t, firstErr, context.DeadlineExceeded)
	cancel()
	err := <-stopped
	require.ErrorIs(t, err, firstErr, "node deadline must not overwrite the failed stop's error")
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, r.m.instances.Has(run.ID))
	require.True(t, run.StopUnconfirmed.Load())
	stops := 0
	for len(r.m.auditEvents) > 0 {
		if (<-r.m.auditEvents).Type == EventInstanceStopping {
			stops++
		}
	}
	require.Equal(t, 1, stops, "the sweep must never reclaim a failed first-pass stop")
}

func TestNodeStopRunDeadlinePreservesDaemonGrace(t *testing.T) {
	r := newContractRig(t, false)
	parent, cancelParent := context.WithCancel(t.Context())
	defer cancelParent()
	r.m.Start(parent)
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		svc.Defaults.Metadata = &config.AppMetadata{ShutdownTimeout: "100ms"}
		return nil
	}))
	run, runDir := nodeStopRun(t, r.m, 0, 24*time.Hour)
	run.TrackGoroutine()
	released := false
	defer func() {
		if !released {
			run.GoroutineDone()
		}
	}()
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	daemon := &testDaemonInstaller{binary: binary, args: []string{"-test.run=^TestNodeStopEngineHelper$", "--", "node-stop-engine", dir, "400ms"}}
	require.NoError(t, r.m.controlDaemon(t.Context(), "ollama", "start", config.ServiceConfig{}, daemon, false))
	state := r.m.serviceState("ollama")
	state.mu.Lock()
	done := state.done
	state.mu.Unlock()
	t.Cleanup(func() { _ = state.stop(context.Background(), false); <-done })
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil }, contractWait, time.Millisecond)
	state.mu.Lock()
	pid := state.status.PID
	state.mu.Unlock()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	cancelParent()
	require.ErrorIs(t, r.m.shutdownCtx.Err(), context.Canceled, "background work must follow server cancellation")
	require.ErrorIs(t, r.m.startsCtx.Err(), context.Canceled, "background starts must end during server drain")
	select {
	case <-r.m.reaperDone:
	case <-time.After(contractWait):
		t.Fatal("idle reaper outlived server cancellation")
	}
	require.NoError(t, r.m.daemonsCtx.Err(), "server cancellation must leave the daemon serving until the manager's daemon phase")
	require.NoError(t, syscall.Kill(-pid, 0))
	_, err = os.Stat(filepath.Join(dir, "signals"))
	require.True(t, os.IsNotExist(err), "the daemon must receive no signal before run teardown")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.Stop(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(runDir, "signals")); return err == nil }, contractWait, time.Millisecond)
	require.NoError(t, r.m.daemonsCtx.Err(), "the daemon must keep serving while the run phase is open")
	_, err = os.Stat(filepath.Join(dir, "signals"))
	require.True(t, os.IsNotExist(err), "the daemon must receive no signal during the run phase")
	run.GoroutineDone()
	released = true
	require.ErrorIs(t, <-stopped, context.DeadlineExceeded)
	require.False(t, r.m.instances.Has(run.ID))
	_, err = os.Stat(filepath.Join(dir, "exited"))
	require.NoError(t, err, "the daemon must finish its own grace after the run budget expires")
	select {
	case <-done:
	default:
		t.Fatal("manager returned before the daemon exited")
	}
	count, err := os.ReadFile(filepath.Join(dir, "signals"))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, count)
}

func TestNodeStopSignalsEveryDaemonBeforeWaiting(t *testing.T) {
	r := newContractRig(t, false)
	r.m.Start(t.Context())
	binary, err := os.Executable()
	require.NoError(t, err)
	var dirs []string
	var pids []int
	for _, name := range []string{"first", "second"} {
		dir := t.TempDir()
		daemon := &testDaemonInstaller{binary: binary, args: []string{"-test.run=^TestNodeStopEngineHelper$", "--", "node-stop-engine", dir, "0s"}}
		require.NoError(t, r.m.controlDaemon(t.Context(), name, "start", config.ServiceConfig{}, daemon, false))
		state := r.m.serviceState(name)
		state.mu.Lock()
		done := state.done
		state.mu.Unlock()
		t.Cleanup(func() { _ = state.stop(context.Background(), false); <-done })
		require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "ready")); return err == nil }, contractWait, time.Millisecond)
		state.mu.Lock()
		pid := state.status.PID
		state.mu.Unlock()
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		dirs, pids = append(dirs, dir), append(pids, pid)
	}
	ctx, cancel := context.WithTimeout(t.Context(), constants.ShutdownRunTimeout)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.Stop(ctx) }()
	require.Eventually(t, func() bool {
		for _, dir := range dirs {
			if _, err := os.Stat(filepath.Join(dir, "signals")); err != nil {
				return false
			}
		}
		return true
	}, contractWait, time.Millisecond)
	for _, pid := range pids {
		require.NoError(t, syscall.Kill(-pid, 0), "every daemon must receive TERM while all are still in grace")
	}
	for _, dir := range dirs {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "release"), nil, 0600))
	}
	require.NoError(t, <-stopped)
	for _, dir := range dirs {
		_, err := os.Stat(filepath.Join(dir, "exited"))
		require.NoError(t, err, "each daemon must finish its own grace")
		count, err := os.ReadFile(filepath.Join(dir, "signals"))
		require.NoError(t, err)
		require.Equal(t, []byte{1}, count)
	}
}

func TestNodeStopBudgetBoundsAlreadyStoppingRun(t *testing.T) {
	r := newContractRig(t, false)
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		svc.Defaults.Metadata = &config.AppMetadata{ShutdownTimeout: "60s"}
		return nil
	}))
	run, dir := nodeStopRun(t, r.m, 0, 24*time.Hour)
	stopped := make(chan error, 1)
	go func() { stopped <- r.m.StopInstance(t.Context(), run.ID) }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(dir, "signals")); return err == nil }, contractWait, time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_ = r.m.Stop(ctx)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(contractWait):
		t.Fatal("an existing stop must escalate when the node run budget expires")
	}
	require.ErrorIs(t, syscall.Kill(-run.GetProcessGroupID(), 0), syscall.ESRCH)
	_, err := os.Stat(filepath.Join(dir, "exited"))
	require.True(t, os.IsNotExist(err), "run must be killed before its delayed exit")
	count, err := os.ReadFile(filepath.Join(dir, "signals"))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, count)
}

func TestCancelledStopLeavesRunServing(t *testing.T) {
	r := newContractRig(t, false)
	run, dir := nodeStopRun(t, r.m, 0, 400*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, r.m.StopInstance(ctx, run.ID), context.Canceled)
	require.Equal(t, instance.StatusRunning, run.GetStatus())
	require.NoError(t, run.Context().Err())
	require.NoError(t, syscall.Kill(-run.GetProcessGroupID(), 0))
	_, err := os.Stat(filepath.Join(dir, "signals"))
	require.True(t, os.IsNotExist(err), "cancelled stop must neither signal nor close launcher stdin")
}

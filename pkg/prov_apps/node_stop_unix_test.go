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

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
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
	count := byte(0)
	for {
		select {
		case <-signals:
			count++
			if err := os.WriteFile(filepath.Join(dir, "signals"), []byte{count}, 0600); err != nil {
				os.Exit(2)
			}
			if count == 1 {
				finished = time.After(delay)
			}
		case <-finished:
			syscall.Exit(0)
		}
	}
}

func nodeStopRun(t *testing.T, m *ProviderAppManager, n int, exitDelay time.Duration) (*instance.Instance, string) {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	require.NoError(t, err)
	run := instance.NewInstance("stop-"+strconv.Itoa(n), "generic", "model-"+strconv.Itoa(n), 0, 0, 8)
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
	for n := range 3 {
		run, dir := nodeStopRun(t, r.m, n, 400*time.Millisecond)
		if n == 0 {
			run.SetKeepAlive(-1)
			r.m.pins.update(EnsureRequest{Provider: run.Provider, Model: run.Model, Parameters: map[string]string{"keep-alive": "-1"}}, true, r.m.AppsConfig())
		}
		dirs = append(dirs, dir)
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
	}, 300*time.Millisecond, time.Millisecond, "every group must be signaled before the first run finishes")
	require.NoError(t, <-stopped)
	for _, dir := range dirs {
		count, err := os.ReadFile(filepath.Join(dir, "signals"))
		require.NoError(t, err)
		require.Equal(t, []byte{1}, count)
	}
	require.Empty(t, r.m.instances.List())
	require.Equal(t, pins, readPins(t), "node stop must preserve the pinned run record")
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
	require.GreaterOrEqual(t, constants.ShutdownRunTimeout, constants.EngineMaxGracefulTimeout+process.ForceTimeout+constants.ShutdownRunEpilogueTimeout)
	require.Equal(t, constants.EngineGracefulTimeout, process.GracefulTimeout)
	require.Greater(t, constants.SupervisorStopTimeout, constants.ShutdownDrainTimeout+constants.ShutdownRunTimeout+process.ForceTimeout+constants.ShutdownEpilogueTimeout)
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
			if _, ok := r.m.instances.Get(run.ID); ok {
				require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
			}
			count, err := os.ReadFile(filepath.Join(dir, "signals"))
			require.NoError(t, err)
			require.Equal(t, []byte{1}, count)
		})
	}
}

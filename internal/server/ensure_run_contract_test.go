package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario 4 of the EnsureRun contract (pkg/prov_apps/ensure_run_contract_test.go)
// lives here because readiness waiting does: a readiness waiter holds the run
// it waits on, so a crash reaches it as that run's cause even when a later
// caller has already replaced the run.

const (
	readinessWaiters = 16
	readinessWait    = 10 * time.Second
	// readinessBaseGapsEnv runs the scenarios the base cannot pass yet.
	readinessBaseGapsEnv = "ZZR_CONTRACT_BASE_GAPS"
)

// crashingEngine stays loading until $0.crash exists, then exits; every
// later launch of it becomes ready.
const crashingEngine = `#!/bin/sh
trap 'kill $! 2>/dev/null; exit 0' TERM INT
if [ -e "$0.crashed" ]; then
  printf 'READY\n'
  while :; do /bin/sleep 1 & wait $!; done
fi
while [ ! -e "$0.crash" ]; do /bin/sleep 0.01; done
touch "$0.crashed"
echo 'engine crashed while loading' >&2
exit 3
`

type readinessRig struct {
	s      *Server
	mgr    *prov_apps.ProviderAppManager
	engine string
}

func newReadinessRig(t *testing.T) *readinessRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	r := &readinessRig{engine: filepath.Join(t.TempDir(), "engine")}
	require.NoError(t, os.WriteFile(r.engine, []byte(crashingEngine), 0o700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("fake", config.ServiceConfig{
		Enabled: new(true), Name: "fake", Protocol: config.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8780, 8789}, BasePort: 8780,
			Execution: config.ExecutionConfig{Type: "cli", Command: r.engine},
			HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{
				Timeout: "30s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}},
			}},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	mgr, err := prov_apps.NewProviderAppManager(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		assert.NoError(t, mgr.Stop(ctx))
	})
	r.mgr = mgr
	r.s = &Server{}
	r.s.providers.appMgr = mgr
	return r
}

type readinessOutcome struct {
	run *instance.Instance
	err error
}

// loadingRun starts the crashing engine and parks waiters on its readiness.
func (r *readinessRig) loadingRun(t *testing.T) (*instance.Instance, []chan readinessOutcome) {
	t.Helper()
	result, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "m"})
	require.NoError(t, err)
	outs := make([]chan readinessOutcome, readinessWaiters)
	for i := range outs {
		outs[i] = make(chan readinessOutcome, 1)
		go func() {
			run, err := r.s.loadModelViaNodeAPI("m", "fake", "chat")
			outs[i] <- readinessOutcome{run, err}
		}()
	}
	waitParkedIn(t, ").loadModelViaNodeAPI(", readinessWaiters)
	return result.Run, outs
}

func (r *readinessRig) crash(t *testing.T, run *instance.Instance) {
	t.Helper()
	require.NoError(t, os.WriteFile(r.engine+".crash", nil, 0o600))
	select {
	case <-run.LeftStarting():
	case <-time.After(readinessWait):
		t.Fatal("run never left starting")
	}
	require.Equal(t, instance.StatusFailed, run.GetStatus())
}

// assertCause requires every waiter to report the crashed run's own failure.
func assertCause(t *testing.T, run *instance.Instance, outs []chan readinessOutcome) {
	t.Helper()
	for _, out := range outs {
		var o readinessOutcome
		select {
		case o = <-out:
		case <-time.After(readinessWait):
			t.Fatal("a readiness waiter never returned")
		}
		require.Error(t, o.err)
		assert.Nil(t, o.run)
		if cause := run.FailureCause(); cause != nil {
			assert.ErrorIs(t, o.err, cause)
		} else {
			assert.ErrorContains(t, o.err, run.GetErrorMessage())
		}
	}
}

func TestEnsureRunContractReadinessWaiters(t *testing.T) {
	t.Run("4a crash while loading reaches every waiter", func(t *testing.T) {
		r := newReadinessRig(t)
		run, outs := r.loadingRun(t)
		r.crash(t, run)
		assertCause(t, run, outs)
	})

	t.Run("4b a replacement never hides the cause from an old waiter", func(t *testing.T) {
		if os.Getenv(readinessBaseGapsEnv) == "" {
			t.Skip("base gap: waiters re-read the registry by id; once the failed run is replaced they answer \"disappeared while waiting\"")
		}
		r := newReadinessRig(t)
		run, outs := r.loadingRun(t)
		r.crash(t, run)
		replaced, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "m"})
		require.NoError(t, err)
		require.NotEqual(t, run.ID, replaced.Run.ID)
		assertCause(t, run, outs)
		select {
		case <-replaced.Run.LeftStarting():
		case <-time.After(readinessWait):
			t.Fatal("replacement never left starting")
		}
	})
}

// waitParkedIn returns once exactly n goroutines through frame exist and all
// are blocked, on two consecutive looks.
func waitParkedIn(t *testing.T, frame string, n int) {
	t.Helper()
	blockedStates := []string{"chan receive", "chan send", "select", "semacquire", "sync.Cond.Wait", "sync.Mutex.Lock", "sync.RWMutex.Lock", "sync.RWMutex.RLock", "sync.WaitGroup.Wait"}
	stable, total, blocked := 0, 0, 0
	for end := time.Now().Add(readinessWait); time.Now().Before(end); runtime.Gosched() {
		buf := make([]byte, 1<<20)
		size := runtime.Stack(buf, true)
		for size == len(buf) {
			buf = make([]byte, 2*len(buf))
			size = runtime.Stack(buf, true)
		}
		buf = buf[:size]
		total, blocked = 0, 0
		for _, g := range strings.Split(string(buf), "\n\n") {
			if !strings.Contains(g, frame) {
				continue
			}
			total++
			header, _, _ := strings.Cut(g, "\n")
			_, state, _ := strings.Cut(header, "[")
			for _, s := range blockedStates {
				if strings.HasPrefix(state, s) {
					blocked++
					break
				}
			}
		}
		if total == n && blocked == n {
			if stable++; stable == 2 {
				return
			}
			continue
		}
		stable = 0
		time.Sleep(time.Millisecond) // poll interval, not synchronization
	}
	t.Fatalf("want %d goroutines parked in %s, have %d (%d blocked)", n, frame, total, blocked)
}

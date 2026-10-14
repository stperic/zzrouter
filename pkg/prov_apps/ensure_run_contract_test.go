package prov_apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	"github.com/stperic/zzrouter/pkg/jobs"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// The EnsureRun contract on the real clock: the start mechanism may change,
// these outcomes may not. Interleavings are forced through option seams and
// counted from goroutine stacks, never from sleeps.

// contractCallers is how many callers ask for one model at once.
const contractCallers = 16

// contractWait bounds every wait in this file; reaching it is a failure.
const contractWait = 10 * time.Second

// contractEngine prints READY once $0.ready exists, then serves until
// stopped, recording each graceful stop in $0.terms (the launcher
// forwards INT when its stdin closes, TERM when signalled).
const contractEngine = `#!/bin/sh
trap 'echo "$$" >> "$0.terms"; kill $! 2>/dev/null; exit 0' TERM INT
while [ ! -e "$0.ready" ]; do /bin/sleep 0.01; done
printf 'READY\n'
while :; do /bin/sleep 1 & wait $!; done
`

type contractRig struct {
	m        *ProviderAppManager
	engine   string
	attempts atomic.Int32
}

// newContractRig builds two on-demand providers, generic and other, over
// one fake engine; holdReady keeps runs starting until ready is called.
func newContractRig(t *testing.T, holdReady bool, opts ...Option) *contractRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	r := &contractRig{engine: filepath.Join(t.TempDir(), "engine")}
	require.NoError(t, os.WriteFile(r.engine, []byte(contractEngine), 0o700))
	if !holdReady {
		r.ready(t)
	}
	cfg := &config.AppsConfig{}
	for name, base := range map[string]int{"generic": 8740, "other": 8760} {
		require.NoError(t, cfg.AddApp(name, config.ServiceConfig{
			Enabled: new(true), Name: name, Protocol: config.ProtocolOpenAI, Mode: "on-demand",
			Runtime: &config.AppRuntimeConfig{
				PortRange: []int{base, base + 9}, BasePort: base,
				Execution: config.ExecutionConfig{Type: "cli", Command: r.engine},
				HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{
					Timeout: "10s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}},
				}},
			},
			Defaults:     &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55"}},
			Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
		}))
	}
	observe := WithStartObserver(func(ctx context.Context, _ EnsureRequest) (context.Context, func(EnsureResult, error)) {
		r.attempts.Add(1)
		return ctx, func(EnsureResult, error) {}
	})
	m, err := NewProviderAppManager(cfg, append([]Option{observe}, opts...)...)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	r.m = m
	return r
}

func (r *contractRig) ready(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(r.engine+".ready", nil, 0o600))
}

func (r *contractRig) terms(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(r.engine + ".terms")
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(data), "\n")
}

type outcome struct {
	result EnsureResult
	err    error
}

func (r *contractRig) ensure(ctx context.Context, req EnsureRequest) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		result, err := r.m.EnsureRun(ctx, req)
		out <- outcome{result, err}
	}()
	return out
}

// requireSharedRun asserts every caller got the same run and returns how many
// report starting it.
func requireSharedRun(t *testing.T, results []outcome) int {
	t.Helper()
	started := 0
	for _, o := range results {
		require.NoError(t, o.err)
		require.NotNil(t, o.result.Run)
		assert.Equal(t, results[0].result.Run.ID, o.result.Run.ID)
		if o.result.Started {
			started++
		}
	}
	return started
}

func collect(t *testing.T, outs []<-chan outcome) []outcome {
	t.Helper()
	results := make([]outcome, 0, len(outs))
	for _, out := range outs {
		select {
		case o := <-out:
			results = append(results, o)
		case <-time.After(contractWait):
			t.Fatal("a caller never returned")
		}
	}
	return results
}

func awaitRunning(t *testing.T, run *instance.Instance) {
	t.Helper()
	select {
	case <-run.LeftStarting():
	case <-time.After(contractWait):
		t.Fatal("run never left starting")
	}
	require.Equal(t, instance.StatusRunning, run.GetStatus(), run.GetErrorMessage())
}

// barrier holds the first goroutine to reach it until released; later
// arrivals pass straight through.
type barrier struct {
	first            atomic.Bool
	entered, release chan struct{}
	releaseOnce      sync.Once
}

func newBarrier() *barrier {
	return &barrier{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *barrier) hold() {
	if b.first.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
}

func (b *barrier) open() { b.releaseOnce.Do(func() { close(b.release) }) }

func (b *barrier) reached(t *testing.T) {
	t.Helper()
	t.Cleanup(b.open)
	select {
	case <-b.entered:
	case <-time.After(contractWait):
		t.Fatal("barrier never reached")
	}
}

// blockedStates are goroutine states that wait on another goroutine.
var blockedStates = []string{"chan receive", "chan send", "select", "semacquire", "sync.Cond.Wait", "sync.Mutex.Lock", "sync.RWMutex.Lock", "sync.RWMutex.RLock", "sync.WaitGroup.Wait"}

// parked counts goroutines with frame on their stack, and how many of
// them are blocked.
func parked(frame string) (total, blocked int) {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
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
	return total, blocked
}

// waitParkedOn returns once exactly n goroutines through frame exist and
// all are blocked, on two consecutive looks: a goroutine that only
// contends for a mutex has a runnable holder among them, so it cannot
// pass for a parked one.
func waitParkedOn(t *testing.T, frame string, n int, deadline time.Duration) {
	t.Helper()
	stable := 0
	var total, blocked int
	for end := time.Now().Add(deadline); time.Now().Before(end); runtime.Gosched() {
		total, blocked = parked(frame)
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

func waitParked(t *testing.T, n int, deadline time.Duration) {
	t.Helper()
	waitParkedOn(t, ").EnsureRun(", n, deadline)
}

// leaderAndJoiners starts one caller, waits for it at b, then parks the
// joiners behind it; reqs[0] is the leader's request.
func (r *contractRig) leaderAndJoiners(t *testing.T, b *barrier, ctxs []context.Context, reqs []EnsureRequest) []<-chan outcome {
	t.Helper()
	outs := []<-chan outcome{r.ensure(ctxs[0], reqs[0])}
	b.reached(t)
	for i := 1; i < len(reqs); i++ {
		outs = append(outs, r.ensure(ctxs[i], reqs[i]))
	}
	waitParked(t, len(reqs), contractWait)
	return outs
}

func same(ctx context.Context, req EnsureRequest) ([]context.Context, []EnsureRequest) {
	ctxs, reqs := make([]context.Context, contractCallers), make([]EnsureRequest, contractCallers)
	for i := range reqs {
		ctxs[i], reqs[i] = ctx, req
	}
	return ctxs, reqs
}

func catalogBarrier(b *barrier) Option {
	return WithLocalCatalog(func(context.Context, string) (bool, error) { b.hold(); return true, nil })
}

// failFirstLocalize fails the first launch's parameters after its run registered.
func failFirstLocalize(cause error) Option {
	var calls atomic.Int32
	return WithParamLocalizer(func(_, _, _ string, params map[string]string) (Localized, error) {
		if calls.Add(1) == 1 {
			return Localized{}, cause
		}
		return Localized{Params: params}, nil
	})
}

// errorLog counts error records naming one model.
type errorLog struct {
	model string
	n     atomic.Int32
}

func (h *errorLog) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelError }
func (h *errorLog) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "model" && a.Value.String() == h.model {
			h.n.Add(1)
			return false
		}
		return true
	})
	return nil
}
func (h *errorLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *errorLog) WithGroup(string) slog.Handler      { return h }

func TestEnsureRunContract(t *testing.T) {
	req := EnsureRequest{Provider: "generic", Model: "shared"}

	t.Run("1 success shares one start", func(t *testing.T) {
		b := newBarrier()
		r := newContractRig(t, false, catalogBarrier(b))
		ctxs, reqs := same(t.Context(), req)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		results := collect(t, outs)
		assert.Equal(t, int32(1), r.attempts.Load())
		assert.Equal(t, 1, requireSharedRun(t, results))
		assert.Len(t, r.m.instances.List(), 1, "the registry holds exactly one run")
		awaitRunning(t, results[0].result.Run)
	})

	t.Run("2 failure before registration reaches every caller", func(t *testing.T) {
		b := newBarrier()
		refusal := &process.MemoryFitError{Parameter: "budget", Device: "GPU-A", RequiredMiB: 9500, FreeMiB: 6000}
		r := newContractRig(t, false, WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
			b.hold()
			return nil, refusal
		}))
		ctxs, reqs := same(t.Context(), req)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		for _, o := range collect(t, outs) {
			require.ErrorIs(t, o.err, refusal)
		}
		assert.Equal(t, int32(1), r.attempts.Load())
		assert.Empty(t, r.m.instances.List())
	})

	t.Run("3 launch error after registration, then a fresh attempt", func(t *testing.T) {
		b := newBarrier()
		r := newContractRig(t, false, failFirstLocalize(errTestStart), WithModelSource(func(context.Context, string) string { b.hold(); return "" }))
		ctxs, reqs := same(t.Context(), req)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		for _, o := range collect(t, outs) {
			require.ErrorIs(t, o.err, errTestStart)
		}
		assert.Equal(t, int32(1), r.attempts.Load())
		late := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
		require.NoError(t, late.err)
		assert.True(t, late.result.Started)
		assert.Equal(t, int32(2), r.attempts.Load())
		awaitRunning(t, late.result.Run)
	})

	t.Run("3b a caller reusing a run released before launch gets its cause", func(t *testing.T) {
		b := newBarrier()
		var calls atomic.Int32
		r := newContractRig(t, false, WithParamLocalizer(func(_, _, _ string, params map[string]string) (Localized, error) {
			if calls.Add(1) == 1 {
				b.hold() // registered, not launched
				return Localized{}, errTestStart
			}
			return Localized{Params: params}, nil
		}))
		leader := r.ensure(t.Context(), req)
		b.reached(t)
		fast := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
		require.NoError(t, fast.err)
		require.NotNil(t, fast.result.Run, "the registered run is reused while it starts")
		assert.False(t, fast.result.Started)
		b.open()
		require.ErrorIs(t, collect(t, []<-chan outcome{leader})[0].err, errTestStart)
		select {
		case <-fast.result.Run.LeftStarting():
		case <-time.After(contractWait):
			t.Fatal("a run released before launch never left starting")
		}
		require.ErrorIs(t, fast.result.Run.FailureCause(), errTestStart)
	})

	t.Run("3c a caller after the flight ended starts fresh", func(t *testing.T) {
		b, removed := newBarrier(), newBarrier()
		r := newContractRig(t, false, failFirstLocalize(errTestStart), WithModelSource(func(context.Context, string) string { b.hold(); return "" }))
		r.m.seams.flightRemoved = removed.hold
		ctxs, reqs := same(t.Context(), req)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		removed.reached(t) // the failed flight is gone; its callers are not answered yet
		late := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
		require.NoError(t, late.err, "a caller that finds no flight observes the registry")
		assert.True(t, late.result.Started)
		removed.open()
		for _, o := range collect(t, outs) {
			require.ErrorIs(t, o.err, errTestStart)
		}
		assert.Equal(t, int32(2), r.attempts.Load())
		awaitRunning(t, late.result.Run)
	})

	t.Run("5 concurrent force replaces a ready run once", func(t *testing.T) {
		b := newBarrier()
		var gate atomic.Bool
		r := newContractRig(t, false, WithLocalCatalog(func(context.Context, string) (bool, error) {
			if gate.Load() {
				b.hold()
			}
			return true, nil
		}))
		old := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
		require.NoError(t, old.err)
		awaitRunning(t, old.result.Run)
		gate.Store(true)
		force := req
		force.Force = true
		ctxs, reqs := same(t.Context(), force)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		results := collect(t, outs)
		// No caller is refused while the old run stops.
		requireSharedRun(t, results)
		assert.NotEqual(t, old.result.Run.ID, results[0].result.Run.ID)
		assert.Equal(t, instance.StatusStopped, old.result.Run.GetStatus())
		assert.Equal(t, 1, r.terms(t), "the old run is stopped once")
		assert.Equal(t, int32(2), r.attempts.Load())
		assert.Len(t, r.m.instances.List(), 1)
		awaitRunning(t, results[0].result.Run)
	})

	t.Run("5b force and plain callers share one cold start", func(t *testing.T) {
		for _, forceLeads := range []bool{false, true} {
			t.Run(fmt.Sprintf("force leads=%t", forceLeads), func(t *testing.T) {
				b := newBarrier()
				r := newContractRig(t, false, catalogBarrier(b))
				ctxs, reqs := same(t.Context(), req)
				for i := range reqs {
					reqs[i].Force = (i == 0) == forceLeads
				}
				outs := r.leaderAndJoiners(t, b, ctxs, reqs)
				b.open()
				results := collect(t, outs)
				requireSharedRun(t, results)
				assert.True(t, results[0].result.Started)
				assert.Equal(t, int32(1), r.attempts.Load())
				assert.Len(t, r.m.instances.List(), 1)
				awaitRunning(t, results[0].result.Run)
				assert.Zero(t, r.terms(t), "nobody replaced the run the flight started")
			})
		}
	})

	t.Run("6 cancelled callers never cancel the start", func(t *testing.T) {
		b := newBarrier()
		r := newContractRig(t, false, catalogBarrier(b))
		ctxs, reqs := same(t.Context(), req)
		leaderCtx, cancelLeader := context.WithCancel(t.Context())
		joinerCtx, cancelJoiner := context.WithCancel(t.Context())
		ctxs[0], ctxs[1] = leaderCtx, joinerCtx
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		cancelLeader()
		cancelJoiner()
		b.open()
		results := collect(t, outs)
		require.ErrorIs(t, results[0].err, context.Canceled)
		require.ErrorIs(t, results[1].err, context.Canceled)
		assert.Equal(t, int32(1), r.attempts.Load())
		for _, o := range results[2:] {
			require.NoError(t, o.err)
			require.NotNil(t, o.result.Run)
			assert.Equal(t, results[2].result.Run.ID, o.result.Run.ID)
		}
		awaitRunning(t, results[2].result.Run)
	})

	t.Run("7 stop while held drains", func(t *testing.T) {
		ignore := goleak.IgnoreCurrent()
		b := newBarrier()
		r := newContractRig(t, false, catalogBarrier(b))
		ctxs, reqs := same(t.Context(), req)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		stopped := make(chan error, 1)
		go func() { stopped <- r.m.Stop(context.Background()) }()
		waitParkedOn(t, "ProviderAppManager).Stop(", 1, contractWait)
		select {
		case <-stopped:
			t.Fatal("Stop returned while a start was held")
		default:
		}
		b.open()
		results := collect(t, outs)
		require.NoError(t, <-stopped)
		for _, o := range results {
			if o.err != nil {
				assert.ErrorIs(t, o.err, ErrShutdown)
			} else {
				assert.NotNil(t, o.result.Run)
			}
		}
		assert.Empty(t, r.m.instances.List())
		goleak.VerifyNone(t, ignore)
	})

	t.Run("8 detached leader", func(t *testing.T) {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("fails=%t", fails), func(t *testing.T) {
				b := newBarrier()
				opts := []Option{WithModelSource(func(context.Context, string) string { b.hold(); return "" })}
				if fails {
					opts = append(opts, failFirstLocalize(errTestStart))
				}
				r := newContractRig(t, false, opts...)
				log := &errorLog{model: req.Model}
				previous := slog.Default()
				slog.SetDefault(slog.New(log))
				t.Cleanup(func() { slog.SetDefault(previous) })
				detached := req
				detached.Detached = true
				_, err := r.m.EnsureRun(t.Context(), detached)
				require.NoError(t, err)
				b.reached(t)
				var outs []<-chan outcome
				for range contractCallers {
					outs = append(outs, r.ensure(t.Context(), req))
				}
				waitParked(t, contractCallers, contractWait)
				b.open()
				results := collect(t, outs)
				for _, o := range results {
					if fails {
						require.ErrorIs(t, o.err, errTestStart)
						continue
					}
					require.NoError(t, o.err)
					require.NotNil(t, o.result.Run)
					assert.False(t, o.result.Started, "only the detached leader started the run")
					assert.Equal(t, results[0].result.Run.ID, o.result.Run.ID)
				}
				if !fails {
					awaitRunning(t, results[0].result.Run)
				}
				ctx, cancel := context.WithTimeout(context.Background(), contractWait)
				defer cancel()
				require.NoError(t, r.m.Stop(ctx)) // drains the detached start and its log
				want := int32(0)
				if fails {
					want = 1
				}
				assert.Equal(t, want, log.n.Load(), "a detached failure is logged exactly once")
				assert.Equal(t, int32(1), r.attempts.Load())
			})
		}
	})

	t.Run("9 another provider joins", func(t *testing.T) {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("fails=%t", fails), func(t *testing.T) {
				b := newBarrier()
				opts := []Option{catalogBarrier(b)}
				if fails {
					opts = append(opts, failFirstLocalize(errTestStart))
				}
				r := newContractRig(t, false, opts...)
				ctxs, reqs := same(t.Context(), req)
				for i := 1; i < len(reqs); i++ {
					reqs[i].Provider = "other"
				}
				outs := r.leaderAndJoiners(t, b, ctxs, reqs)
				b.open()
				results := collect(t, outs)
				assert.Equal(t, int32(1), r.attempts.Load())
				if fails {
					for _, o := range results {
						require.ErrorIs(t, o.err, errTestStart)
					}
					return
				}
				require.NoError(t, results[0].err)
				assert.True(t, results[0].result.Started)
				for _, o := range results[1:] {
					require.ErrorIs(t, o.err, ErrRunHeldByOtherProvider)
				}
				awaitRunning(t, results[0].result.Run)
			})
		}
	})

	t.Run("10 two budgeted models queue", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte("#!/bin/sh\ncase \"$1\" in\n--query-gpu=*) echo 'GPU-A, 0, 10000, 6000' ;;\nesac\n"), 0o700))
		t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("CUDA_VISIBLE_DEVICES", "0")
		reg, err := jobs.NewRegistry(jobs.Config{NodeName: "test", Clock: clock.System()})
		require.NoError(t, err)
		t.Cleanup(reg.Stop)
		r := newContractRig(t, true, WithJobsRegistry(reg), WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
			return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
		}))
		budgeted := func(model string) *instance.Instance {
			t.Helper()
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: model})})[0]
			require.NoError(t, o.err)
			return o.result.Run
		}
		first := budgeted("one")
		require.True(t, first.Admitted())
		second := budgeted("two")
		assert.Equal(t, instance.StatusStarting, second.GetStatus())
		assert.False(t, second.Admitted())
		assert.Equal(t, first.ID, second.ToInfo("").WaitingOn)
		require.NotEmpty(t, second.StreamJobID)
		require.Eventually(t, func() bool {
			event, err := reg.Get(second.StreamJobID)
			return err == nil && event.Meta["waiting_on"] == first.ID
		}, contractWait, time.Millisecond, "the queued run's job names what it waits on")
		third := budgeted("three")
		require.NoError(t, r.m.StopInstance(t.Context(), third.ID), "a queued run is stoppable")
		_, listed := r.m.instances.Get(third.ID)
		assert.False(t, listed)
		_, err = r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Runtime: "generic", Model: "smoke", Origin: OriginInstall})
		require.ErrorIs(t, err, errAdmissionBusy)
		assert.Len(t, r.m.instances.List(), 2)
		r.ready(t)
		awaitRunning(t, first)
		awaitRunning(t, second)
		assert.True(t, second.Admitted())
	})

	t.Run("11 a stopping run answers retry later", func(t *testing.T) {
		r := newContractRig(t, false)
		o := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
		require.NoError(t, o.err)
		run := o.result.Run
		awaitRunning(t, run)
		run.TrackGoroutine() // holds the stop in its drain
		var held sync.Once
		drop := func() { held.Do(run.GoroutineDone) }
		t.Cleanup(drop)
		stopped := make(chan error, 1)
		go func() { stopped <- r.m.StopInstance(context.Background(), run.ID) }()
		waitParkedOn(t, ").stopInstance(", 1, contractWait)
		require.Equal(t, instance.StatusStopping, run.GetStatus())
		_, err := r.m.EnsureRun(t.Context(), req)
		var admission *instance.AdmissionError
		require.ErrorAs(t, err, &admission)
		require.ErrorIs(t, err, instance.ErrStopping)
		assert.Positive(t, admission.RetryAfter)
		drop()
		require.NoError(t, <-stopped)
	})

	t.Run("12 a smoke during a flight runs on its own, no deadlock", func(t *testing.T) {
		r := newContractRig(t, false)
		release, err := r.m.installs.acquireProvider(t.Context(), "generic") // the installer's gate
		require.NoError(t, err)
		unlock := sync.OnceFunc(release)
		t.Cleanup(unlock)
		leader := r.ensure(t.Context(), req)
		waitParkedOn(t, ").acquireProvider(", 1, contractWait)
		smoke := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Runtime: "generic", Model: req.Model, Origin: OriginInstall})})[0]
		// The installer smoke-tests and stops its run before releasing its gate.
		require.NoError(t, smoke.err)
		require.NoError(t, r.m.StopInstance(t.Context(), smoke.result.Run.ID))
		unlock()
		o := collect(t, []<-chan outcome{leader})[0]
		require.NoError(t, o.err)
		assert.True(t, o.result.Started)
		assert.Len(t, r.m.instances.List(), 1)
		awaitRunning(t, o.result.Run)
	})
}

// G3: a stop that lands before the run's process is recorded still ends it,
// instead of draining until a deadline.
func TestEnsureRunContractStopBeforeProcessRecorded(t *testing.T) {
	const stopWait = 5 * time.Second // half the rig's readiness timeout
	for _, stage := range []string{"before launch", "after process start"} {
		t.Run(stage, func(t *testing.T) {
			r := newContractRig(t, true)
			b := newBarrier()
			if stage == "before launch" {
				r.m.seams.beforeLaunch = b.hold
			} else {
				r.m.seams.processStarted = b.hold
			}
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "shared"})})[0]
			require.NoError(t, o.err)
			run := o.result.Run
			b.reached(t)
			stopped := make(chan error, 1)
			go func() { stopped <- r.m.StopInstance(context.Background(), run.ID) }()
			waitParkedOn(t, ").stopInstance(", 1, contractWait)
			b.open()
			select {
			case err := <-stopped:
				require.NoError(t, err)
			case <-time.After(stopWait):
				t.Fatal("StopInstance waited on a process nobody signalled")
			}
			assert.LessOrEqual(t, r.terms(t), 1, "the process is stopped once")
			_, err := os.Stat(filepath.Join(config.Paths().GetPIDDir(), run.ID+".pid"))
			assert.True(t, os.IsNotExist(err), "no tracked pid left")
			pid := run.GetProcessID()
			if stage == "before launch" {
				assert.Zero(t, pid, "a run stopped before launch spawns nothing")
			} else if assert.Positive(t, pid, "a started process is recorded") {
				assert.False(t, process.ProcessExists(int32(pid)), "the process is gone")
			}
			_, listed := r.m.instances.Get(run.ID)
			assert.False(t, listed)
		})
	}
}

// The served contract: an Ollama model is a run like a launched one, started,
// reused, stopped and reconciled through the same core. The daemon is a fake
// answering the Ollama API over the manager's injected client.

const (
	servedModel   = "m"
	servedLoaded  = "m:latest" // the daemon's name for servedModel
	servedBase    = "http://daemon:11434"
	daemonForever = 200 * 365 * 24 * time.Hour // how far ahead the daemon reports an infinite expiry
	settle        = 50 * time.Millisecond      // a window in which nothing must happen
)

// daemonCall is one load or unload the daemon received.
type daemonCall struct {
	model     string
	unload    bool
	body      map[string]any
	keepAlive any
}

// fakeDaemon is an Ollama daemon: loads, unloads and lists models, and can be
// held, failed, made unreachable or made to report stale state.
type fakeDaemon struct {
	mu            sync.Mutex
	loaded        map[string]time.Time
	calls         []daemonCall
	hold          chan struct{}
	entered       chan struct{}
	loadStatus    int
	loadDelay     time.Duration
	unreachable   bool
	psErr         bool
	psHook        func()
	beforeRequest func(*http.Request)
	stale         map[string]bool
	unloadHold    chan struct{} // an unload drops the model, then answers once this closes
	psAuth        []string      // the Authorization header of each listing
	holdsOne      bool          // a load unloads every other model, as a daemon short of memory does
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{loaded: map[string]time.Time{}, stale: map[string]bool{}}
}

func daemonReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
}

func (d *fakeDaemon) RoundTrip(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	unreachable, before := d.unreachable, d.beforeRequest
	d.mu.Unlock()
	if before != nil {
		before(req)
	}
	if unreachable {
		return nil, errors.New("dial tcp: connection refused")
	}
	switch req.URL.Path {
	case "/v1/models":
		return daemonReply(http.StatusOK, `{"data":[]}`), nil
	case "/api/ps":
		d.set(func(d *fakeDaemon) { d.psAuth = append(d.psAuth, req.Header.Get("Authorization")) })
		return d.ps()
	case "/api/embed", "/api/generate":
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		return d.generate(req.Context(), body)
	}
	return daemonReply(http.StatusNotFound, `{"error":"not found"}`), nil
}

// fakeVRAM is what the fake daemon reports each model holds.
const fakeVRAM = 512 * constants.BytesPerMB

func (d *fakeDaemon) ps() (*http.Response, error) {
	d.mu.Lock()
	if d.psErr {
		d.mu.Unlock()
		return daemonReply(http.StatusInternalServerError, `{"error":"ps failed"}`), nil
	}
	var models []map[string]any
	for name, expires := range d.loaded {
		if expires.IsZero() {
			expires = time.Now().Add(daemonForever)
		}
		models = append(models, map[string]any{"name": name, "model": name, "digest": "sha-" + name, "size_vram": fakeVRAM, "expires_at": expires.Format(time.RFC3339), "details": map[string]any{"format": "gguf", "family": "llama", "parameter_size": "8B", "quantization_level": "Q4_K_M"}})
	}
	for name := range d.stale {
		if _, listed := d.loaded[name]; !listed {
			models = append(models, map[string]any{"name": name, "model": name, "digest": "sha-" + name, "expires_at": time.Now().Add(time.Minute).Format(time.RFC3339)})
		}
	}
	hook := d.psHook
	d.mu.Unlock()
	if hook != nil {
		hook() // the snapshot above is what this answer reports
	}
	raw, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, err
	}
	return daemonReply(http.StatusOK, string(raw)), nil
}

func (d *fakeDaemon) generate(ctx context.Context, body map[string]any) (*http.Response, error) {
	model, _ := body["model"].(string)
	keepAlive, named := body["keep_alive"]
	call := daemonCall{model: model, body: body, keepAlive: keepAlive, unload: named && keepAlive == float64(0)}
	d.mu.Lock()
	d.calls = append(d.calls, call)
	if call.unload {
		delete(d.loaded, model)
		hold := d.unloadHold
		d.mu.Unlock()
		if hold != nil {
			<-hold
		}
		return daemonReply(http.StatusOK, `{"done":true}`), nil
	}
	hold, entered, delay, status := d.hold, d.entered, d.loadDelay, d.loadStatus
	d.mu.Unlock()
	if hold != nil {
		close(entered)
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if status != 0 {
		return daemonReply(status, `{"error":"model failed to load"}`), nil
	}
	expires := time.Time{} // a negative keep_alive never expires
	if seconds, _ := keepAlive.(float64); seconds >= 0 {
		expires = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	d.mu.Lock()
	if d.holdsOne {
		clear(d.loaded)
	}
	d.loaded[strings.ToLower(model)] = expires // Ollama lists names lower-cased
	d.mu.Unlock()
	return daemonReply(http.StatusOK, `{"done":true}`), nil
}

// holdLoads parks the next load until release.
func (d *fakeDaemon) holdLoads() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hold, d.entered = make(chan struct{}), make(chan struct{})
}

func (d *fakeDaemon) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hold != nil {
		close(d.hold)
		d.hold = nil
	}
}

func (d *fakeDaemon) waitEntered(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	entered := d.entered
	d.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(contractWait):
		t.Fatal("no load reached the daemon")
	}
}

func (d *fakeDaemon) set(fn func(d *fakeDaemon)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

// loadExternally is another client loading model with its own expiry; zero is forever.
func (d *fakeDaemon) loadExternally(model string, expires time.Time) {
	d.set(func(d *fakeDaemon) { d.loaded[model] = expires })
}

func (d *fakeDaemon) drop(model string) { d.set(func(d *fakeDaemon) { delete(d.loaded, model) }) }

func (d *fakeDaemon) generateCalls() []daemonCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *fakeDaemon) count(unload bool) int {
	n := 0
	for _, c := range d.generateCalls() {
		if c.unload == unload {
			n++
		}
	}
	return n
}

func (d *fakeDaemon) isLoaded(model string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.loaded[model]
	return ok
}

type servedRig struct {
	*contractRig
	daemon *fakeDaemon
	config atomic.Pointer[config.AppsConfig] // the config in use, as the node's store holds it
	edits  []func(*config.ServiceConfig)
}

// servedConfig builds ollama from scratch with every edit so far: a new config shares nothing with one in use.
func (r *servedRig) servedConfig(t *testing.T) *config.AppsConfig {
	t.Helper()
	svc := config.ServiceConfig{
		Enabled: new(true), Name: "ollama", Protocol: config.ProtocolOllama, Mode: "external",
		Runtime:      &config.AppRuntimeConfig{Endpoint: servedBase, KeepAlive: "5m"},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}
	for _, edit := range r.edits {
		edit(&svc)
	}
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("ollama", svc))
	return cfg
}

// edit changes the provider as production does: a new config, reloaded.
func (r *servedRig) edit(t *testing.T, fn func(*config.ServiceConfig)) {
	t.Helper()
	r.edits = append(r.edits, fn)
	r.config.Store(r.servedConfig(t))
	r.m.ReloadConfig(r.config.Load())
}

// newServedRig builds a served provider, ollama, over a fake daemon.
func newServedRig(t *testing.T, opts ...Option) *servedRig {
	t.Helper()
	return newServedRigTimed(t, 0, opts...)
}

// newServedRigTimed is newServedRig with a client timeout on the manager's HTTP client.
func newServedRigTimed(t *testing.T, clientTimeout time.Duration, opts ...Option) *servedRig {
	t.Helper()
	isolateProviderRoot(t)
	r := &servedRig{contractRig: &contractRig{}, daemon: newFakeDaemon()}
	r.config.Store(r.servedConfig(t))
	resolve := WithBackendResolver(func(name string) (*backend.Resolved, bool) {
		svc, ok := r.config.Load().LookupApp(name)
		if !ok {
			return nil, false
		}
		return backend.Target(&svc)
	})
	observe := WithStartObserver(func(ctx context.Context, _ EnsureRequest) (context.Context, func(EnsureResult, error)) {
		r.attempts.Add(1)
		return ctx, func(EnsureResult, error) {}
	})
	m, err := NewProviderAppManager(r.config.Load(), append([]Option{WithHTTPClient(&http.Client{Transport: r.daemon, Timeout: clientTimeout}), resolve, observe}, opts...)...)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	m.SetProviderVersion("ollama", "test") // a reload would otherwise probe a real daemon for it
	r.m = m
	return r
}

// serviceControlled makes ollama a managed service its OS supervisor controls.
func (r *servedRig) serviceControlled() {
	r.m.installs.Dispatcher().Register("ollama", &testDaemonInstaller{binary: "ollama", managed: true})
	r.m.serviceState("ollama").manager = &testOSService{name: "systemd", running: true}
}

func (r *servedRig) running(t *testing.T, req EnsureRequest) *instance.Instance {
	t.Helper()
	o := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
	require.NoError(t, o.err)
	require.NotNil(t, o.result.Run, "a served start registers a run")
	awaitRunning(t, o.result.Run)
	return o.result.Run
}

func (r *servedRig) tick(t *testing.T) {
	t.Helper()
	r.m.reconcileServedRuns(t.Context())
}

// runLog is what run's log file holds.
func runLog(t *testing.T, run *instance.Instance) string {
	t.Helper()
	require.NotEmpty(t, run.LogFilePath, "every run has a log")
	body, err := os.ReadFile(run.LogFilePath)
	require.NoError(t, err)
	return string(body)
}

func awaitLeft(t *testing.T, run *instance.Instance) {
	t.Helper()
	select {
	case <-run.LeftStarting():
	case <-time.After(contractWait):
		t.Fatal("run never left starting")
	}
}

func modelSourceBarrier(b *barrier, gate *atomic.Bool) Option {
	return WithModelSource(func(context.Context, string) string {
		if gate == nil || gate.Load() {
			b.hold()
		}
		return ""
	})
}

func TestEnsureRunContractServed(t *testing.T) {
	served := EnsureRequest{Provider: "ollama", Model: servedModel}

	t.Run("S1 concurrent callers share one load", func(t *testing.T) {
		b := newBarrier()
		r := newServedRig(t, modelSourceBarrier(b, nil))
		ctxs, reqs := same(t.Context(), served)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		results := collect(t, outs)
		assert.Equal(t, 1, requireSharedRun(t, results))
		assert.Equal(t, int32(1), r.attempts.Load())
		awaitRunning(t, results[0].result.Run)
		assert.Equal(t, 1, r.daemon.count(false), "one load reaches the daemon")
		assert.Len(t, r.m.instances.List(), 1)
		assert.Equal(t, servedLoaded, results[0].result.Run.Model)
	})

	t.Run("S2 a failed load reaches every holder, then a fresh start", func(t *testing.T) {
		for _, failure := range []string{"daemon 500", "unreachable"} {
			t.Run(failure, func(t *testing.T) {
				b := newBarrier()
				r := newServedRig(t, modelSourceBarrier(b, nil))
				failureHeld := newBarrier()
				r.daemon.set(func(d *fakeDaemon) {
					d.beforeRequest = func(req *http.Request) {
						if req.URL.Path == "/api/embed" {
							failureHeld.hold()
						}
					}
					d.loadStatus = http.StatusInternalServerError
					d.unreachable = failure == "unreachable"
				})
				ctxs, reqs := same(t.Context(), served)
				outs := r.leaderAndJoiners(t, b, ctxs, reqs)
				b.open()
				results := collect(t, outs)
				run := results[0].result.Run
				require.NotNil(t, run)
				for _, o := range results {
					require.NoError(t, o.err)
					assert.Same(t, run, o.result.Run)
				}
				failureHeld.reached(t)
				failureHeld.open()
				awaitLeft(t, run)
				require.Error(t, run.FailureCause(), "every holder reads the load's cause")
				assert.Equal(t, instance.StatusFailed, run.GetStatus())
				assert.Empty(t, r.m.instances.List(), "a failed load leaves no run")
				assert.Equal(t, int32(1), r.attempts.Load())
				r.daemon.set(func(d *fakeDaemon) { d.loadStatus, d.unreachable = 0, false })
				late := r.running(t, served)
				assert.NotEqual(t, run.ID, late.ID)
				assert.Equal(t, int32(2), r.attempts.Load())
			})
		}
	})

	t.Run("S3 cancelled callers never cancel the load", func(t *testing.T) {
		b := newBarrier()
		r := newServedRig(t, modelSourceBarrier(b, nil))
		r.daemon.holdLoads()
		ctxs, reqs := same(t.Context(), served)
		leaderCtx, cancelLeader := context.WithCancel(t.Context())
		ctxs[0] = leaderCtx
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		cancelLeader()
		b.open()
		results := collect(t, outs)
		require.ErrorIs(t, results[0].err, context.Canceled)
		run := results[1].result.Run
		require.NotNil(t, run)
		for _, o := range results[1:] {
			require.NoError(t, o.err)
			assert.Same(t, run, o.result.Run)
		}
		r.daemon.waitEntered(t)
		lateCtx, cancelLate := context.WithCancel(t.Context())
		late := collect(t, []<-chan outcome{r.ensure(lateCtx, served)})[0]
		cancelLate()
		require.NoError(t, late.err)
		assert.Same(t, run, late.result.Run, "a caller arriving during the load reuses its run")
		r.daemon.release()
		awaitRunning(t, run)
		assert.Equal(t, 1, r.daemon.count(false))
	})

	t.Run("S4 stop while a start is held", func(t *testing.T) {
		t.Run("before registration", func(t *testing.T) {
			b := newBarrier()
			r := newServedRig(t, modelSourceBarrier(b, nil))
			ctxs, reqs := same(t.Context(), served)
			outs := r.leaderAndJoiners(t, b, ctxs, reqs)
			stopped := make(chan error, 1)
			go func() { stopped <- r.m.Stop(context.Background()) }()
			waitParkedOn(t, "ProviderAppManager).Stop(", 1, contractWait)
			b.open()
			for _, o := range collect(t, outs) {
				if o.err != nil {
					assert.ErrorIs(t, o.err, ErrShutdown)
				} else if o.result.Run != nil {
					awaitLeft(t, o.result.Run)
				}
			}
			require.NoError(t, <-stopped)
			assert.Empty(t, r.m.instances.List())
			assert.Zero(t, r.daemon.count(true), "the daemon outlives the node: nothing is unloaded")
		})
		t.Run("during the load", func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.holdLoads()
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
			require.NoError(t, o.err)
			run := o.result.Run
			require.NotNil(t, run)
			r.daemon.waitEntered(t)
			require.NoError(t, r.m.Stop(context.Background()))
			awaitLeft(t, run)
			require.ErrorIs(t, run.FailureCause(), ErrShutdown)
			assert.Empty(t, r.m.instances.List())
			assert.Zero(t, r.daemon.count(true))
		})
	})

	t.Run("S5 concurrent force unloads once and loads once", func(t *testing.T) {
		b := newBarrier()
		var gate atomic.Bool
		r := newServedRig(t, modelSourceBarrier(b, &gate))
		old := r.running(t, served)
		gate.Store(true)
		force := served
		force.Force = true
		ctxs, reqs := same(t.Context(), force)
		outs := r.leaderAndJoiners(t, b, ctxs, reqs)
		b.open()
		results := collect(t, outs)
		requireSharedRun(t, results)
		replacement := results[0].result.Run
		assert.NotEqual(t, old.ID, replacement.ID)
		awaitRunning(t, replacement)
		assert.Equal(t, instance.StatusStopped, old.GetStatus())
		calls := r.daemon.generateCalls()
		require.Len(t, calls, 3, "load, unload, load")
		assert.False(t, calls[0].unload)
		assert.True(t, calls[1].unload)
		assert.False(t, calls[2].unload)
		assert.Equal(t, int32(2), r.attempts.Load())
		assert.Len(t, r.m.instances.List(), 1)
	})

	t.Run("S6 an explicit stop unloads once", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
		calls := r.daemon.generateCalls()
		require.Len(t, calls, 2)
		assert.True(t, calls[1].unload)
		assert.Contains(t, runLog(t, run), "stopped (user)")
		assert.Equal(t, servedLoaded, calls[1].model)
		assert.Equal(t, float64(0), calls[1].keepAlive)
		assert.Empty(t, r.m.instances.List())
		require.ErrorIs(t, r.m.StopInstance(t.Context(), run.ID), ErrInstanceNotFound)
		assert.Equal(t, 1, r.daemon.count(true))
	})

	t.Run("S7 reconcile", func(t *testing.T) {
		t.Run("a dropped model retires its run", func(t *testing.T) {
			r := newServedRig(t)
			run := r.running(t, served)
			ctx, cancel := context.WithTimeout(t.Context(), contractWait)
			defer cancel()
			require.NoError(t, run.WaitForGoroutines(ctx))
			r.daemon.drop(servedLoaded)
			r.tick(t)
			require.ErrorIs(t, run.FailureCause(), ErrModelDropped)
			assert.Equal(t, instance.StatusFailed, run.GetStatus())
			assert.Empty(t, r.m.instances.List())
			assert.Zero(t, r.daemon.count(true))
			assert.Regexp(t, `(?s)load requested, keep-alive 5m0s\n.* loaded\n.* dropped by the daemon: .*a request to the model starts it again\n$`, runLog(t, run))
		})
		t.Run("an unknown model is adopted", func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.loadExternally("x:latest", time.Now().Add(time.Hour))
			r.tick(t)
			assert.Empty(t, r.m.instances.List(), "one sighting adopts nothing")
			r.tick(t)
			adopted, ok := r.m.instances.GetByKey(instance.NewKey("", "x:latest", ""))
			require.True(t, ok)
			assert.Equal(t, instance.StartedBy("external"), adopted.StartedBy)
			assert.Equal(t, instance.StatusRunning, adopted.GetStatus())
			assert.Negative(t, adopted.GetKeepAlive(), "zzRouter never expires what another client loaded")
			assert.Zero(t, adopted.Port)
			assert.Contains(t, runLog(t, adopted), "adopted (loaded by another client)")
		})
		t.Run("a starting run is untouched", func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.holdLoads()
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
			require.NoError(t, o.err)
			r.daemon.waitEntered(t)
			r.tick(t)
			r.tick(t)
			assert.Equal(t, instance.StatusStarting, o.result.Run.GetStatus())
			assert.Len(t, r.m.instances.List(), 1)
			r.daemon.release()
			awaitRunning(t, o.result.Run)
		})
	})

	t.Run("S41 a load the daemon makes room for reports the run it displaced", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.set(func(d *fakeDaemon) { d.holdsOne = true })
		first := r.running(t, served)
		second := r.running(t, EnsureRequest{Provider: "ollama", Model: "b"})
		assert.Equal(t, []instance.EvictedRun{{RunID: first.ID, Model: first.Model, Provider: "ollama", FreedMiB: 512}}, second.EvictedRuns())
		require.ErrorIs(t, first.FailureCause(), ErrModelDropped)
		assert.Contains(t, runLog(t, first), "displaced by run "+second.ID)
		assert.Equal(t, []*instance.Instance{second}, r.m.instances.List())
		assert.Zero(t, r.daemon.count(true), "the daemon unloaded it; zzRouter sends nothing")
	})

	t.Run("S42 a run that became ready during the load is displaced too", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.set(func(d *fakeDaemon) { d.holdsOne = true })
		r.daemon.holdLoads()
		pending := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "ollama", Model: "b"})})[0]
		require.NoError(t, pending.err)
		r.daemon.waitEntered(t)
		var parked chan struct{}
		r.daemon.set(func(d *fakeDaemon) { parked, d.hold = d.hold, nil })
		first := r.running(t, served)
		close(parked)
		awaitRunning(t, pending.result.Run)
		assert.Equal(t, first.ID, pending.result.Run.EvictedRuns()[0].RunID)
		require.ErrorIs(t, first.FailureCause(), ErrModelDropped)
	})

	t.Run("S43 a model dropped while another loads is the load's to report", func(t *testing.T) {
		r := newServedRig(t)
		first := r.running(t, served)
		r.daemon.holdLoads()
		pending := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "ollama", Model: "b"})})[0]
		require.NoError(t, pending.err)
		r.daemon.waitEntered(t)
		r.daemon.drop(servedLoaded)
		r.tick(t)
		assert.Equal(t, instance.StatusRunning, first.GetStatus(), "the daemon may be making room for the load")
		r.daemon.release()
		awaitRunning(t, pending.result.Run)
		require.Len(t, pending.result.Run.EvictedRuns(), 1)
		assert.Equal(t, first.ID, pending.result.Run.EvictedRuns()[0].RunID)
		assert.Contains(t, runLog(t, first), "displaced by run "+pending.result.Run.ID)
	})

	t.Run("S8 a provider service restart retires its runs", func(t *testing.T) {
		r := newServedRig(t)
		r.serviceControlled()
		run := r.running(t, served)
		_, err := r.m.ControlProviderService(t.Context(), "ollama", "restart")
		require.NoError(t, err)
		require.ErrorIs(t, run.FailureCause(), ErrModelDropped)
		assert.Empty(t, r.m.instances.List())
		assert.Zero(t, r.daemon.count(true), "the daemon went away; nothing is unloaded")
		next := r.running(t, served)
		assert.NotEqual(t, run.ID, next.ID)
		assert.Equal(t, 2, r.daemon.count(false))
	})

	t.Run("S9 runs sharing a daemon never share a port", func(t *testing.T) {
		r := newServedRig(t)
		a := r.running(t, EnsureRequest{Provider: "ollama", Model: "a"})
		other := r.running(t, EnsureRequest{Provider: "ollama", Model: "b"})
		for _, run := range []*instance.Instance{a, other} {
			assert.Zero(t, run.Port)
			assert.Empty(t, run.HealthURL)
		}
		_, mapped := r.m.instances.GetByPort(11434)
		assert.False(t, mapped)
		for _, stats := range r.m.ports.GetPoolStats() {
			assert.Zero(t, stats.AllocatedPorts, "no pool port for a served run")
		}
		require.NoError(t, r.m.StopInstance(t.Context(), a.ID))
		kept, ok := r.m.instances.GetByKey(instance.NewKey("", "b:latest", ""))
		require.True(t, ok)
		assert.Same(t, other, kept)
		assert.Equal(t, instance.StatusRunning, kept.GetStatus())
		assert.True(t, r.daemon.isLoaded("b:latest"))
		assert.False(t, r.daemon.isLoaded("a:latest"))
	})

	t.Run("S11 a load carries the run's keep-alive as a number", func(t *testing.T) {
		r := newServedRig(t)
		pinned := served
		pinned.Parameters = map[string]string{"keep-alive": "-1"}
		run := r.running(t, pinned)
		plain := r.running(t, EnsureRequest{Provider: "ollama", Model: "plain"})
		calls := r.daemon.generateCalls()
		require.Len(t, calls, 2)
		assert.Equal(t, float64(-1), calls[0].keepAlive, "a number: the string \"-1\" is a 400")
		assert.Equal(t, float64(300), calls[1].keepAlive, "the provider's keep-alive, in seconds")
		for _, call := range calls {
			assert.ElementsMatch(t, []string{"model", "input", "keep_alive"}, slices.Collect(maps.Keys(call.body)), "no parameter reaches the daemon")
		}
		for _, served := range []*instance.Instance{run, plain} {
			assert.Negative(t, served.GetKeepAlive(), "the daemon expires a served model, not zzRouter")
		}
		assert.Equal(t, "-1", run.Resolved().Parameters["keep-alive"])
		assert.Equal(t, "5m0s", plain.Resolved().Parameters["keep-alive"])
	})

	t.Run("S12 the daemon expires an idle model and the reconcile retires its run", func(t *testing.T) {
		r := newServedRig(t)
		r.m.Start(t.Context())
		short := served
		short.Parameters = map[string]string{"keep-alive": "1s"}
		run := r.running(t, short)
		assert.Equal(t, float64(1), r.daemon.generateCalls()[0].keepAlive)
		lease, err := run.Acquire(t.Context(), nil) // a request without a hint re-arms nothing
		require.NoError(t, err)
		lease.Release()
		time.Sleep(time.Second + settle)
		assert.Len(t, r.m.instances.List(), 1, "zzRouter arms no timer of its own")
		r.daemon.drop(servedLoaded) // the daemon's own expiry
		r.tick(t)
		require.ErrorIs(t, run.FailureCause(), ErrModelDropped)
		assert.Empty(t, r.m.instances.List())
		assert.Zero(t, r.daemon.count(true), "zzRouter sends no unload")
	})

	t.Run("S13 a model a process provider holds refuses a served start", func(t *testing.T) {
		r := newServedRig(t)
		held := instance.NewInstance("held", "generic", servedLoaded, 0, 0, 0)
		held.MarkRunning(false, nil)
		require.NoError(t, r.m.instances.Register(held))
		_, err := r.m.EnsureRun(t.Context(), served)
		require.ErrorIs(t, err, ErrRunHeldByOtherProvider)
		assert.Zero(t, r.daemon.count(false))
	})

	t.Run("S14 a stop during the load unloads after it", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.holdLoads()
		o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
		require.NoError(t, o.err)
		r.daemon.waitEntered(t)
		stopped := make(chan error, 1)
		go func() { stopped <- r.m.StopInstance(context.Background(), o.result.Run.ID) }()
		waitParkedOn(t, ").StopInstance(", 1, contractWait)
		assert.Zero(t, r.daemon.count(true), "no unload races the load")
		r.daemon.release()
		require.NoError(t, <-stopped)
		calls := r.daemon.generateCalls()
		require.Len(t, calls, 2)
		assert.False(t, calls[0].unload)
		assert.True(t, calls[1].unload)
		assert.False(t, r.daemon.isLoaded(servedLoaded))
		r.tick(t)
		r.tick(t)
		assert.Empty(t, r.m.instances.List())
	})

	t.Run("S15 a zero keep-alive hint unloads at release", func(t *testing.T) {
		r := newServedRig(t)
		r.m.Start(t.Context())
		run := r.running(t, served)
		zero := time.Duration(0)
		lease, err := run.Acquire(t.Context(), &keepalive.Override{Duration: &zero})
		require.NoError(t, err)
		time.Sleep(settle)
		assert.Len(t, r.m.instances.List(), 1, "a held run is not expired")
		lease.Release()
		require.Eventually(t, func() bool { return len(r.m.instances.List()) == 0 }, contractWait, 10*time.Millisecond)
		time.Sleep(settle)
		assert.Equal(t, 1, r.daemon.count(true))
	})

	t.Run("S16 reconcile races", func(t *testing.T) {
		t.Run("a load completing during a tick keeps its run", func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.holdLoads()
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
			require.NoError(t, o.err)
			run := o.result.Run
			r.daemon.waitEntered(t)
			r.daemon.set(func(d *fakeDaemon) {
				d.psHook = func() { // once: the landing load's own listing must not wait on itself
					d.set(func(d *fakeDaemon) { d.psHook = nil })
					d.release()
					<-run.LeftStarting()
				}
			})
			r.tick(t)
			assert.Equal(t, instance.StatusRunning, run.GetStatus())
			assert.Len(t, r.m.instances.List(), 1)
		})
		t.Run("a model listed right after its stop is not adopted", func(t *testing.T) {
			r := newServedRig(t)
			run := r.running(t, served)
			r.daemon.set(func(d *fakeDaemon) { d.stale[servedLoaded] = true })
			require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
			r.tick(t)
			assert.Empty(t, r.m.instances.List())
			r.daemon.set(func(d *fakeDaemon) { delete(d.stale, servedLoaded) })
			r.tick(t)
			assert.Empty(t, r.m.instances.List())
		})
	})

	t.Run("S17 adoption racing a start leaves one run", func(t *testing.T) {
		t.Run("start first", func(t *testing.T) {
			b := newBarrier()
			var gate atomic.Bool
			r := newServedRig(t, modelSourceBarrier(b, &gate))
			r.daemon.loadExternally(servedLoaded, time.Now().Add(time.Hour))
			r.tick(t)
			gate.Store(true)
			start := r.ensure(t.Context(), served)
			b.reached(t)
			r.tick(t)
			b.open()
			o := collect(t, []<-chan outcome{start})[0]
			require.NoError(t, o.err)
			assert.True(t, o.result.Started)
			assert.Len(t, r.m.instances.List(), 1)
		})
		t.Run("adoption first", func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.loadExternally(servedLoaded, time.Now().Add(time.Hour))
			r.tick(t)
			r.tick(t)
			adopted, ok := r.m.instances.GetByKey(instance.NewKey("", servedLoaded, ""))
			require.True(t, ok)
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
			require.NoError(t, o.err)
			assert.False(t, o.result.Started)
			assert.Same(t, adopted, o.result.Run)
			assert.Len(t, r.m.instances.List(), 1)
		})
	})

	t.Run("S18 an untagged name is one run under the daemon's name", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		assert.Equal(t, servedLoaded, run.Model)
		assert.Equal(t, servedLoaded, run.WireModel)
		for range 3 {
			r.tick(t)
		}
		assert.Equal(t, instance.StatusRunning, run.GetStatus())
		listed, ok := r.m.instances.GetByKey(instance.NewKey("", servedLoaded, ""))
		require.True(t, ok)
		assert.Same(t, run, listed)
		again := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "ollama", Model: servedLoaded})})[0]
		require.NoError(t, again.err)
		assert.Same(t, run, again.result.Run)
	})

	t.Run("S19 a pin from before a node restart is adopted with no zzRouter expiry", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.loadExternally("p:latest", time.Time{})
		r.tick(t)
		r.tick(t)
		adopted, ok := r.m.instances.GetByKey(instance.NewKey("", "p:latest", ""))
		require.True(t, ok)
		assert.Negative(t, adopted.GetKeepAlive())
		assert.Greater(t, time.Until(adopted.Loaded().ExpiresAt), daemonForever/2, "the daemon's own expiry is recorded")
		assert.Equal(t, instance.StartedBy("external"), adopted.StartedBy)
	})

	t.Run("S20 a failed listing changes nothing", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		r.daemon.loadExternally("x:latest", time.Now().Add(time.Hour))
		r.daemon.set(func(d *fakeDaemon) { d.psErr = true })
		for range 3 {
			r.tick(t)
		}
		assert.Equal(t, instance.StatusRunning, run.GetStatus())
		assert.Len(t, r.m.instances.List(), 1)
		r.daemon.set(func(d *fakeDaemon) { d.psErr = false })
		r.tick(t)
		assert.Len(t, r.m.instances.List(), 1, "failed listings were no sightings")
	})

	t.Run("S21 an install-driven restart retires running runs only", func(t *testing.T) {
		r := newServedRig(t)
		r.serviceControlled()
		running := r.running(t, served)
		r.daemon.holdLoads()
		o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "ollama", Model: "b"})})[0]
		require.NoError(t, o.err)
		r.daemon.waitEntered(t)
		require.NoError(t, r.m.controlInstallService(t.Context(), "ollama", install.ServiceRestart))
		require.ErrorIs(t, running.FailureCause(), ErrModelDropped)
		assert.Equal(t, instance.StatusStarting, o.result.Run.GetStatus(), "a starting run's outcome is its load's")
		r.daemon.release()
		awaitRunning(t, o.result.Run)
		assert.Len(t, r.m.instances.List(), 1)
	})

	t.Run("S23 a load outlasts the client timeout within its readiness budget", func(t *testing.T) {
		for _, tc := range []struct {
			budget string
			ready  bool
		}{{"", true}, {"20ms", false}} {
			t.Run("budget="+tc.budget, func(t *testing.T) {
				const clientTimeout, loadTime = 20 * time.Millisecond, 200 * time.Millisecond
				r := newServedRigTimed(t, clientTimeout)
				r.daemon.set(func(d *fakeDaemon) { d.loadDelay = loadTime })
				if tc.budget != "" {
					r.edit(t, func(svc *config.ServiceConfig) {
						svc.Runtime.HealthCheck.ReadinessProbe = &config.ProbeConfig{Timeout: tc.budget}
					})
				}
				o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
				require.NoError(t, o.err)
				awaitLeft(t, o.result.Run)
				if tc.ready {
					assert.Equal(t, instance.StatusRunning, o.result.Run.GetStatus(), o.result.Run.GetErrorMessage())
				} else {
					require.ErrorIs(t, o.result.Run.FailureCause(), context.DeadlineExceeded)
				}
			})
		}
	})

	t.Run("S24 parameters status answers what a reload would change", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		pinned := r.running(t, EnsureRequest{Provider: "ollama", Model: "p", Parameters: map[string]string{"keep-alive": "-1"}})
		assert.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), run).State)
		r.edit(t, func(svc *config.ServiceConfig) {
			svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"num-ctx": "4096"}}
		})
		assert.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), run).State, "a key the load does not send changes nothing")
		r.edit(t, func(svc *config.ServiceConfig) {
			svc.Runtime.KeepAlive = "10m"
		})
		stale := r.m.ParametersStatus(t.Context(), run)
		assert.Equal(t, instance.ParametersStale, stale.State)
		assert.Contains(t, stale.Changed, "parameters.keep-alive")
		assert.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), pinned).State, "a run's own keep-alive holds against config")
	})

	t.Run("S25 restarting an adopted run unloads and loads once", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.loadExternally("x:latest", time.Now().Add(time.Hour))
		r.tick(t)
		r.tick(t)
		adopted, ok := r.m.instances.GetByKey(instance.NewKey("", "x:latest", ""))
		require.True(t, ok)
		next, err := r.m.RestartInstance(t.Context(), adopted.ID)
		require.NoError(t, err)
		awaitRunning(t, next)
		calls := r.daemon.generateCalls()
		require.Len(t, calls, 2)
		assert.True(t, calls[0].unload)
		assert.False(t, calls[1].unload)
		assert.Equal(t, instance.StartedByAPI, next.StartedBy)
		assert.Equal(t, float64(300), calls[1].keepAlive, "the restart loads with the provider's keep-alive")
		assert.Equal(t, "ollama", next.SnapshotConfig().Provider)
	})

	t.Run("S26 a failed unload is retried until the model is free", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		r.daemon.set(func(d *fakeDaemon) { d.unreachable = true })
		require.Error(t, r.m.StopInstance(t.Context(), run.ID))
		assert.True(t, run.StopUnconfirmed.Load())
		_, err := r.m.EnsureRun(t.Context(), served)
		require.ErrorIs(t, err, instance.ErrStopping)
		r.daemon.set(func(d *fakeDaemon) { d.unreachable = false })
		r.tick(t) // the daemon still holds the model: the stop is retried
		assert.Empty(t, r.m.instances.List())
		assert.False(t, r.daemon.isLoaded(servedLoaded), "a pinned model is unloaded, not left for the daemon")
		next := r.running(t, served)
		assert.NotEqual(t, run.ID, next.ID)
	})
}

func TestEnsureRunContractServedEnds(t *testing.T) {
	served := EnsureRequest{Provider: "ollama", Model: servedModel}

	t.Run("S27 a stop in progress is never ended by the reconcile", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		hold := make(chan struct{})
		r.daemon.set(func(d *fakeDaemon) { d.unloadHold = hold })
		stopped := make(chan error, 1)
		go func() { stopped <- r.m.StopInstance(context.Background(), run.ID) }()
		require.Eventually(t, func() bool { return !r.daemon.isLoaded(servedLoaded) }, contractWait, time.Millisecond)
		r.tick(t)
		_, listed := r.m.instances.Get(run.ID)
		assert.True(t, listed, "the run stays registered until its own unload answers")
		_, err := r.m.EnsureRun(t.Context(), served)
		require.ErrorIs(t, err, instance.ErrStopping, "no load can land before the unload")
		close(hold)
		require.NoError(t, <-stopped)
		assert.Empty(t, r.m.instances.List())
	})

	t.Run("S28 a run of a provider config no longer enables still ends", func(t *testing.T) {
		t.Run("a stop in progress when it is disabled", func(t *testing.T) {
			r := newServedRig(t)
			r.daemon.holdLoads()
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
			require.NoError(t, o.err)
			r.daemon.waitEntered(t)
			stopped := make(chan error, 1)
			go func() { stopped <- r.m.StopInstance(context.Background(), o.result.Run.ID) }()
			waitParkedOn(t, ").StopInstance(", 1, contractWait)
			r.edit(t, func(svc *config.ServiceConfig) { svc.Enabled = new(false) }) // the stop holds the run; the reload leaves it
			r.daemon.release()
			require.NoError(t, <-stopped)
			assert.Equal(t, 1, r.daemon.count(true), "the disabled provider's daemon is still told")
			assert.False(t, r.daemon.isLoaded(servedLoaded))
			assert.Empty(t, r.m.instances.List())
		})
		t.Run("retired by the reload", func(t *testing.T) {
			r := newServedRig(t)
			run := r.running(t, served)
			r.edit(t, func(svc *config.ServiceConfig) { svc.Enabled = new(false) })
			require.ErrorIs(t, run.FailureCause(), ErrModelDropped)
			assert.Empty(t, r.m.instances.List())
			assert.Zero(t, r.daemon.count(true), "the provider no longer exists here: nothing is asked of it")
		})
	})

	t.Run("S29 keep-alive: explicit reaches the daemon, unset is its own, zero is zzRouter's", func(t *testing.T) {
		r := newServedRig(t)
		r.m.Start(t.Context())
		r.edit(t, func(svc *config.ServiceConfig) { svc.Runtime.KeepAlive = "" })
		unset := r.running(t, served)
		assert.NotContains(t, r.daemon.generateCalls()[0].body, "keep_alive", "the daemon's OLLAMA_KEEP_ALIVE applies")
		assert.NotContains(t, unset.Resolved().Parameters, "keep-alive")
		zero := r.running(t, EnsureRequest{Provider: "ollama", Model: "z", Parameters: map[string]string{"keep-alive": "0"}})
		assert.NotContains(t, r.daemon.generateCalls()[1].body, "keep_alive", "a zero never reaches the daemon")
		assert.True(t, r.daemon.isLoaded("z:latest"))
		assert.Zero(t, zero.GetKeepAlive())
		lease, err := zero.Acquire(t.Context(), nil)
		require.NoError(t, err)
		time.Sleep(settle)
		assert.True(t, r.daemon.isLoaded("z:latest"), "a held run is not ended")
		lease.Release()
		require.Eventually(t, func() bool {
			_, listed := r.m.instances.Get(zero.ID)
			return !r.daemon.isLoaded("z:latest") && !listed
		}, contractWait, 10*time.Millisecond)
		_, listed := r.m.instances.Get(zero.ID)
		assert.False(t, listed)
	})

	t.Run("S30 an unload asked of a model with no run starts nothing", func(t *testing.T) {
		r := newServedRig(t)
		unload := served
		unload.Unload, unload.Origin = true, OriginRequest
		result, err := r.m.EnsureRun(t.Context(), unload)
		require.NoError(t, err)
		assert.Nil(t, result.Run)
		assert.Zero(t, r.daemon.count(false), "nothing is loaded to be unloaded")
		assert.Empty(t, r.m.instances.List())
		run := r.running(t, served)
		result, err = r.m.EnsureRun(t.Context(), unload)
		require.NoError(t, err)
		assert.Same(t, run, result.Run, "a run that exists serves the request; its hint ends it at release")
	})

	t.Run("S31 a served model is one run whatever the endpoint and name", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel, Endpoint: EndpointEmbeddings})
		for _, endpoint := range []Endpoint{EndpointChat, EndpointEmbeddings} {
			found, ok := r.m.GetInstanceByModel("ollama", servedModel, endpoint)
			require.True(t, ok)
			assert.Same(t, run, found)
		}
		found, ok := r.m.GetInstanceByModel("", servedModel, EndpointChat)
		require.True(t, ok, "a caller naming no provider finds the run under the daemon's name")
		assert.Same(t, run, found)
		assert.Equal(t, servedModel, r.m.RequestModel(run, servedModel), "the daemon resolves the untagged name")
		assert.Equal(t, servedLoaded, r.m.RequestModel(run, "alias"), "another name is sent as the daemon's")
		held := instance.NewInstance("held", "generic", "x:latest", 0, 0, 0)
		held.Endpoint = string(EndpointEmbeddings)
		held.MarkRunning(false, nil)
		require.NoError(t, r.m.instances.Register(held))
		r.daemon.loadExternally("x:latest", time.Now().Add(time.Hour))
		r.tick(t)
		r.tick(t)
		assert.Len(t, r.m.instances.List(), 2, "a model any endpoint's run holds is never adopted")
	})

	t.Run("S32 a deliberate daemon restart is a stop, not a crash, and listings keep the daemon's details", func(t *testing.T) {
		r := newServedRig(t)
		r.serviceControlled()
		run := r.running(t, served)
		r.tick(t)
		loaded := run.Loaded()
		assert.Equal(t, "sha-"+servedLoaded, loaded.Digest)
		assert.Equal(t, instance.Loaded{Size: 0, VRAM: fakeVRAM, Digest: "sha-" + servedLoaded, Format: "gguf", Family: "llama", ParameterSize: "8B", QuantLevel: "Q4_K_M", ExpiresAt: loaded.ExpiresAt}, loaded)
		for len(r.m.auditEvents) > 0 {
			<-r.m.auditEvents
		}
		_, err := r.m.ControlProviderService(t.Context(), "ollama", "restart")
		require.NoError(t, err)
		var reasons []obsruns.StopReason
		for len(r.m.auditEvents) > 0 {
			if e := <-r.m.auditEvents; e.Type == EventInstanceStopped && e.Instance == run.ID {
				reasons = append(reasons, e.Reason)
			}
		}
		assert.Equal(t, []obsruns.StopReason{obsruns.StopReasonUser}, reasons)
	})

	t.Run("S33 a served start names its model", func(t *testing.T) {
		r := newServedRig(t)
		_, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "ollama"})
		require.ErrorIs(t, err, ErrParameterValidation)
		assert.Zero(t, r.daemon.count(false))
		assert.Empty(t, r.m.instances.List())
	})

	t.Run("S34 the reconcile's listing carries the provider's credential, edits included", func(t *testing.T) {
		r := newServedRig(t)
		r.edit(t, func(svc *config.ServiceConfig) {
			svc.Runtime.API = &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "first-token"}
		})
		r.tick(t)
		r.edit(t, func(svc *config.ServiceConfig) {
			svc.Runtime.API = &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "second-token"}
		})
		r.tick(t)
		r.daemon.mu.Lock()
		defer r.daemon.mu.Unlock()
		assert.Equal(t, []string{"Bearer first-token", "Bearer second-token"}, r.daemon.psAuth)
	})
}

func TestEnsureRunContractServedRaces(t *testing.T) {
	served := EnsureRequest{Provider: "ollama", Model: servedModel}

	t.Run("S35 a user's retried stop and the reconcile's retry end a run once", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		r.daemon.set(func(d *fakeDaemon) { d.unreachable = true })
		require.Error(t, r.m.StopInstance(t.Context(), run.ID))
		hold := make(chan struct{})
		stopped := make(chan error, 1)
		r.daemon.set(func(d *fakeDaemon) {
			d.unreachable, d.unloadHold = false, hold
			d.psHook = func() { // the listing is taken; the user retries the stop now
				d.psHook = nil
				go func() { stopped <- r.m.StopInstance(context.Background(), run.ID) }()
				for r.daemon.count(true) == 0 { // the user's unload has reached the daemon

					time.Sleep(time.Millisecond)
				}
			}
		})
		ticked := make(chan struct{})
		go func() { r.tick(t); close(ticked) }()
		time.Sleep(settle)
		close(hold)
		<-ticked
		require.NoError(t, <-stopped)
		assert.Equal(t, 1, r.daemon.count(true), "one unload reaches the daemon")
		assert.Empty(t, r.m.instances.List())
	})

	t.Run("S36 a run retired while it loads never runs, and its load is undone", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.holdLoads()
		o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
		require.NoError(t, o.err)
		r.daemon.waitEntered(t)
		r.edit(t, func(svc *config.ServiceConfig) { svc.Enabled = new(false) })
		require.ErrorIs(t, o.result.Run.FailureCause(), ErrModelDropped)
		for len(r.m.auditEvents) > 0 {
			<-r.m.auditEvents
		}
		r.daemon.release()
		require.Eventually(t, func() bool { return r.daemon.count(true) == 1 }, contractWait, time.Millisecond)
		require.NoError(t, o.result.Run.WaitForGoroutines(t.Context()))
		assert.False(t, r.daemon.isLoaded(servedLoaded))
		assert.Equal(t, instance.StatusFailed, o.result.Run.GetStatus())
		for len(r.m.auditEvents) > 0 {
			assert.NotEqual(t, EventInstanceRunning, (<-r.m.auditEvents).Type)
		}
	})

	t.Run("S37 a model the daemon does not have is not local", func(t *testing.T) {
		r := newServedRig(t)
		r.daemon.set(func(d *fakeDaemon) { d.loadStatus = http.StatusNotFound })
		o := collect(t, []<-chan outcome{r.ensure(t.Context(), served)})[0]
		require.NoError(t, o.err)
		awaitLeft(t, o.result.Run)
		require.ErrorIs(t, o.result.Run.FailureCause(), ErrModelNotLocal)
	})

	t.Run("S38 nothing is reconciled while the node stops", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, served)
		r.daemon.drop(servedLoaded)
		r.m.stopping.Store(true)
		r.tick(t)
		r.m.stopping.Store(false) // let cleanup stop the node
		assert.Equal(t, instance.StatusRunning, run.GetStatus())
	})

	t.Run("S39 a served provider is reached through its config alone", func(t *testing.T) {
		r := newServedRig(t, WithBackendResolver(func(string) (*backend.Resolved, bool) { return nil, false }))
		run := r.running(t, served)
		base, _, err := r.m.Upstream(run)
		require.NoError(t, err)
		assert.Equal(t, servedBase, base)
	})

	t.Run("S40 a daemon name is matched as the daemon matches it", func(t *testing.T) {
		r := newServedRig(t)
		run := r.running(t, EnsureRequest{Provider: "ollama", Model: "library/M"})
		assert.Equal(t, "M:latest", run.Model)
		assert.True(t, r.daemon.isLoaded("m:latest"))
		for range 3 {
			r.tick(t)
		}
		assert.Equal(t, instance.StatusRunning, run.GetStatus())
		assert.Len(t, r.m.instances.List(), 1, "the lower-cased listing is the same model, not one to adopt")
	})
}

// TestEnsureRunContractIdle pins the reaper contract independently of its scheduling mechanism.
func TestEnsureRunContractIdle(t *testing.T) {
	for _, limit := range []string{"0", "25ms", "-1"} {
		t.Run(limit, func(t *testing.T) {
			r := newContractRig(t, false)
			r.m.Start(t.Context())
			o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "idle", Parameters: map[string]string{"keep-alive": limit}})})[0]
			require.NoError(t, o.err)
			run := o.result.Run
			lease, err := run.Acquire(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(lease.Release)
			awaitRunning(t, run)
			time.Sleep(75 * time.Millisecond)
			require.Equal(t, instance.StatusRunning, run.GetStatus(), "a held lease never expires")
			lease.Release()
			if limit == "-1" {
				time.Sleep(75 * time.Millisecond)
				assert.Equal(t, instance.StatusRunning, run.GetStatus(), "a pin never expires idle")
			} else {
				eventually(t, func() bool { return !r.m.instances.Has(run.ID) }, "idle run must stop after its final lease")
			}
		})
	}
	t.Run("tree and request precedence", func(t *testing.T) {
		r := newContractRig(t, false, WithNodename(func() string { return "worker" }))
		fresh := &config.AppsConfig{}
		r.m.AppsConfig().RangeApps(func(name string, svc config.ServiceConfig) bool {
			svc.Defaults = &config.AppDefaultsConfig{Parameters: map[string]string{"budget": "0.55", "keep-alive": "2m"}}
			svc.Models = map[string]config.ModelSpec{"idle": {Parameters: map[string]string{"keep-alive": "3m"}}}
			svc.Nodes = map[string]config.NodeSpec{"worker": {Parameters: map[string]string{"keep-alive": "4m"}, Models: map[string]config.NodeModelSpec{"idle": {Parameters: map[string]string{"keep-alive": "-1"}}}}}
			require.NoError(t, fresh.AddApp(name, svc))
			return true
		})
		r.m.ReloadConfig(fresh)
		o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "idle"})})[0]
		require.NoError(t, o.err)
		awaitRunning(t, o.result.Run)
		assert.Negative(t, o.result.Run.GetKeepAlive(), "node x model must replace provider/model/node limits")
		other := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "override", Parameters: map[string]string{"keep-alive": "10m"}})})[0]
		require.NoError(t, other.err)
		assert.Equal(t, 10*time.Minute, other.result.Run.GetKeepAlive())
	})
}

func TestEnsureRunContractIdleBurst(t *testing.T) {
	r := newContractRig(t, false)
	svc, ok := r.m.AppsConfig().LookupApp("generic")
	require.True(t, ok)
	for n := range 90 {
		run, err := r.m.newRun(EnsureRequest{Provider: "generic", Model: fmt.Sprint(n), Parameters: map[string]string{"keep-alive": "0"}}, svc, 0)
		require.NoError(t, err)
		require.NoError(t, r.m.instances.Register(run))
		run.MarkRunning(false, nil)
		lease, err := run.Acquire(t.Context(), nil)
		require.NoError(t, err)
		lease.Release()
	}
	// Expiry notifications before Start must retain all registry state, even beyond the old buffer.
	time.Sleep(75 * time.Millisecond)
	r.m.Start(t.Context())
	eventually(t, func() bool { return len(r.m.instances.List()) == 0 }, "every released zero run must stop")
}

func TestEnsureRunContractIdleRefresh(t *testing.T) {
	r := newContractRig(t, false)
	r.m.Start(t.Context())
	o := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "refresh", Parameters: map[string]string{"keep-alive": "25ms"}})})[0]
	require.NoError(t, o.err)
	lease, err := o.result.Run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(lease.Release)
	awaitRunning(t, o.result.Run)
	pin := -time.Second
	o.result.Run.StampActivityWithHints(&keepalive.Override{Duration: &pin})
	lease.Release()
	time.Sleep(75 * time.Millisecond)
	assert.Equal(t, instance.StatusRunning, o.result.Run.GetStatus(), "an obsolete deadline cannot expire a newer pin")
	zero := time.Duration(0)
	lease, err = o.result.Run.Acquire(t.Context(), &keepalive.Override{Duration: &zero})
	require.NoError(t, err)
	time.Sleep(25 * time.Millisecond)
	require.Equal(t, instance.StatusRunning, o.result.Run.GetStatus())
	lease.Release()
	eventually(t, func() bool { return !r.m.instances.Has(o.result.Run.ID) }, "updated zero ends at release")
}

func TestEnsureRunContractQueuedSettingsRefresh(t *testing.T) {
	r := newContractRig(t, true, WithMemoryBudget(func(string) (*schema.MemoryBudget, error) {
		return &schema.MemoryBudget{Kind: "fraction_total", Parameter: "budget"}, nil
	}))
	r.m.memorySnapshot = func(context.Context) ([]gpu.MemoryDevice, error) {
		return []gpu.MemoryDevice{{UUID: "GPU-A", TotalMiB: 10000, FreeMiB: 6000}}, nil
	}
	edit := func(limit string) {
		fresh := &config.AppsConfig{}
		r.m.AppsConfig().RangeApps(func(name string, svc config.ServiceConfig) bool {
			svc.Models = map[string]config.ModelSpec{"two": {Parameters: map[string]string{config.KeepAliveParameter: limit}}}
			require.NoError(t, fresh.AddApp(name, svc))
			return true
		})
		r.m.ReloadConfig(fresh)
	}
	edit("0")
	first := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "one"})})[0]
	require.NoError(t, first.err)
	second := collect(t, []<-chan outcome{r.ensure(t.Context(), EnsureRequest{Provider: "generic", Model: "two"})})[0]
	require.NoError(t, second.err)
	require.Equal(t, first.result.Run.ID, second.result.Run.WaitingOn())
	edit("-1")
	r.ready(t)
	awaitRunning(t, first.result.Run)
	awaitRunning(t, second.result.Run)
	assert.Negative(t, second.result.Run.GetKeepAlive(), "admitted idle policy must match the re-resolved launch")
	assert.Equal(t, "-1", second.result.Run.Resolved().Parameters[config.KeepAliveParameter])
}

func TestNodeScopedEnsureConcurrentProcess(t *testing.T) {
	r := newContractRig(t, true)
	fresh := &config.AppsConfig{}
	for _, name := range []string{"generic", "other"} {
		svc, ok := r.m.appsConfig.LookupApp(name)
		require.True(t, ok)
		svc.RunScope, svc.Protocol, svc.Capabilities = config.RunScopeNode, "", nil
		require.NoError(t, fresh.AddApp(name, svc))
	}
	r.m.ReloadConfig(fresh)
	r.m.localCatalog = func(context.Context, string) (bool, error) {
		t.Error("node run queried the model catalog")
		return false, nil
	}
	start := make(chan struct{})
	outs := make([]<-chan outcome, contractCallers)
	for i := range outs {
		out := make(chan outcome, 1)
		outs[i] = out
		go func() {
			<-start
			result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
			out <- outcome{result, err}
		}()
	}
	close(start)
	results := collect(t, outs)
	var starts int
	for _, out := range results {
		require.NoError(t, out.err)
		require.Same(t, results[0].result.Run, out.result.Run)
		if out.result.Started {
			starts++
		}
	}
	require.Equal(t, 1, starts)
	require.Equal(t, int32(1), r.attempts.Load())
	r.ready(t)
	run := results[0].result.Run
	awaitRunning(t, run)
	require.Positive(t, run.GetProcessID())
	require.Empty(t, run.Model)
	before := run.GetLastActivity()
	reused, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
	require.NoError(t, err)
	require.Same(t, run, reused.Run)
	require.True(t, run.GetLastActivity().After(before))
	other, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "other"})
	require.NoError(t, err)
	require.NotEqual(t, run.ID, other.Run.ID)
	_, err = r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Model: "unwanted"})
	require.ErrorIs(t, err, ErrParameterValidation)
	_, err = r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic", Endpoint: EndpointEmbeddings})
	require.ErrorIs(t, err, ErrParameterValidation)
	restarted, err := r.m.RestartInstance(t.Context(), run.ID)
	require.NoError(t, err)
	require.NotEqual(t, run.ID, restarted.ID)
	awaitRunning(t, restarted)
	require.Equal(t, instance.ParametersCurrent, r.m.ParametersStatus(t.Context(), restarted).State)
}

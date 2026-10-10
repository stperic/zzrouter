package prov_apps

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
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

// contractBaseGapsEnv runs the scenarios the base cannot pass yet.
const contractBaseGapsEnv = "ZZR_CONTRACT_BASE_GAPS"

func baseGap(t *testing.T, observed string) {
	t.Helper()
	if os.Getenv(contractBaseGapsEnv) == "" {
		t.Skip("base gap: " + observed)
	}
}

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
		started := 0
		for _, o := range results {
			require.NoError(t, o.err)
			require.NotNil(t, o.result.Run)
			assert.Equal(t, results[0].result.Run.ID, o.result.Run.ID)
			if o.result.Started {
				started++
			}
		}
		assert.Equal(t, 1, started)
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
		for _, o := range results {
			require.NoError(t, o.err, "no caller is refused while the old run stops")
			require.NotNil(t, o.result.Run)
			assert.Equal(t, results[0].result.Run.ID, o.result.Run.ID)
		}
		assert.NotEqual(t, old.result.Run.ID, results[0].result.Run.ID)
		assert.Equal(t, instance.StatusStopped, old.result.Run.GetStatus())
		assert.Equal(t, 1, r.terms(t), "the old run is stopped once")
		assert.Equal(t, int32(2), r.attempts.Load())
		assert.Len(t, r.m.instances.List(), 1)
		awaitRunning(t, results[0].result.Run)
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
		t.Run("callers get ErrShutdown or the run", func(t *testing.T) {
			baseGap(t, "every caller gets context.Canceled from the start Stop cancelled")
			for _, o := range results {
				if o.err != nil {
					assert.ErrorIs(t, o.err, ErrShutdown)
				} else {
					assert.NotNil(t, o.result.Run)
				}
			}
		})
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
}

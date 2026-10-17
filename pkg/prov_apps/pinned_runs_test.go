package prov_apps

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pinnedPath() string { return filepath.Join(config.Paths().GetDataDir(), "pinned_runs.json") }

func readPins(t *testing.T) []EnsureRequest {
	t.Helper()
	data, err := os.ReadFile(pinnedPath())
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var state pinnedRuns
	require.NoError(t, json.Unmarshal(data, &state))
	require.Equal(t, 1, state.Version)
	return state.Runs
}

func seedPins(t *testing.T, reqs ...EnsureRequest) {
	t.Helper()
	data, err := json.Marshal(&pinnedRuns{Version: 1, Runs: reqs})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pinnedPath(), data, 0o600))
}

func startPin(t *testing.T, m *ProviderAppManager, req EnsureRequest) *instance.Instance {
	t.Helper()
	result, err := m.EnsureRun(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, result.Run)
	awaitRunning(t, result.Run)
	return result.Run
}

func pinRequest(model string) EnsureRequest {
	return EnsureRequest{Provider: "generic", Model: model, Parameters: map[string]string{"keep-alive": "-1"}}
}

func TestPinnedRestoreShutdownAndRequestReplay(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		svc.Capabilities.WireEndpoints = append(svc.Capabilities.WireEndpoints, "embeddings")
		svc.Defaults.Environment = map[string]string{"TIER_ONLY": "before"}
		return nil
	}))
	req := pinRequest("remembered")
	req.Endpoint = EndpointEmbeddings
	req.Parameters["budget"] = "0.37"
	req.EnvVars = map[string]string{"PIN_TEST_TOKEN": "request-token"}
	run := startPin(t, r.m, req)
	require.True(t, run.IdleSnapshot().Pinned)
	require.Equal(t, []EnsureRequest{req}, readPins(t))
	info, err := os.Stat(pinnedPath())
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	raw, err := os.ReadFile(pinnedPath())
	require.NoError(t, err)
	var file map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &file))
	var fields []map[string]any
	require.NoError(t, json.Unmarshal(file["runs"], &fields))
	assert.Len(t, fields[0], 5, "empty runtime is omitted; only the request tier persists")
	assert.NotContains(t, string(raw), "TIER_ONLY")
	assert.NotContains(t, string(raw), r.engine)
	require.NoError(t, r.m.Stop(t.Context()))
	require.Equal(t, []EnsureRequest{req}, readPins(t), "node shutdown preserves pins")
	require.NoError(t, r.m.appsConfig.UpdateApp("generic", func(svc *config.ServiceConfig) error {
		svc.Defaults.Environment["TIER_ONLY"] = "after"
		return nil
	}))
	reg, err := jobs.NewRegistry(jobs.Config{NodeName: "restore-test", Clock: clock.System()})
	require.NoError(t, err)
	t.Cleanup(reg.Stop)
	attempts := make(chan EnsureRequest, 2)
	m, err := NewProviderAppManager(r.m.appsConfig, WithJobsRegistry(reg), declaredSchema(t, "parameters:\n  budget: {}\n"), WithStartObserver(func(ctx context.Context, req EnsureRequest) (context.Context, func(EnsureResult, error)) {
		attempts <- req
		return ctx, func(EnsureResult, error) {}
	}))
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	m.Start(t.Context())
	m.RestorePinnedRuns()
	m.onDemandStarts.Wait()
	restored, exists := m.instances.GetByKey(run.Key())
	require.True(t, exists)
	awaitRunning(t, restored)
	require.True(t, restored.IdleSnapshot().Pinned)
	assert.Equal(t, req.Parameters, restored.SnapshotConfig().Parameters)
	assert.Equal(t, req.EnvVars, restored.SnapshotConfig().EnvVars)
	assert.Equal(t, "after", restored.Resolved().Environment["TIER_ONLY"], "restore resolves current tiers")
	select {
	case observed := <-attempts:
		assert.Equal(t, OriginRestore, observed.Origin)
		assert.False(t, observed.Force)
		assert.False(t, observed.Detached)
		assert.Equal(t, req.Endpoint, observed.Endpoint)
		assert.Equal(t, req.Runtime, observed.Runtime)
	default:
		t.Fatal("restore bypassed EnsureRun's cold-start observer")
	}
	assert.NotEmpty(t, restored.StreamJobID)
	assert.Len(t, reg.List(jobs.KindRun), 1)
	job, err := reg.Get(restored.StreamJobID)
	require.NoError(t, err)
	assert.Equal(t, OriginRestore, job.Meta["origin"])
	require.NoError(t, m.StopInstance(t.Context(), restored.ID))
	assert.Empty(t, readPins(t), "operator stop removes a pin, including shared-endpoint keys")
}

func TestPinnedRestoreUnpinnedAndForce(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	r := newContractRig(t, false)
	unpinned := startPin(t, r.m, EnsureRequest{Provider: "generic", Model: "ordinary"})
	assert.Empty(t, readPins(t))
	require.NoError(t, r.m.StopInstance(t.Context(), unpinned.ID))
	req := pinRequest("replace")
	startPin(t, r.m, req)
	require.Len(t, readPins(t), 1)
	req.Force = true
	req.Parameters = map[string]string{"keep-alive": "30m"}
	run := startPin(t, r.m, req)
	assert.False(t, run.IdleSnapshot().Pinned)
	assert.Empty(t, readPins(t))
}

func TestPinnedRestoreTierUnpins(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := EnsureRequest{Provider: "generic", Model: "tier-pin"}
	seedPins(t, req)
	r := newContractRig(t, false)
	r.m.Start(t.Context())
	r.m.RestorePinnedRuns()
	r.m.onDemandStarts.Wait()
	run, exists := r.m.instances.GetByKey(req.Key())
	require.True(t, exists)
	awaitRunning(t, run)
	assert.False(t, run.IdleSnapshot().Pinned)
	assert.Empty(t, readPins(t), "readiness drops a restored pin when current tiers no longer pin it")
}

func TestPinnedRestoreFailureContinues(t *testing.T) {
	for _, failure := range []string{"removed provider", "disabled provider", "missing model", "after registration"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			bad, good := pinRequest("bad"), pinRequest("good")
			if failure == "removed provider" || failure == "disabled provider" {
				bad.Provider = "other"
			}
			if failure == "removed provider" {
				bad.Provider = "removed"
			}
			seedPins(t, bad, good)
			attempts := make(chan string, 4)
			r := newContractRig(t, false, WithLocalCatalog(func(_ context.Context, model string) (bool, error) {
				attempts <- model
				return model != "bad" || failure != "missing model", nil
			}))
			switch failure {
			case "disabled provider":
				require.NoError(t, r.m.appsConfig.UpdateApp("other", func(svc *config.ServiceConfig) error { svc.Enabled = new(false); return nil }))
			case "after registration":
				bad.EnvVars = map[string]string{"LD_PRELOAD": "/refused"}
				seedPins(t, bad, good)
				r.m.loadPinnedRuns()
			}
			r.m.Start(t.Context())
			r.m.RestorePinnedRuns()
			r.m.onDemandStarts.Wait()
			run, exists := r.m.instances.GetByKey(good.Key())
			require.True(t, exists, "failed restore must not prevent the next entry")
			awaitRunning(t, run)
			require.Eventually(t, func() bool { return len(readPins(t)) == 1 }, contractWait, time.Millisecond)
			assert.Equal(t, "good", readPins(t)[0].Model)
			r.m.RestorePinnedRuns()
			r.m.onDemandStarts.Wait()
			wantAttempts := int32(1)
			if failure == "after registration" {
				wantAttempts++
			}
			assert.Equal(t, wantAttempts, r.attempts.Load(), "failed entries are attempted at most once")
		})
	}
}

// A pin refused for its parameters, an endpoint the provider stopped
// serving or a key its schema no longer declares, is a config an operator
// can fix: the record stays for the next restore and the refusal is
// logged at error, while the restore goes on to the next pin.
func TestPinnedRestoreKeepsAPinRefusedForItsParameters(t *testing.T) {
	for _, refusal := range []string{"endpoint", "parameter"} {
		t.Run(refusal, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			bad, good := pinRequest("bad"), pinRequest("good")
			if refusal == "endpoint" {
				bad.Endpoint = EndpointReranking
			} else {
				bad.Parameters["ghost-flag"] = "1"
			}
			seedPins(t, bad, good)
			log := capturePinLog(t)
			r := newContractRig(t, false)
			r.m.Start(t.Context())
			r.m.RestorePinnedRuns()
			r.m.onDemandStarts.Wait()
			run, exists := r.m.instances.GetByKey(good.Key())
			require.True(t, exists)
			awaitRunning(t, run)
			_, restored := r.m.instances.GetByKey(bad.Key())
			assert.False(t, restored)
			pins := readPins(t)
			require.Len(t, pins, 2)
			assert.Equal(t, bad, pins[0], "the refused pin keeps its record and its place")
			assert.Equal(t, "good", pins[1].Model)
			assert.True(t, log.containsAt(slog.LevelError, "Pinned run refused at restore; its pin is kept"))
		})
	}
}

func TestPinnedRestoreExcludedRuns(t *testing.T) {
	for _, kind := range []string{"served", "adopted", "disposable", "install"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			if kind == "served" || kind == "adopted" {
				r := newServedRig(t)
				r.edit(t, func(svc *config.ServiceConfig) { svc.Runtime.KeepAlive = "-1" })
				var run *instance.Instance
				if kind == "served" {
					run = r.running(t, EnsureRequest{Provider: "ollama", Model: servedModel})
				} else {
					r.daemon.loadExternally("adopted:latest", time.Time{})
					r.tick(t)
					r.tick(t)
					var exists bool
					run, exists = r.m.instances.GetByKey(instance.NewKey("", "adopted:latest", ""))
					require.True(t, exists)
				}
				require.True(t, run.IdleSnapshot().Pinned)
				assert.Empty(t, readPins(t))
				return
			}
			r := newContractRig(t, false)
			req := pinRequest(kind)
			var run *instance.Instance
			if kind == "disposable" {
				req.DisposablePlanID = "smoke-plan"
				svc, ok := r.m.appsConfig.LookupApp("generic")
				require.True(t, ok)
				var err error
				run, err = r.m.registerRun(t.Context(), req, launchSelection{key: "generic", config: svc})
				require.NoError(t, err)
				require.True(t, r.m.publishRunning(run, true, "ready"))
			} else {
				req.Origin, req.Runtime = OriginInstall, "generic"
				run = startPin(t, r.m, req)
			}
			require.True(t, run.IdleSnapshot().Pinned)
			assert.Empty(t, readPins(t))
		})
	}
}

type pinLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *pinLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *pinLog) Handle(_ context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record.Clone())
	return nil
}
func (l *pinLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *pinLog) WithGroup(string) slog.Handler      { return l }
func (l *pinLog) contains(message string) bool       { return l.containsAt(slog.LevelWarn, message) }
func (l *pinLog) containsAt(level slog.Level, message string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.Message == message && r.Level == level {
			return true
		}
	}
	return false
}

func capturePinLog(t *testing.T) *pinLog {
	t.Helper()
	l := &pinLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return l
}

func TestPinnedRestoreInvalidFile(t *testing.T) {
	for _, data := range []string{`{"version":1,"runs":[`, `{"version":99,"runs":[{"provider":"generic","model":"bad"}]}`, `{"runs":[{"provider":"generic","model":"bad"}]}`, `null`} {
		t.Run(data, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			require.NoError(t, os.WriteFile(pinnedPath(), []byte(data), 0o600))
			log := capturePinLog(t)
			r := newContractRig(t, false)
			r.m.Start(t.Context())
			r.m.RestorePinnedRuns()
			r.m.onDemandStarts.Wait()
			require.NoError(t, r.m.Stop(t.Context()))
			assert.Empty(t, r.m.instances.List())
			assert.Zero(t, r.attempts.Load())
			actual, err := os.ReadFile(pinnedPath() + ".corrupt")
			require.NoError(t, err)
			assert.Equal(t, data, string(actual), "invalid file is set aside as evidence")
			assert.NoFileExists(t, pinnedPath())
			assert.True(t, log.contains("Cannot load pinned runs"))
		})
	}
}

func TestPinnedRestoreStopDrains(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	seedPins(t, pinRequest("held"), pinRequest("never"))
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	r := newContractRig(t, false, WithLocalCatalog(func(ctx context.Context, _ string) (bool, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return false, ctx.Err()
	}))
	r.m.Start(t.Context())
	r.m.RestorePinnedRuns()
	select {
	case <-entered:
	case <-time.After(contractWait):
		t.Fatal("restore did not enter EnsureRun")
	}
	done := make(chan error, 1)
	go func() { done <- r.m.Stop(context.Background()) }()
	select {
	case <-cancelled:
	case <-time.After(contractWait):
		unblock()
		t.Fatal("Stop did not cancel restore")
	}
	select {
	case <-done:
		t.Fatal("Stop returned before the restore flight drained")
	default:
	}
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(contractWait):
		t.Fatal("Stop failed to drain restore")
	}
	r.m.onDemandStarts.Wait()
	assert.Empty(t, r.m.instances.List(), "nothing may register after the shutdown drain")
	assert.Len(t, readPins(t), 2, "shutdown cancellation preserves requests for the next boot")
	r.m.RestorePinnedRuns()
	r.m.onDemandStarts.Wait()
	assert.Empty(t, r.m.instances.List())
}

func TestPinnedRestoreOnceAndEarlyRun(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	req := pinRequest("early")
	seedPins(t, req)
	r := newContractRig(t, false)
	r.m.Start(t.Context())
	run := startPin(t, r.m, req)
	r.m.RestorePinnedRuns()
	r.m.onDemandStarts.Wait()
	assert.Equal(t, int32(1), r.attempts.Load(), "EnsureRun reuses the early run")
	require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
	r.m.pins.update(req, true)
	r.m.RestorePinnedRuns()
	r.m.onDemandStarts.Wait()
	assert.Empty(t, r.m.instances.List(), "restore is once per boot even when the file changes")
	require.NoError(t, r.m.Stop(t.Context()))
	r.m.RestorePinnedRuns()
	r.m.onDemandStarts.Wait()
	assert.Empty(t, r.m.instances.List(), "restore after Stop does nothing")
}

func TestPinnedRestoreTerminalPaths(t *testing.T) {
	for _, terminal := range []string{"operator", "process", "health", "readiness", "launch"} {
		t.Run(terminal, func(t *testing.T) {
			t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
			r := newContractRig(t, false)
			run := startPin(t, r.m, pinRequest("terminal"))
			require.Len(t, readPins(t), 1)
			switch terminal {
			case "operator":
				require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
			case "process":
				pid, startToken := run.ClaimTermination(true)
				require.NoError(t, process.Terminate(t.Context(), process.TerminateRequest{PID: pid, StartToken: startToken, ProcessGroupID: run.GetProcessGroupID(), Graceful: time.Second}))
				require.Eventually(t, func() bool { return run.GetStatus().IsTerminal() }, contractWait, time.Millisecond)
			case "health":
				mon := health.NewMonitor(r.m.instances, health.MonitorConfig{Interval: time.Millisecond, Timeout: time.Millisecond, MaxRetries: 1})
				ctx, cancel := context.WithTimeout(t.Context(), contractWait)
				defer cancel()
				mon.Run(ctx, run, health.CheckConfig{HTTPHealthPath: "/health"}, func(*instance.Instance) { t.Error("dead health endpoint cannot promote readiness") })
				require.Equal(t, instance.StatusFailed, run.GetStatus())
			case "readiness":
				r.m.failReadiness(run, health.ReadinessSignal{Failed: true, Pattern: "readiness failed"})
			case "launch":
				run.MarkFailedError(errors.New("launch failed"))
			}
			assert.Empty(t, readPins(t), "every terminal route must forget the pin")
		})
	}
}

func TestPinnedRestoreWriteFailureDoesNotFailLifecycle(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	log := capturePinLog(t)
	r := newContractRig(t, false)
	require.NoError(t, os.Mkdir(pinnedPath(), 0o700))
	run := startPin(t, r.m, pinRequest("write-failure"))
	require.True(t, run.IdleSnapshot().Pinned)
	require.True(t, log.contains("Cannot persist pinned run"))
	require.NoError(t, r.m.StopInstance(t.Context(), run.ID))
	assert.Empty(t, r.m.instances.List())
}

func TestPinnedRestoreOrder(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	seedPins(t, pinRequest("oldest"), pinRequest("next"), pinRequest("newest"))
	barrier := newBarrier()
	t.Cleanup(barrier.open)
	starts := make(chan string, 3)
	r := newContractRig(t, false, WithStartObserver(func(ctx context.Context, req EnsureRequest) (context.Context, func(EnsureResult, error)) {
		starts <- req.Model
		barrier.hold()
		return ctx, func(EnsureResult, error) {}
	}))
	r.m.Start(t.Context())
	r.m.RestorePinnedRuns()
	barrier.reached(t)
	assert.Equal(t, "oldest", <-starts)
	select {
	case name := <-starts:
		t.Fatalf("restore started %s concurrently with oldest", name)
	default:
	}
	barrier.open()
	r.m.onDemandStarts.Wait()
	assert.Equal(t, "next", <-starts)
	assert.Equal(t, "newest", <-starts)
	for _, run := range r.m.instances.List() {
		awaitRunning(t, run)
	}
}

func TestPinnedRequestSerialization(t *testing.T) {
	req := EnsureRequest{
		Provider: "generic", Model: "model", Endpoint: EndpointEmbeddings, Runtime: "runtime",
		Parameters: map[string]string{"keep-alive": "-1"}, EnvVars: map[string]string{"TOKEN": "secret"},
		Origin: OriginInstall, Detached: true, Force: true, Unload: true, DisposablePlanID: "disposable", Port: 9999,
		Files: []instance.FileMount{{NodePath: "/local/file"}}, NativeConfig: &instance.NativeConfig{Command: []string{"/local/executable"}},
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	assert.Len(t, fields, 6)
	var got EnsureRequest
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, EnsureRequest{Provider: req.Provider, Model: req.Model, Endpoint: req.Endpoint, Runtime: req.Runtime, Parameters: req.Parameters, EnvVars: req.EnvVars}, got)
}

func TestPinnedRestorePolicyMatchesAPI(t *testing.T) {
	for _, n := range []Node{
		{Lifecycle: LifecycleProcess},
		{Lifecycle: LifecycleProcess, Absent: true},
		{Lifecycle: LifecycleProcess, ShuttingDown: true},
		{Lifecycle: LifecycleProcess, Existing: &RunView{ID: "warm", Provider: "generic", Status: instance.StatusRunning}},
		{Lifecycle: LifecycleProcess, AdmissionHeld: true},
	} {
		w := Want{Provider: "generic", Key: pinRequest("model").Key(), Origin: OriginAPI}
		api := Decide(w, n)
		w.Origin = OriginRestore
		restored := Decide(w, n)
		assert.Equal(t, api.Act, restored.Act)
		assert.Equal(t, api.Run, restored.Run)
		assert.Equal(t, api.Replace, restored.Replace)
		assert.Equal(t, api.Err, restored.Err)
	}
}

func TestPinnedRecoveryKeepsPlaceWithoutRewrite(t *testing.T) {
	t.Setenv("ZZROUTER_TEST_HOME", t.TempDir())
	s := &pinnedRuns{path: pinnedPath(), Version: 1}
	first, second := pinRequest("first"), pinRequest("second")
	s.update(first, true)
	s.update(second, true)
	require.NoError(t, os.Remove(pinnedPath()))
	s.update(first, true) // a run back from unhealthy reports running again
	assert.NoFileExists(t, pinnedPath(), "an unchanged pin is not rewritten")
	changed := pinRequest("first")
	changed.Parameters["ctx-size"] = "4096"
	s.update(changed, true)
	got := readPins(t)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"first", "second"}, []string{got[0].Model, got[1].Model})
	assert.Equal(t, "4096", got[0].Parameters["ctx-size"])
}

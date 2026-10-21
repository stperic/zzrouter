package prov_apps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServedStopWaitsForLoadingModel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		readiness    string
		releaseAfter time.Duration
	}{
		{"default readiness", "", constants.EngineGracefulTimeout + constants.ShutdownRunTailTimeout + time.Second},
		{"configured readiness", "3m", defaultReadinessTimeout + constants.ShutdownRunEpilogueTimeout + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newServedRig(t)
				r.edit(t, func(svc *config.ServiceConfig) {
					svc.Runtime.HealthCheck.ReadinessProbe = &config.ProbeConfig{Timeout: tc.readiness}
				})
				r.daemon.holdLoads()
				t.Cleanup(r.daemon.release)
				req := EnsureRequest{Provider: "ollama", Model: servedModel}
				o := collect(t, []<-chan outcome{r.ensure(t.Context(), req)})[0]
				require.NoError(t, o.err)
				run := o.result.Run
				r.daemon.waitEntered(t)
				stopped := make(chan error, 1)
				go func() { stopped <- r.m.StopInstance(t.Context(), run.ID) }()
				synctest.Wait()
				require.Equal(t, instance.StatusStopping, run.GetStatus())
				time.Sleep(tc.releaseAfter)
				select {
				case err := <-stopped:
					t.Fatalf("stop abandoned the in-flight load: %v", err)
				default:
				}
				r.daemon.release()
				require.NoError(t, <-stopped)
				require.False(t, run.StopUnconfirmed.Load())
				require.False(t, r.m.instances.Has(run.ID))
				require.False(t, r.daemon.isLoaded(run.WireModel))
				next := r.running(t, req)
				require.NotEqual(t, run.ID, next.ID)
			})
		})
	}
}

func TestEnsureRunForceStopsAnotherProvidersRun(t *testing.T) {
	m, err := NewProviderAppManager(aliasTestConfig(t), WithLocalCatalog(func(context.Context, string) (bool, error) { return true, nil }))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
	held := instance.NewInstance("other-provider", "other", "model", 8210, 0, 0)
	held.MarkRunning(false, nil)
	require.NoError(t, m.instances.Register(held))
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Model: "model", Provider: "fake"})
	require.ErrorIs(t, err, ErrRunHeldByOtherProvider)
	result, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "model", Provider: "fake", Force: true})
	require.NoError(t, err)
	require.True(t, result.Started)
	require.NotNil(t, result.Run)
	assert.Equal(t, "fake", result.Run.Provider)
	assert.Equal(t, instance.StatusStopped, held.GetStatus())
	_, found := m.instances.Get(held.ID)
	assert.False(t, found)
}

func TestEnsureRunInstallSmokeBypassesLocalCatalog(t *testing.T) {
	checked := 0
	m := onDemandTestManager(t, nil)
	m.localCatalog = func(context.Context, string) (bool, error) { checked++; return false, nil }
	_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "not-cataloged", Provider: "fake"})
	require.ErrorIs(t, err, ErrModelNotLocal)
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Model: "not-cataloged", Provider: "fake", Runtime: "fake", Origin: OriginInstall})
	require.ErrorIs(t, err, errTestStart, "smoke must pass the catalog rule and reach command preparation")
	assert.Equal(t, 1, checked)
}

func TestEnsureRunCancelledJoinerDoesNotCancelStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		m := onDemandTestManager(t, nil)
		m.localCatalog = func(context.Context, string) (bool, error) { <-release; return true, nil }
		leader := make(chan error, 1)
		go func() {
			_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
			leader <- err
		}()
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		joiner := make(chan error, 1)
		go func() { _, err := m.EnsureRun(ctx, EnsureRequest{Model: "shared", Provider: "fake"}); joiner <- err }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-joiner, context.Canceled)
		close(release)
		require.ErrorIs(t, <-leader, errTestStart)
	})
}

func TestEnsureRunOneFlightAcrossProviders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var starts atomic.Int32
		m := onDemandTestManager(t, nil)
		m.localCatalog = func(context.Context, string) (bool, error) { starts.Add(1); <-release; return true, nil }
		leader := make(chan error, 1)
		go func() {
			_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
			leader <- err
		}()
		synctest.Wait()
		joiner := make(chan error, 1)
		go func() {
			_, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "other"})
			joiner <- err
		}()
		synctest.Wait()
		assert.Equal(t, int32(1), starts.Load())
		close(release)
		require.ErrorIs(t, <-leader, errTestStart)
		require.ErrorIs(t, <-joiner, errTestStart)
	})
}

func TestEnsureRunStartedOnlyForFlightLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		m, err := NewProviderAppManager(aliasTestConfig(t), WithLocalCatalog(func(context.Context, string) (bool, error) { <-release; return true, nil }))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
		results := make(chan EnsureResult, 2)
		var callers sync.WaitGroup
		callers.Go(func() {
			result, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
			assert.NoError(t, err)
			results <- result
		})
		synctest.Wait()
		callers.Go(func() {
			result, err := m.EnsureRun(t.Context(), EnsureRequest{Model: "shared", Provider: "fake"})
			assert.NoError(t, err)
			results <- result
		})
		synctest.Wait()
		close(release)
		callers.Wait()
		first, second := <-results, <-results
		require.NotNil(t, first.Run)
		require.Same(t, first.Run, second.Run)
		assert.NotEqual(t, first.Started, second.Started)
	})
}

func TestEnsureRunWarmReuseSkipsCatalogAndHeldInstallGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := onDemandTestManager(t, nil)
		m.localCatalog = func(context.Context, string) (bool, error) {
			t.Error("warm reuse queried catalog")
			return false, nil
		}
		run := instance.NewInstance("warm", "fake", "missing-from-catalog", 8200, 0, 0)
		run.MarkRunning(false, nil)
		require.NoError(t, m.instances.Register(run))
		unlock, err := m.installs.acquireProvider(t.Context(), "Fake")
		require.NoError(t, err)
		defer unlock()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			result, err := m.EnsureRun(ctx, EnsureRequest{Provider: "fake", Model: run.Model})
			assert.Same(t, run, result.Run)
			assert.False(t, result.Started)
			done <- err
		}()
		synctest.Wait()
		select {
		case err := <-done:
			require.NoError(t, err)
		default:
			cancel()
			t.Fatal("reuse waited on the install gate")
		}
	})
}

func TestEnsureRunCatalogEvidenceFailureDoesNotRefuse(t *testing.T) {
	m := onDemandTestManager(t, nil)
	m.localCatalog = func(context.Context, string) (bool, error) { return false, errors.New("snapshot unavailable") }
	_, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Model: "missing-evidence"})
	require.ErrorIs(t, err, errTestStart)
}

func TestEnsureRunLeaderCancellationLeavesJoinedStartAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		m, err := NewProviderAppManager(aliasTestConfig(t), WithLocalCatalog(func(ctx context.Context, _ string) (bool, error) {
			<-release
			require.NoError(t, ctx.Err())
			return true, nil
		}))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
		ctx, cancel := context.WithCancel(t.Context())
		leader := make(chan error, 1)
		go func() { _, err := m.EnsureRun(ctx, EnsureRequest{Provider: "fake", Model: "shared"}); leader <- err }()
		synctest.Wait()
		joined := make(chan EnsureResult, 1)
		go func() {
			result, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Model: "shared"})
			assert.NoError(t, err)
			joined <- result
		}()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-leader, context.Canceled)
		close(release)
		result := <-joined
		require.NotNil(t, result.Run)
		assert.False(t, result.Started)
	})
}

func TestEnsureRunDetachedAcceptsBeforePreparationAndStopDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, cancelled := make(chan struct{}), make(chan struct{})
		m := onDemandTestManager(t, func(ctx context.Context, _ string) string { <-ctx.Done(); close(cancelled); <-release; return "" })
		result, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Model: "detached", Detached: true})
		require.NoError(t, err)
		assert.Nil(t, result.Run)
		synctest.Wait()
		stopped := make(chan error, 1)
		go func() { stopped <- m.Stop(context.Background()) }()
		<-cancelled
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop did not wait for detached start")
		default:
		}
		close(release)
		require.NoError(t, <-stopped)
	})
}

func TestDecideSelectsIndexedLiveEndpoint(t *testing.T) {
	m := onDemandTestManager(t, nil)
	old := instance.NewInstance("leftover", "other", "model", 8210, 0, 0)
	old.Endpoint = "embeddings"
	old.MarkFailed("unconfirmed")
	old.StopUnconfirmed.Store(true)
	require.NoError(t, m.instances.Register(old))
	live := instance.NewInstance("live", "fake", "model", 8200, 0, 0)
	live.MarkRunning(false, nil)
	require.NoError(t, m.instances.Register(live))
	req := EnsureRequest{Provider: "fake", Model: "model", Endpoint: EndpointChat}
	decision, existing := m.decide(t.Context(), preparedStart{req: req})
	assert.Equal(t, ActReuse, decision.Act)
	assert.Same(t, live, existing)
	assert.Empty(t, decision.Replace)
}

func TestEnsureRunObserverRecordsColdAttemptsOnly(t *testing.T) {
	m := onDemandTestManager(t, nil)
	started, finished := 0, 0
	m.observeStart = func(ctx context.Context, req EnsureRequest) (context.Context, func(EnsureResult, error)) {
		started++
		assert.Equal(t, "cold", req.Model)
		return ctx, func(_ EnsureResult, err error) { finished++; assert.ErrorIs(t, err, errTestStart) }
	}
	_, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Model: "cold"})
	require.ErrorIs(t, err, errTestStart)
	warm := instance.NewInstance("warm", "fake", "warm", 8200, 0, 0)
	warm.MarkRunning(false, nil)
	require.NoError(t, m.instances.Register(warm))
	result, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Model: "warm"})
	require.NoError(t, err)
	assert.Same(t, warm, result.Run)
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Runtime: "fake", Model: "smoke", Origin: OriginInstall})
	require.ErrorIs(t, err, errTestStart)
	assert.Equal(t, 1, started)
	assert.Equal(t, 1, finished)
}

func TestEnsureRunSmokeTimeoutRetainsPreparationOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timedOut, release := make(chan struct{}), make(chan struct{})
		m, err := NewProviderAppManager(aliasTestConfig(t), WithModelSource(func(ctx context.Context, _ string) string {
			<-ctx.Done()
			close(timedOut)
			<-release
			return ""
		}))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := m.EnsureRun(ctx, EnsureRequest{Provider: "fake", Runtime: "fake", Model: "smoke", Origin: OriginInstall})
			done <- err
		}()
		<-timedOut
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("smoke returned before preparation relinquished ownership")
		default:
		}
		close(release)
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		assert.Empty(t, m.instances.List(), "timed-out smoke must leave no late run or process")
	})
}

func TestCanonicalRunLookupContract(t *testing.T) {
	calls := 0
	r := newServedRig(t, WithModelCanonicalizer(func(model string) string {
		calls++
		if model == "alias" {
			return servedModel
		}
		return model
	}))
	run := instance.NewInstance("warm", "ollama", servedLoaded, 0, -1, 1)
	run.Endpoint = "chat"
	run.MarkRunning(false, nil)
	require.NoError(t, r.m.instances.Register(run))
	got, found := r.m.GetInstanceByCanonicalModel("ollama", servedModel, EndpointChat)
	require.True(t, found)
	assert.Same(t, run, got)
	assert.Zero(t, calls, "canonical inference lookup never invokes replica/alias resolution")
	_, found = r.m.GetInstanceByCanonicalModel("other", servedLoaded, EndpointChat)
	assert.False(t, found, "another provider cannot borrow this run")
	got, found = r.m.GetInstanceByModel("ollama", "alias", EndpointChat)
	require.True(t, found)
	assert.Same(t, run, got)
	assert.Equal(t, 1, calls, "management alias adapter resolves once")
}

func TestCanonicalRunLookupContractProviderAliases(t *testing.T) {
	cfg := aliasTestConfig(t)
	svc, ok := cfg.LookupApp("fake")
	require.True(t, ok)
	runtime := *svc.Runtime
	runtime.PortRange, runtime.BasePort = []int{8210, 8215}, 8210
	svc.Runtime = &runtime
	require.NoError(t, cfg.AddApp("llamacpp", svc))
	m, err := NewProviderAppManager(cfg)
	require.NoError(t, err)
	cleanupProviderManager(t, m)
	run := instance.NewInstance("display-provider", "llama.cpp", "m", 0, -1, 1)
	run.Endpoint = "chat"
	run.MarkRunning(false, nil)
	require.NoError(t, m.instances.Register(run))
	for _, provider := range []string{"llama.cpp", "llamacpp"} {
		got, found := m.GetInstanceByCanonicalModel(provider, "m", EndpointChat)
		require.True(t, found, provider)
		assert.Same(t, run, got)
		got, found = m.GetInstanceByModel(provider, "m", EndpointChat)
		require.True(t, found, provider)
		assert.Same(t, run, got)
	}
	_, found := m.GetInstanceByCanonicalModel("fake", "m", EndpointChat)
	assert.False(t, found, "different configured keys retain provider authority")
}

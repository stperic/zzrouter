package prov_apps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/singleflight"
)

func TestEnsureRunForceStopsAnotherProvidersRun(t *testing.T) {
	m, err := NewProviderAppManager(aliasTestConfig(t), WithLocalCatalog(func(context.Context, string) (bool, error) { return true, nil }))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, m.Stop(context.Background())) })
	held := instance.NewInstance("other-provider", "other", "model", 8210, 0, 0)
	held.MarkRunning()
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
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Model: "not-cataloged", Provider: "fake", Runtime: "fake", installSmoke: true})
	require.ErrorIs(t, err, errTestStart, "smoke must pass the catalog rule and reach command preparation")
	assert.Equal(t, 1, checked)
}

func TestJoinStartUsesActualRunAndErrorPrecedence(t *testing.T) {
	m := onDemandTestManager(t, nil)
	run := instance.NewInstance("held", "fake", "model", 8200, 0, 0)
	run.MarkRunning()
	require.NoError(t, m.instances.Register(run))
	for _, tc := range []struct {
		name   string
		req    EnsureRequest
		flight singleflight.Result
		err    error
	}{
		{"matching reuse", EnsureRequest{Provider: "fake"}, singleflight.Result{Val: EnsureResult{Run: run}}, nil},
		{"provider mismatch", EnsureRequest{Provider: "other"}, singleflight.Result{Val: EnsureResult{Run: run}}, ErrRunHeldByOtherProvider},
		{"failed flight precedes provider mismatch", EnsureRequest{Provider: "other"}, singleflight.Result{Err: errTestStart}, errTestStart},
		{"runtime mismatch", EnsureRequest{Provider: "fake", Runtime: "other"}, singleflight.Result{Val: EnsureResult{Run: run}}, ErrInstancesRunning},
		{"plan mismatch", EnsureRequest{Provider: "fake", DisposablePlanID: "other"}, singleflight.Result{Val: EnsureResult{Run: run}}, ErrInstancesRunning},
		{"force cannot join reuse", EnsureRequest{Provider: "fake", Force: true}, singleflight.Result{Val: EnsureResult{Run: run}}, ErrInstancesRunning},
		{"force joins fresh start", EnsureRequest{Provider: "fake", Force: true}, singleflight.Result{Val: EnsureResult{Run: run, Started: true}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Model, tc.req.Endpoint = "model", EndpointChat
			_, err := m.joinStart(tc.req, tc.flight, false)
			require.ErrorIs(t, err, tc.err)
		})
	}
	run.Config.Runtime = "changed"
	_, err := m.joinStart(EnsureRequest{Provider: "fake", Model: "model", Endpoint: EndpointChat}, singleflight.Result{Val: EnsureResult{Run: run}}, false)
	require.ErrorIs(t, err, ErrInstancesRunning, "join policy must read the current snapshot")
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
		run.MarkRunning()
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

func TestStartSnapshotSelectsIndexedLiveEndpoint(t *testing.T) {
	m := onDemandTestManager(t, nil)
	old := instance.NewInstance("leftover", "other", "model", 8210, 0, 0)
	old.Endpoint = "embeddings"
	old.MarkFailed("unconfirmed")
	old.StopUnconfirmed.Store(true)
	require.NoError(t, m.instances.Register(old))
	live := instance.NewInstance("live", "fake", "model", 8200, 0, 0)
	live.MarkRunning()
	require.NoError(t, m.instances.Register(live))
	req := EnsureRequest{Provider: "fake", Model: "model", Endpoint: EndpointChat}
	snapshot := m.startSnapshot(t.Context(), preparedStart{req: req})
	require.NotNil(t, snapshot.run)
	decision := decideStart(req, snapshot)
	assert.Same(t, live, decision.reuse)
	assert.Nil(t, decision.stop)
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
	warm.MarkRunning()
	require.NoError(t, m.instances.Register(warm))
	result, err := m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Model: "warm"})
	require.NoError(t, err)
	assert.Same(t, warm, result.Run)
	_, err = m.EnsureRun(t.Context(), EnsureRequest{Provider: "fake", Runtime: "fake", Model: "smoke", installSmoke: true})
	require.ErrorIs(t, err, errTestStart)
	assert.Equal(t, 1, started)
	assert.Equal(t, 1, finished)
}

func TestJoinStartRetainsIdentityAfterNativeRunRemoval(t *testing.T) {
	m := onDemandTestManager(t, nil)
	run := instance.NewInstance("removed", "fake", "model", 8200, 0, 0)
	run.MarkRunning()
	require.NoError(t, m.instances.Register(run))
	require.NoError(t, m.instances.Remove(run.ID))
	flight := singleflight.Result{Val: EnsureResult{Run: run, Started: true}}
	for _, tc := range []struct {
		req EnsureRequest
		err error
	}{
		{EnsureRequest{Provider: "fake"}, nil},
		{EnsureRequest{Provider: "other"}, ErrRunHeldByOtherProvider},
		{EnsureRequest{Provider: "fake", Runtime: "other"}, ErrInstancesRunning},
		{EnsureRequest{Provider: "fake", DisposablePlanID: "other"}, ErrInstancesRunning},
	} {
		tc.req.Model, tc.req.Endpoint = "model", EndpointChat
		_, err := m.joinStart(tc.req, flight, false)
		assert.ErrorIs(t, err, tc.err)
	}
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
			_, err := m.EnsureRun(ctx, EnsureRequest{Provider: "fake", Runtime: "fake", Model: "smoke", installSmoke: true})
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

func TestJoinStartFailedLeaderReturnsItsCause(t *testing.T) {
	m := onDemandTestManager(t, nil)
	run := instance.NewInstance("failed-leader", "fake", "model", 8200, 0, 0)
	run.MarkFailedError(errTestStart)
	flight := singleflight.Result{Val: EnsureResult{Run: run, Started: true}}
	result, err := m.joinStart(EnsureRequest{Provider: "other", Model: "model", Endpoint: EndpointChat}, flight, false)
	require.ErrorIs(t, err, errTestStart, "the leader's failure precedes a joiner's provider mismatch")
	assert.Nil(t, result.Run)
	assert.False(t, result.Started)
}

func TestJoinStartRefusesMissingOrTerminalOutcome(t *testing.T) {
	m := onDemandTestManager(t, nil)
	for _, state := range []string{"nil", "served", "stopped", "failed without typed cause"} {
		t.Run(state, func(t *testing.T) {
			outcome := EnsureResult{Started: true, Served: state == "served"}
			if state == "stopped" || state == "failed without typed cause" {
				outcome.Run = instance.NewInstance("terminal", "fake", "model", 8200, 0, 0)
				if state == "stopped" {
					outcome.Run.SetStatus(instance.StatusStopped)
				} else {
					outcome.Run.MarkFailed("launch failed")
				}
			}
			result, err := m.joinStart(EnsureRequest{Provider: "fake", Model: "model", Endpoint: EndpointChat}, singleflight.Result{Val: outcome}, false)
			require.Error(t, err)
			assert.Nil(t, result.Run)
			assert.False(t, result.Served)
		})
	}
}

func TestJoinStartUsesOutcomeDespiteDifferentIndexedRun(t *testing.T) {
	m := onDemandTestManager(t, nil)
	returned := instance.NewInstance("returned", "fake", "model", 8200, 0, 0)
	returned.MarkRunning()
	indexed := instance.NewInstance("indexed", "other", "model", 8210, 0, 0)
	indexed.MarkRunning()
	require.NoError(t, m.instances.Register(indexed))
	result, err := m.joinStart(EnsureRequest{Provider: "fake", Model: "model", Endpoint: EndpointChat}, singleflight.Result{Val: EnsureResult{Run: returned, Started: true}}, false)
	require.NoError(t, err)
	assert.Same(t, returned, result.Run)
}

package prov_apps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

type activityObserver interface{ ObserveActivity(bool) }

func observeFixture(t *testing.T, run *instance.Instance) activityObserver {
	t.Helper()
	observer, ok := any(run).(activityObserver)
	require.True(t, ok, "instance must accept observed engine activity")
	return observer
}

func TestActivityHoldProtectsIdleAndLeavesSlotsFree(t *testing.T) {
	run := instance.NewInstance("busy", "studio", "", 0, time.Minute, 0)
	run.MarkRunning(false, nil)
	run.InitConcurrencyLimit(instance.ConcurrencyConfig{MaxConcurrentRequests: 1})
	observer := observeFixture(t, run)
	observer.ObserveActivity(true)
	require.Zero(t, run.GetActiveRequests())
	require.False(t, run.Evictable())
	require.False(t, Expired(run.IdleSnapshot(), config.RunSettings{KeepAlive: time.Minute}, utils.Now().Add(time.Hour)))
	_, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: run, Activity: run.GetLastActivity()}})
	require.False(t, claimed)
	lease, err := run.Acquire(t.Context(), nil)
	require.NoError(t, err)
	lease.Release()
	now := utils.Now().Add(time.Hour)
	restore := utils.SetClock(utils.FixedClock(now))
	defer restore()
	observer.ObserveActivity(false)
	require.Equal(t, now, run.GetLastActivity())
	require.False(t, Expired(run.IdleSnapshot(), config.RunSettings{KeepAlive: time.Minute}, now.Add(time.Minute-time.Nanosecond)))
	require.True(t, Expired(run.IdleSnapshot(), config.RunSettings{KeepAlive: time.Minute}, now.Add(time.Minute)))
	observer.ObserveActivity(false)
	require.Equal(t, now, run.GetLastActivity())
	observer.ObserveActivity(true)
	run.MarkFailed("dead engine")
	require.True(t, run.IdleSnapshot().Leased == false)
	run.Cancel()
}

func TestObservedBusyAndIdleClaimAreSerialized(t *testing.T) {
	for range 100 {
		run := instance.NewInstance("race", "studio", "", 0, time.Minute, 0)
		run.MarkRunning(false, nil)
		observer := observeFixture(t, run)
		start := make(chan struct{})
		holds := make(chan struct{})
		claims := make(chan bool)
		done := make(chan struct{})
		go func() { <-start; observer.ObserveActivity(true); close(holds) }()
		go func() {
			<-start
			release, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: run, Activity: run.GetLastActivity()}})
			claims <- claimed
			<-done
			if claimed {
				release()
			}
		}()
		close(start)
		<-holds
		claimed := <-claims
		require.False(t, claimed && run.IdleSnapshot().Leased)
		if claimed {
			require.ErrorIs(t, run.RefreshActivity(), instance.ErrStopping)
		} else {
			require.NoError(t, run.RefreshActivity())
		}
		close(done)
		run.Cancel()
	}
}

func TestActivityHoldPreventsPressureEviction(t *testing.T) {
	f := newReclaimFixture(t, true)
	observeFixture(t, f.older).ObserveActivity(true)
	observeFixture(t, f.newer).ObserveActivity(true)
	evicted, err := f.admit(t)
	require.Error(t, err)
	require.Empty(t, evicted)
	require.Equal(t, instance.StatusRunning, f.older.GetStatus())
	require.Equal(t, instance.StatusRunning, f.newer.GetStatus())
	require.NoError(t, f.mgr.StopInstance(context.Background(), f.older.ID))
	require.False(t, f.older.IdleSnapshot().Leased)
}

type directQueueTransport struct{}

func (directQueueTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := "{}"
	if req.URL.Path == "/queue" {
		data, _ := json.Marshal(map[string]any{"queue_running": []int{1}, "queue_pending": []int{}})
		body = string(data)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestManagerStopDrainsObservedBusySampler(t *testing.T) {
	r := reservationRig(t, true)
	fresh := &config.AppsConfig{}
	svc, _ := r.m.AppsConfig().LookupApp("generic")
	svc.Runtime.ActivityProbe = &config.ActivityProbe{Path: "/queue", BusyWhenNonempty: []string{"queue_running", "queue_pending"}}
	require.NoError(t, fresh.AddApp("generic", svc))
	r.m.ReloadConfig(fresh)
	r.m.healthMon = health.NewMonitor(r.m.instances, health.MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, MaxRetries: 3, HTTPClient: &http.Client{Transport: directQueueTransport{}}})
	result, err := r.m.EnsureRun(t.Context(), EnsureRequest{Provider: "generic"})
	require.NoError(t, err)
	awaitRunning(t, result.Run)
	require.Eventually(t, func() bool { return result.Run.ToInfo("node").EngineBusy }, contractWait, time.Millisecond)
	require.Zero(t, result.Run.GetActiveRequests())
	ctx, cancel := context.WithTimeout(t.Context(), contractWait)
	defer cancel()
	require.NoError(t, r.m.StopInstance(ctx, result.Run.ID))
	require.NoError(t, result.Run.WaitForGoroutines(ctx))
	require.False(t, result.Run.ToInfo("node").EngineBusy)
	require.Empty(t, result.Run.Resolved().Commitments)
}

func TestEnsureRefreshRejectsOldClaimWithoutLeasingFutureWork(t *testing.T) {
	run := instance.NewInstance("submission-race", "studio", "", 0, time.Minute, 0)
	run.MarkRunning(false, nil)
	defer run.Cancel()
	observer := observeFixture(t, run)
	old := run.GetLastActivity()
	restore := utils.SetClock(utils.FixedClock(old.Add(time.Second)))
	defer restore()
	require.NoError(t, run.RefreshActivity())
	_, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: run, Activity: old}})
	require.False(t, claimed, "ensure must invalidate a claim decided against old activity")
	require.Zero(t, run.GetActiveRequests())
	release, claimed := instance.ClaimIdleStops([]instance.IdleStopCandidate{{Instance: run, Activity: run.GetLastActivity()}})
	require.True(t, claimed, "ensure has no lease over a direct submission after it returns")
	defer release()
	observer.ObserveActivity(true)
	require.False(t, run.IdleSnapshot().Leased, "a later busy sample cannot revoke a stop already claimed")
}

func TestEveryBusySampleRefreshesActivity(t *testing.T) {
	run := instance.NewInstance("clock", "studio", "", 0, time.Minute, 0)
	run.MarkRunning(false, nil)
	defer run.Cancel()
	now := utils.Now().Add(time.Hour)
	restore := utils.SetClock(utils.FixedClock(now))
	defer restore()
	run.ObserveActivity(true)
	require.Equal(t, now, run.GetLastActivity())
	restore()
	now = now.Add(time.Minute)
	restore = utils.SetClock(utils.FixedClock(now))
	defer restore()
	run.ObserveActivity(true)
	require.Equal(t, now, run.GetLastActivity())
}

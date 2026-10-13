package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

type queueTransport func(*http.Request) (*http.Response, error)

func (f queueTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func queueConfig() CheckConfig {
	return CheckConfig{HTTPHealthPath: "/health", ExpectedStatus: http.StatusOK, ActivityProbe: &config.ActivityProbe{Path: "/queue", BusyWhenNonempty: []string{"queue_running", "queue_pending"}}}
}

func TestActivityProbeRetainsStateOnFailure(t *testing.T) {
	failures := 0
	run := instance.NewInstance("probe", "studio", "", 8188, time.Minute, 0)
	run.MarkRunning(false, nil)
	defer run.Cancel()
	body := "{\"queue_running\":[[1]],\"queue_pending\":[]}"
	status := http.StatusOK
	var observed bool
	monitor := NewMonitor(instance.NewRegistry(), MonitorConfig{Timeout: time.Second, HTTPClient: &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
		observed = true
		require.Equal(t, "/queue", req.URL.Path)
		require.Equal(t, "localhost:8188", req.URL.Host)
		require.Empty(t, req.Header.Get("Authorization"))
		_, bounded := req.Context().Deadline()
		require.True(t, bounded)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}})
	monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
	require.True(t, observed)
	require.True(t, run.ToInfo("node").EngineBusy)
	for _, malformed := range []string{"bad json", "{}", "{\"queue_running\":[]}", "{\"queue_running\":{},\"queue_pending\":[]}", strings.Repeat(" ", activityProbeMaxBytes+1)} {
		body = "{\"queue_running\":[[1]],\"queue_pending\":[]}"
		monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
		body = malformed
		monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
		require.True(t, run.ToInfo("node").EngineBusy)
	}
	status = http.StatusServiceUnavailable
	body = "{\"queue_running\":[],\"queue_pending\":[]}"
	monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
	require.True(t, run.ToInfo("node").EngineBusy)
	status = http.StatusOK
	monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
	require.False(t, run.ToInfo("node").EngineBusy)
	body = "{\"queue_running\":[],\"queue_pending\":[[2]]}"
	monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
	require.True(t, run.ToInfo("node").EngineBusy)
}

func TestActivitySamplerCancellationAndDeadRunRelease(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			run := instance.NewInstance("sampling", "studio", "", 8188, time.Minute, 0)
			run.HealthURL = "http://localhost:8188/health"
			run.MarkRunning(false, nil)
			run.ObserveActivity(true)
			reg := instance.NewRegistry()
			require.NoError(t, reg.Register(run))
			entered := make(chan struct{})
			var first atomic.Bool
			monitor := NewMonitor(reg, MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, MaxRetries: 1, HTTPClient: &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/queue" {
					if first.CompareAndSwap(false, true) {
						close(entered)
					}
					if stop {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{\"queue_running\":[1],\"queue_pending\":[]}")), Header: http.Header{}}, nil
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
			})}})
			run.TrackGoroutine()
			go func() { defer run.GoroutineDone(); monitor.Run(run.Context(), run, queueConfig()) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("scheduler did not sample activity")
			}
			if stop {
				run.SetStatus(instance.StatusStopping)
				run.Cancel()
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, run.WaitForGoroutines(ctx))
			if !stop {
				require.Equal(t, instance.StatusFailed, run.GetStatus())
				run.Cancel()
			}
			require.False(t, run.ToInfo("node").EngineBusy)
		})
	}
}

func TestActivityFailuresReleaseHoldAndSuccessResetsStreak(t *testing.T) {
	for _, kind := range []string{"timeout", "non-200", "payload"} {
		t.Run(kind, func(t *testing.T) {
			failures := 0
			run := instance.NewInstance("failed-samples", "studio", "", 8188, time.Minute, 0)
			run.MarkRunning(false, nil)
			defer run.Cancel()
			failing := false
			monitor := NewMonitor(instance.NewRegistry(), MonitorConfig{Timeout: time.Second, HTTPClient: &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
				body, status := `{"queue_running":[1],"queue_pending":[]}`, http.StatusOK
				if failing {
					switch kind {
					case "timeout":
						return nil, context.DeadlineExceeded
					case "non-200":
						status = http.StatusServiceUnavailable
					case "payload":
						body = `{}`
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})}})
			monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
			require.True(t, run.ToInfo("node").EngineBusy)
			failing = true
			for range activityProbeFailureLimit - 1 {
				monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
				require.True(t, run.ToInfo("node").EngineBusy)
			}
			failing = false
			monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
			failing = true
			for range activityProbeFailureLimit - 1 {
				monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
				require.True(t, run.ToInfo("node").EngineBusy)
			}
			monitor.checkActivity(t.Context(), run, queueConfig(), &failures)
			require.False(t, run.ToInfo("node").EngineBusy)
			require.True(t, run.Evictable())
			require.Equal(t, instance.StatusRunning, run.GetStatus())
		})
	}
}

func TestActivitySamplerHealthyEngineReleasesStaleHold(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	run := instance.NewInstance("stale-hold", "studio", "", 8188, time.Minute, 0)
	run.HealthURL = "http://localhost:8188/system_stats"
	run.MarkRunning(false, nil)
	defer run.Cancel()
	registry := instance.NewRegistry()
	require.NoError(t, registry.Register(run))
	failures := make(chan struct{})
	var queries, healthChecks atomic.Int32
	monitor := NewMonitor(registry, MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, HTTPClient: &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
		body, status := `{}`, http.StatusOK
		if req.URL.Path == "/queue" {
			if queries.Add(1) == 1 {
				body = `{"queue_running":[1],"queue_pending":[]}`
			} else {
				select {
				case <-failures:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				status = http.StatusServiceUnavailable
			}
		} else {
			healthChecks.Add(1)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}})
	done := make(chan struct{})
	go func() { defer close(done); monitor.Run(run.Context(), run, queueConfig()) }()
	require.Eventually(t, func() bool { return run.ToInfo("node").EngineBusy }, time.Second, time.Millisecond)
	close(failures)
	require.Eventually(t, func() bool { return run.Evictable() }, time.Second, time.Millisecond)
	require.GreaterOrEqual(t, healthChecks.Load(), int32(activityProbeFailureLimit))
	require.Equal(t, instance.StatusRunning, run.GetStatus())
	run.Cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sampler did not stop")
	}
}

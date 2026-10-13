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

func TestActivityProbeObservations(t *testing.T) {
	run := instance.NewInstance("probe", "studio", "", 8188, time.Minute, 0)
	run.MarkRunning(false, nil)
	defer run.Cancel()
	body := `{"queue_running":[[1]],"queue_pending":[]}`
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
	busy, ok := monitor.checkActivity(t.Context(), run, queueConfig())
	require.True(t, observed)
	require.True(t, ok)
	require.True(t, busy)
	require.False(t, run.ToInfo("node").EngineBusy, "sampling must not change the hold")
	run.ObserveActivity(true)
	for _, malformed := range []string{"bad json", "{}", `{"queue_running":[]}`, `{"queue_running":{},"queue_pending":[]}`, strings.Repeat(" ", activityProbeMaxBytes+1)} {
		body = malformed
		busy, ok = monitor.checkActivity(t.Context(), run, queueConfig())
		require.False(t, ok)
		require.False(t, busy)
		require.True(t, run.ToInfo("node").EngineBusy)
	}
	status = http.StatusServiceUnavailable
	body = `{"queue_running":[],"queue_pending":[]}`
	_, ok = monitor.checkActivity(t.Context(), run, queueConfig())
	require.False(t, ok)
	status = http.StatusOK
	busy, ok = monitor.checkActivity(t.Context(), run, queueConfig())
	require.True(t, ok)
	require.False(t, busy)
	require.True(t, run.ToInfo("node").EngineBusy, "the loop owns hold release")
	body = `{"queue_running":[],"queue_pending":[[2]]}`
	busy, ok = monitor.checkActivity(t.Context(), run, queueConfig())
	require.True(t, ok)
	require.True(t, busy)
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

func TestActivityUsesHealthRetryThresholdAndResetsStreak(t *testing.T) {
	for _, kind := range []string{"timeout", "non-200", "payload"} {
		for _, policy := range []struct {
			name                         string
			retries, override, threshold int
		}{
			{"monitor retries", 2, 0, 2}, {"provider retries", 2, 5, 5},
		} {
			t.Run(kind+"/"+policy.name, func(t *testing.T) {
				defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
				run := instance.NewInstance("retry-policy", "studio", "", 8188, time.Minute, 0)
				run.HealthURL = "http://localhost:8188/system_stats"
				run.MarkRunning(false, nil)
				registry := instance.NewRegistry()
				require.NoError(t, registry.Register(run))
				samples := make(chan bool)
				observed := make(chan bool, 1)
				monitor := NewMonitor(registry, MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, MaxRetries: policy.retries, HTTPClient: &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
					body, status := `{"queue_running":[1],"queue_pending":[]}`, http.StatusOK
					if req.URL.Path == "/queue" {
						select {
						case failing := <-samples:
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
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					} else {
						observed <- run.ToInfo("node").EngineBusy
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				})}})
				cfg := queueConfig()
				cfg.MaxRetries = policy.override
				done := make(chan struct{})
				go func() { defer close(done); monitor.Run(run.Context(), run, cfg) }()
				defer func() {
					run.Cancel()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("sampler did not stop")
					}
				}()
				sample := func(failing, busy bool) {
					t.Helper()
					select {
					case samples <- failing:
					case <-time.After(time.Second):
						t.Fatal("sample was not requested")
					}
					select {
					case held := <-observed:
						require.Equal(t, busy, held)
					case <-time.After(time.Second):
						t.Fatal("sample was not observed")
					}
				}
				sample(false, true)
				for range policy.threshold - 1 {
					sample(true, true)
				}
				sample(false, true)
				for range policy.threshold - 1 {
					sample(true, true)
				}
				sample(true, false)
				require.True(t, run.Evictable())
				require.Equal(t, instance.StatusRunning, run.GetStatus())
				sample(false, true)
			})
		}
	}
}

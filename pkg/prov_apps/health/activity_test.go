package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

type queueTransport func(*http.Request) (*http.Response, error)

func (f queueTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func queueConfig() CheckConfig {
	return CheckConfig{HTTPHealthPath: "/health", ExpectedStatus: http.StatusOK, Observe: &schema.Observe{Path: "/queue", Format: "json", Busy: []string{"queue_running", "queue_pending"}}}
}

func TestObserveBusyOnRunPortWithoutCredentials(t *testing.T) {
	run := instance.NewInstance("probe", "studio", "", 8188, time.Minute, 0)
	run.MarkRunning(false, nil)
	defer run.Cancel()
	body := `{"queue_running":[[1]],"queue_pending":[]}`
	status := http.StatusOK
	var observed bool
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	cookie, err := http.ParseSetCookie("session=fixture; HttpOnly; SameSite=Lax")
	require.NoError(t, err)
	endpoint := &url.URL{Scheme: "http", Host: "localhost:8188"}
	jar.SetCookies(endpoint, []*http.Cookie{cookie})
	require.NotEmpty(t, jar.Cookies(endpoint))
	monitor := NewMonitor(instance.NewRegistry(), MonitorConfig{Timeout: time.Second, HTTPClient: &http.Client{Jar: jar, Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
		observed = true
		require.Equal(t, "/queue", req.URL.Path)
		require.Equal(t, "localhost:8188", req.URL.Host)
		require.Empty(t, req.Header)
		_, bounded := req.Context().Deadline()
		require.True(t, bounded)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}})
	read := func() Observation {
		ctx, cancel := monitor.checkContext(t.Context(), queueConfig())
		defer cancel()
		return ReadObservation(ctx, monitor.client, run, queueConfig().Observe)
	}
	run.BaseURL = "http://advertised.example:8188"
	result := read()
	require.True(t, observed)
	require.Empty(t, result.Cause)
	require.Equal(t, new(true), result.Busy)
	require.False(t, run.ToInfo("node").EngineBusy, "sampling must not change the hold")
	run.ObserveActivity(true)
	for _, bad := range []struct {
		body  string
		cause ObservationCause
	}{
		{"bad json", ObservationMalformed},
		{"{}", ObservationSeriesAbsent},
		{`{"queue_running":[]}`, ObservationSeriesAbsent},
		{`{"queue_running":[1],"queue_pending":null}`, ObservationMalformed},
		{`{"queue_running":{},"queue_pending":[]}`, ObservationMalformed},
	} {
		body = bad.body
		result = read()
		require.Nil(t, result.Busy)
		require.Equal(t, bad.cause, result.Cause)
		require.True(t, run.ToInfo("node").EngineBusy)
	}
	status = http.StatusServiceUnavailable
	body = `{"queue_running":[],"queue_pending":[]}`
	require.Equal(t, ObservationErrorStatus, read().Cause)
	status = http.StatusCreated
	require.Equal(t, Observation{Cause: ObservationErrorStatus, StatusCode: status}, read())
	status = http.StatusOK
	require.Equal(t, new(false), read().Busy)
	require.True(t, run.ToInfo("node").EngineBusy, "the loop owns hold release")
	body = `{"queue_running":[],"queue_pending":[[2]]}`
	require.Equal(t, new(true), read().Busy)

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
			go func() {
				defer run.GoroutineDone()
				monitor.Run(run.Context(), run, queueConfig(), func(current *instance.Instance) { current.MarkRunning(true, nil) })
			}()
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
				observed := make(chan instance.InstanceInfo, 1)
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
						select {
						case observed <- run.ToInfo("node"):
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				})}})
				cfg := queueConfig()
				cfg.MaxRetries = policy.override
				monitor.ConfigureActivity(run, cfg)
				done := make(chan struct{})
				go func() {
					defer close(done)
					monitor.Run(run.Context(), run, cfg, func(current *instance.Instance) { current.MarkRunning(true, nil) })
				}()
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
						require.Equal(t, busy, held.EngineBusy)
						if failing {
							require.Equal(t, "unknown", held.Activity.State)
						} else {
							require.Equal(t, "active", held.Activity.State)
						}
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
				require.Equal(t, "activity_unknown", run.EvictionBlockedReason())
				require.Equal(t, instance.StatusRunning, run.GetStatus())
				sample(false, true)
			})
		}
	}
}

func TestMonitorObservationValidityWindow(t *testing.T) {
	run := instance.NewInstance("window", "studio", "", 8188, time.Minute, 0)
	defer run.Cancel()
	run.MarkRunning(false, nil)
	registry := instance.NewRegistry()
	require.NoError(t, registry.Register(run))
	now := utils.Now()
	restore := utils.SetClock(utils.FixedClock(now))
	defer restore()

	cfg := queueConfig()
	cfg.Interval = 5 * time.Millisecond
	client := observationClient(http.StatusOK, `{"queue_running":[1],"queue_pending":[]}`)
	monitor := NewMonitor(registry, MonitorConfig{Timeout: time.Second, MaxRetries: 1, HTTPClient: client})
	monitor.ConfigureActivity(run, cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.Run(run.Context(), run, cfg, func(*instance.Instance) {})
	}()
	defer func() {
		run.Cancel()
		<-done
	}()

	require.Eventually(t, func() bool { return run.ToInfo("node").EngineBusy }, time.Second, time.Millisecond)
	window := 2*cfg.Interval + constants.HealthCheckTimeout
	require.Equal(t, "active", run.ActivitySnapshot(now.Add(window-time.Millisecond)).State)
	require.Equal(t, "unknown", run.ActivitySnapshot(now.Add(window)).State)
	require.True(t, run.ToInfo("node").EngineBusy)
}

func TestMonitorSkipsObservationWithoutBusyRole(t *testing.T) {
	run := instance.NewInstance("numeric-only", "studio", "", 8188, time.Minute, 0)
	require.True(t, run.Admit())
	run.HealthURL = "http://localhost:8188/health"
	defer run.Cancel()
	registry := instance.NewRegistry()
	require.NoError(t, registry.Register(run))
	var called atomic.Int32
	client := &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/metrics" {
			called.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}
	monitor := NewMonitor(registry, MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, MaxRetries: 1, HTTPClient: client})
	cfg := queueConfig()
	cfg.Observe = &schema.Observe{Path: "/metrics", Format: "prometheus", Running: "running"}
	monitor.ConfigureActivity(run, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	published := false
	monitor.Run(ctx, run, cfg, func(*instance.Instance) {
		published = true
		cancel()
	})
	require.True(t, published)
	require.Zero(t, called.Load())
	require.Equal(t, "idle", run.ToInfo("node").Activity.State)
}

func TestMonitorScopesBusyObservationToEffectiveRuntime(t *testing.T) {
	for _, runtime := range []string{"mlx", "mlx-vlm"} {
		t.Run(runtime, func(t *testing.T) {
			run := observationRun(t, runtime)
			run.HealthURL = "http://localhost:8188/health"
			require.True(t, run.Admit())
			registry := instance.NewRegistry()
			require.NoError(t, registry.Register(run))
			calls := 0
			client := &http.Client{Transport: queueTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/queue" {
					calls++
					body := `{"queue_running":[1],"queue_pending":[]}`
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
			})}
			monitor := NewMonitor(registry, MonitorConfig{Interval: time.Millisecond, Timeout: time.Second, MaxRetries: 1, HTTPClient: client})
			cfg := queueConfig()
			cfg.Observe.Runtimes = []string{"mlx-vlm"}
			monitor.ConfigureActivity(run, cfg)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			monitor.Run(ctx, run, cfg, func(run *instance.Instance) {
				run.MarkRunning(true, nil)
				cancel()
			})
			require.Equal(t, instance.StatusRunning, run.GetStatus())
			info := run.ToInfo("node")
			if runtime == "mlx" {
				require.Zero(t, calls)
				require.False(t, info.EngineBusy)
				require.Equal(t, "idle", info.Activity.State)
			} else {
				require.Positive(t, calls, "the effective runtime must be sampled")
				require.True(t, info.EngineBusy)
				require.Equal(t, "active", info.Activity.State)
			}
		})
	}
}

package prov_apps

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	obsruns "github.com/stperic/zzrouter/pkg/observability/runs"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

func readinessWatch(t *testing.T, probe *health.ReadinessProbe, monitor *health.Monitor) (*instance.Instance, *health.ReadinessWriter, <-chan struct{}, <-chan Event) {
	t.Helper()
	require.NoError(t, probe.Validate())
	inst := instance.NewInstance("startup", "engine", "model", 0, 0, 0)
	writer := health.NewReadinessWriter(io.Discard, probe, nil, nil)
	events := make(chan Event, 4)
	manager := &ProviderAppManager{healthMon: monitor, auditEvents: events}
	done := make(chan struct{})
	go func() { defer close(done); manager.watchReadiness(inst, writer, probe) }()
	t.Cleanup(func() {
		inst.Cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("readiness watcher did not exit")
		}
	})
	return inst, writer, done, events
}

func startupProbe(stall, absolute time.Duration) *health.ReadinessProbe {
	return &health.ReadinessProbe{Timeout: absolute, StallTimeout: stall, LogPatterns: health.LogPatterns{Success: []health.PatternMatcher{{Pattern: "READY"}}}}
}

func waitReadiness(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("readiness watcher did not resolve")
	}
}

func TestReadinessSteadyOutputExtendsStallWindow(t *testing.T) {
	probe := startupProbe(200*time.Millisecond, 2*time.Second)
	inst, writer, done, _ := readinessWatch(t, probe, nil)
	start := utils.Now()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for utils.Now().Sub(start) < 3*probe.StallTimeout {
		<-ticker.C
		_, err := writer.Write([]byte("loading weights"))
		require.NoError(t, err)
		require.Equal(t, instance.StatusStarting, inst.GetStatus(), inst.ToInfo("").ErrorMessage)
	}
	_, err := writer.Write([]byte("\nREADY\n"))
	require.NoError(t, err)
	waitReadiness(t, done, time.Second)
	require.Equal(t, instance.StatusRunning, inst.GetStatus())
}

func TestReadinessSilenceFailsWithLastLine(t *testing.T) {
	probe := startupProbe(60*time.Millisecond, time.Second)
	inst, writer, done, events := readinessWatch(t, probe, nil)
	_, err := writer.Write([]byte("old line\nCUDA graph capture"))
	require.NoError(t, err)
	waitReadiness(t, done, 500*time.Millisecond)
	require.Equal(t, instance.StatusFailed, inst.GetStatus())
	require.Equal(t, "no output for 60ms while starting; last line: CUDA graph capture", inst.ToInfo("").ErrorMessage)
	event := <-events
	require.Equal(t, obsruns.FailureReadiness, event.Cause)
	require.Equal(t, inst.ToInfo("").ErrorMessage, event.Message)
}

func TestReadinessAbsentStallUsesAbsoluteTimeout(t *testing.T) {
	cfg := config.ServiceConfig{Runtime: &config.AppRuntimeConfig{HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: "200ms", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}}}}
	probe := buildHealthCheckConfig(cfg, 0).ReadinessProbe
	require.NotNil(t, probe)
	require.Zero(t, probe.StallTimeout)
	start := utils.Now()
	inst, _, done, _ := readinessWatch(t, probe, nil)
	select {
	case <-done:
		t.Fatal("startup ended before absolute timeout")
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, instance.StatusStarting, inst.GetStatus())
	waitReadiness(t, done, time.Second)
	require.GreaterOrEqual(t, utils.Now().Sub(start), probe.Timeout)
	require.Equal(t, "not ready within 200ms", inst.ToInfo("").ErrorMessage)
}

func TestReadinessSilenceAfterReadyNeverFails(t *testing.T) {
	probe := startupProbe(60*time.Millisecond, time.Second)
	inst, writer, done, events := readinessWatch(t, probe, nil)
	_, err := writer.Write([]byte("READY\n"))
	require.NoError(t, err)
	waitReadiness(t, done, probe.StallTimeout/2)
	time.Sleep(2 * probe.StallTimeout)
	require.Equal(t, instance.StatusRunning, inst.GetStatus())
	require.Empty(t, inst.ToInfo("").ErrorMessage)
	require.Len(t, events, 1)
	require.Equal(t, EventInstanceRunning, (<-events).Type)
}

func TestReadinessHealthTransitionRetiresWatcher(t *testing.T) {
	inst, _, done, _ := readinessWatch(t, startupProbe(time.Second, 2*time.Second), nil)
	inst.MarkRunning(false, nil)
	waitReadiness(t, done, 100*time.Millisecond)
}

type readinessTransport func(*http.Request) (*http.Response, error)

func (f readinessTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadinessStallDuringServeCheck(t *testing.T) {
	started := make(chan struct{})
	monitor := health.NewMonitor(nil, health.MonitorConfig{HTTPClient: &http.Client{Transport: readinessTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}})
	probe := startupProbe(100*time.Millisecond, time.Second)
	probe.Serve = &health.ServeCheck{Path: "/generate"}
	inst, writer, done, _ := readinessWatch(t, probe, monitor)
	_, err := writer.Write([]byte("READY\nprofiling"))
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("serve check not started")
	}
	waitReadiness(t, done, 500*time.Millisecond)
	require.Equal(t, instance.StatusFailed, inst.GetStatus())
	require.Contains(t, inst.ToInfo("").ErrorMessage, "no output for 100ms")
	require.Contains(t, inst.ToInfo("").ErrorMessage, "profiling")
}

func TestBuildReadinessStallDuration(t *testing.T) {
	cfg := config.ServiceConfig{Runtime: &config.AppRuntimeConfig{HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{Timeout: "900s", StallTimeout: "240s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}}}}}}
	probe := buildHealthCheckConfig(cfg, 0).ReadinessProbe
	require.NotNil(t, probe)
	require.Equal(t, 240*time.Second, probe.StallTimeout)
	require.Equal(t, 900*time.Second, probe.Timeout)
	cfg.Runtime.HealthCheck.ReadinessProbe.StallTimeout = "invalid"
	require.Nil(t, buildHealthCheckConfig(cfg, 0).ReadinessProbe)
}

func TestReadinessServeOutputKeepsProgressAfterLog(t *testing.T) {
	probe := startupProbe(time.Second, 2*time.Second)
	probe.Serve = &health.ServeCheck{Path: "/generate"}
	writer := health.NewReadinessWriter(io.Discard, probe, nil, nil)
	_, err := writer.Write([]byte("READY\n"))
	require.NoError(t, err)
	before := writer.LastOutputAt()
	time.Sleep(time.Millisecond)
	_, err = writer.Write([]byte(strings.Repeat("x", 500)))
	require.NoError(t, err)
	require.True(t, writer.LastOutputAt().After(before))
	require.Len(t, writer.LastLine(), 300)
}

func TestReadinessServeRejectsSuccessAfterDeadline(t *testing.T) {
	monitor := health.NewMonitor(nil, health.MonitorConfig{HTTPClient: &http.Client{Transport: readinessTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}})
	probe := startupProbe(60*time.Millisecond, time.Second)
	probe.Serve = &health.ServeCheck{Path: "/generate"}
	inst, writer, done, _ := readinessWatch(t, probe, monitor)
	_, err := writer.Write([]byte("READY\n"))
	require.NoError(t, err)
	waitReadiness(t, done, time.Second)
	require.Equal(t, instance.StatusFailed, inst.GetStatus())
	require.Contains(t, inst.ToInfo("").ErrorMessage, "no output for 60ms")
}

func TestReadinessOutputWhileAwaitingServe(t *testing.T) {
	attempts := 0
	monitor := health.NewMonitor(nil, health.MonitorConfig{HTTPClient: &http.Client{Transport: readinessTransport(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}})
	probe := startupProbe(200*time.Millisecond, 4*time.Second)
	probe.Serve = &health.ServeCheck{Path: "/generate"}
	inst, writer, done, _ := readinessWatch(t, probe, monitor)
	_, err := writer.Write([]byte("READY\n"))
	require.NoError(t, err)
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(probe.Timeout)
	defer timeout.Stop()
	for {
		select {
		case <-done:
			require.Equal(t, instance.StatusRunning, inst.GetStatus(), inst.ToInfo("").ErrorMessage)
			require.Equal(t, 2, attempts)
			return
		case <-ticker.C:
			_, err := writer.Write([]byte("warming up\n"))
			require.NoError(t, err)
		case <-timeout.C:
			t.Fatal("serve readiness never resolved")
		}
	}
}

func TestReadinessPublicationContractEndClaim(t *testing.T) {
	inst, writer, done, events := readinessWatch(t, startupProbe(time.Second, 2*time.Second), nil)
	require.True(t, inst.ClaimEnd()) // status is still Starting, but retirement owns the run
	_, err := writer.Write([]byte("READY\n"))
	require.NoError(t, err)
	waitReadiness(t, done, time.Second)
	require.Equal(t, instance.StatusStarting, inst.GetStatus())
	require.True(t, inst.GetStartedAt().IsZero())
	require.Empty(t, events, "the watcher cannot publish a losing readiness transition")
}

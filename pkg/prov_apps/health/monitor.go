package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"

	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Health monitor defaults. These are intentionally more aggressive than the
// centralized constants in pkg/constants/ (HealthCheckInterval=30s, etc.)
// because prov_apps monitors localhost-only processes where faster detection
// of failures is preferred over network tolerance.
const (
	DefaultInterval     = 10 * time.Second
	DefaultTimeout      = 3 * time.Second
	DefaultStartupGrace = 5 * time.Second
	DefaultMaxRetries   = 3

	// serveCheckTimeout bounds one serve check. It is deliberately longer
	// than the health-endpoint timeout: the request that proves an engine
	// can serve runs a real (one-token) generation, and the first one runs
	// while weights are still being paged in.
	serveCheckTimeout = 60 * time.Second
)

// wireModelPlaceholder is expanded in a serve check body to the token the
// engine keys on. Named here rather than imported from pkg/config to keep
// health free of a config dependency.
const wireModelPlaceholder = "${WIRE_MODEL}"

// MonitorConfig holds global health monitoring settings.
type MonitorConfig struct {
	Interval     time.Duration
	Timeout      time.Duration
	StartupGrace time.Duration
	MaxRetries   int
	HTTPClient   *http.Client
}

// DefaultMonitorConfig returns default monitoring settings.
func DefaultMonitorConfig() MonitorConfig {
	return MonitorConfig{
		Interval:     DefaultInterval,
		Timeout:      DefaultTimeout,
		StartupGrace: DefaultStartupGrace,
		MaxRetries:   DefaultMaxRetries,
	}
}

// Monitor performs HTTP health checking for a single instance.
// It runs as a goroutine and updates instance status via the registry.
//
// Lifecycle: starting → running (on first health check pass),
// running → unhealthy (on first failure), unhealthy → failed (after MaxRetries).
// Process death causes immediate failure without retries.
type Monitor struct {
	registry *instance.Registry
	config   MonitorConfig
	client   *http.Client
}

// NewMonitor creates a health monitor.
func NewMonitor(registry *instance.Registry, config MonitorConfig) *Monitor {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: config.Timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true, // prevent goroutine leaks
			},
		}
	}
	return &Monitor{
		registry: registry,
		config:   config,
		client:   client,
	}
}

// Config returns the monitor's configuration.
func (m *Monitor) Config() MonitorConfig {
	return m.config
}

// Run starts health monitoring for an instance. It blocks until ctx is cancelled
// or the instance transitions to a terminal state. The caller should run this in
// a goroutine and call inst.TrackGoroutine/GoroutineDone.
func (m *Monitor) Run(ctx context.Context, inst *instance.Instance, hcConfig CheckConfig) {
	instanceID := inst.ID

	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in health monitor", "instance", instanceID, "error", r, "stack", string(debug.Stack()))
		}
	}()

	if ctx.Err() != nil {
		return
	}

	// Resolve per-instance overrides
	interval := m.config.Interval
	if hcConfig.Interval > 0 {
		interval = hcConfig.Interval
	}

	maxFailures := m.config.MaxRetries
	if hcConfig.MaxRetries > 0 {
		maxFailures = hcConfig.MaxRetries
	}

	// Startup grace period
	select {
	case <-time.After(m.config.StartupGrace):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check instance still exists in registry
			current, exists := m.registry.Get(instanceID)
			if !exists {
				return
			}

			// Check process liveness first — fail immediately if dead
			if current.ProcessID != 0 && !process.ProcessExists(int32(current.ProcessID)) {
				if err := m.registry.UpdateStatus(instanceID, instance.StatusFailed,
					fmt.Sprintf("Process %d is not running", current.ProcessID)); err != nil {
					slog.Warn("failed to update instance status", "instance", instanceID, "error", err)
				}
				return
			}

			// HTTP health check
			healthy := m.checkHTTP(ctx, current, hcConfig)

			if healthy {
				current.SetLastHealthCheck(utils.Now())
				consecutiveFailures = 0

				// A listening socket is not a loaded model. When the
				// provider declares a serve check, it — not the health
				// endpoint — decides the first move to running.
				if current.GetStatus() != instance.StatusRunning && m.Serves(ctx, current, hcConfig.ServeCheck()) {
					if err := m.registry.UpdateStatus(instanceID, instance.StatusRunning, ""); err != nil {
						slog.Warn("failed to update instance status", "instance", instanceID, "status", "running", "error", err)
					}
					current.SetStartedAtIfZero()
					current.UpdateLastUsed()
				}
			} else {
				// A starting instance is loading weights, and an engine that
				// answers 503 until they land is behaving correctly. The
				// readiness probe owns that window and fails the instance on
				// its own deadline; counting these as liveness failures kills
				// a legitimate slow load after maxFailures × interval, which
				// is a fraction of the readiness timeout on every provider.
				if hcConfig.ReadinessProbe != nil && current.GetStatus() == instance.StatusStarting {
					continue
				}

				consecutiveFailures++

				if consecutiveFailures == 1 && current.GetStatus() == instance.StatusRunning {
					if err := m.registry.UpdateStatus(instanceID, instance.StatusUnhealthy, "Health check failed"); err != nil {
						slog.Warn("failed to update instance status", "instance", instanceID, "status", "unhealthy", "error", err)
					}
				}

				if consecutiveFailures >= maxFailures {
					if err := m.registry.UpdateStatus(instanceID, instance.StatusFailed, "Health check failures"); err != nil {
						slog.Warn("failed to update instance status", "instance", instanceID, "status", "failed", "error", err)
					}
					return
				}
			}
		}
	}
}

// checkHTTP performs an HTTP health check.
func (m *Monitor) checkHTTP(ctx context.Context, inst *instance.Instance, config CheckConfig) bool {
	if config.HTTPHealthPath == "" || inst.HealthURL == "" {
		// No HTTP check configured — process is alive (already checked above)
		return true
	}

	timeout := m.config.Timeout
	if config.Timeout > 0 {
		timeout = config.Timeout
	}

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := config.HTTPHealthMethod
	if method == "" {
		method = "GET"
	}

	req, err := http.NewRequestWithContext(checkCtx, method, inst.HealthURL, nil)
	if err != nil {
		return false
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	expected := config.ExpectedStatus
	if expected == 0 {
		expected = 200
	}
	return resp.StatusCode == expected
}

// Serves reports whether the instance can actually serve inference.
//
// A nil check means the provider has nothing beyond its health endpoint to
// offer, so the caller's existing signal stands. Otherwise the declared
// request must come back 2xx: engines that accept connections while still
// loading weights answer /health and /v1/models long before they can answer
// a completion.
func (m *Monitor) Serves(ctx context.Context, inst *instance.Instance, check *ServeCheck) bool {
	if check == nil {
		return true
	}

	timeout := m.config.Timeout
	if serveCheckTimeout > timeout {
		timeout = serveCheckTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d%s", constants.Localhost, inst.Port, check.Path)
	req, err := http.NewRequestWithContext(reqCtx, check.method(), url, strings.NewReader(expandWireModel(check.Body, inst.WireModel)))
	if err != nil {
		return false
	}
	if check.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// expandWireModel substitutes ${WIRE_MODEL} with a JSON-escaped token so a
// weights path containing quotes or backslashes (Windows) can't break the
// body it is embedded in.
func expandWireModel(body, wireModel string) string {
	if body == "" || !strings.Contains(body, wireModelPlaceholder) {
		return body
	}
	escaped, err := json.Marshal(wireModel)
	if err != nil {
		return body
	}
	return strings.ReplaceAll(body, wireModelPlaceholder, string(escaped[1:len(escaped)-1]))
}

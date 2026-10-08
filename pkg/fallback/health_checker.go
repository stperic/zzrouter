package fallback

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"time"
)

const (
	defaultHealthPath     = "/health"
	defaultHealthInterval = 30 * time.Second
	defaultHealthTimeout  = 5 * time.Second

	// recoveryThreshold is the number of consecutive successes needed to mark
	// a deployment as healthy again (hysteresis to avoid flapping).
	recoveryThreshold = 2
)

// HealthTarget describes a deployment endpoint to probe.
type HealthTarget struct {
	DeploymentName string
	URL            string        // Base URL (e.g., "http://localhost:11434")
	Path           string        // Health check path (default "/health")
	Interval       time.Duration // Probe interval (default 30s)
	Timeout        time.Duration // Probe timeout (default 5s)
	Header         http.Header   // Credential the endpoint is sent, if it declares one
}

type deploymentHealth struct {
	healthy            bool
	consecutiveSuccess int
}

// HealthChecker runs background HTTP health probes for model group deployments.
// Unhealthy deployments are excluded before strategy selection (proactive routing).
type HealthChecker struct {
	mu       sync.RWMutex
	status   map[string]*deploymentHealth
	client   *http.Client
	targets  []HealthTarget
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	// wg tracks each owned probeLoop goroutine. Stop cancels the
	// context then blocks on wg.Wait so in-flight HTTP probes finish
	// (or are aborted by ctx) before Stop returns.
	wg sync.WaitGroup
	// emit is invoked outside the checker mutex on healthy⇄unhealthy
	// transitions. Optional — nil = no publish.
	emit func(replica, healthState string)
}

// SetEventEmitter wires a callback that fires on every healthy ⇄
// unhealthy transition for any tracked deployment. healthState is
// "healthy" or "unhealthy" (matching the schema enum). String-typed
// to avoid a back-import from pkg/fallback → pkg/observability/route_events.
func (h *HealthChecker) SetEventEmitter(emit func(replica, healthState string)) {
	h.mu.Lock()
	h.emit = emit
	h.mu.Unlock()
}

// NewHealthChecker creates a health checker. Call Start(ctx) to launch
// background probes.
func NewHealthChecker(targets []HealthTarget) *HealthChecker {
	h := &HealthChecker{
		status:  make(map[string]*deploymentHealth),
		targets: targets,
		client:  &http.Client{},
	}
	for _, t := range targets {
		h.status[t.DeploymentName] = &deploymentHealth{healthy: true, consecutiveSuccess: recoveryThreshold}
	}
	return h
}

// Start launches background probe goroutines. The provided ctx controls
// probe lifecycle — cancelling it stops all probes and tears down
// in-flight HTTP requests.
func (h *HealthChecker) Start(ctx context.Context) {
	h.ctx, h.cancel = context.WithCancel(ctx) //nolint:gosec // cancel stored on h.cancel and invoked by Stop
	for _, t := range h.targets {
		h.wg.Add(1)
		//nolint:contextcheck // probeLoop reads h.ctx, derived from Start's ctx above; reported on this line, so annotated here
		go func(target HealthTarget) {
			defer h.wg.Done()
			h.probeLoop(target)
		}(t)
	}
}

// IsHealthy returns whether a deployment is considered healthy.
// Unknown deployments (not configured for health checks) are assumed healthy.
func (h *HealthChecker) IsHealthy(deploymentName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	s, ok := h.status[deploymentName]
	if !ok {
		return true // Unknown = healthy (no health check configured)
	}
	return s.healthy
}

// HealthState returns the closed-enum tri-state for a deployment:
// "healthy", "unhealthy", or "unknown". Unknown means the deployment
// is not configured for health checks (IsHealthy still returns true
// for those — the bool flattens two distinct conditions, but the
// /state endpoint needs to surface them separately).
func (h *HealthChecker) HealthState(deploymentName string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	s, ok := h.status[deploymentName]
	if !ok {
		return "unknown"
	}
	if s.healthy {
		return "healthy"
	}
	return "unhealthy"
}

// Stop shuts down all background probe goroutines, cancels in-flight
// HTTP requests, and blocks until every owned probeLoop has exited.
// Safe to call multiple times.
func (h *HealthChecker) Stop() {
	h.stopOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
	})
	h.wg.Wait()
}

func (h *HealthChecker) probeLoop(target HealthTarget) {
	interval := target.Interval
	if interval <= 0 {
		interval = defaultHealthInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.probe(target)
		}
	}
}

func (h *HealthChecker) probe(target HealthTarget) {
	path := target.Path
	if path == "" {
		path = defaultHealthPath
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}

	url := target.URL + path

	ctx, cancel := context.WithTimeout(h.ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.markUnhealthy(target.DeploymentName)
		return
	}
	maps.Copy(req.Header, target.Header)

	resp, err := h.client.Do(req)
	if err != nil {
		h.markUnhealthy(target.DeploymentName)
		slog.Debug("Health check failed", "deployment", target.DeploymentName, "url", url, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		h.markSuccess(target.DeploymentName)
	} else {
		h.markUnhealthy(target.DeploymentName)
		slog.Debug("Health check unhealthy", "deployment", target.DeploymentName, "url", url, "status", resp.StatusCode)
	}
}

func (h *HealthChecker) markSuccess(name string) {
	h.mu.Lock()
	s, ok := h.status[name]
	if !ok {
		h.mu.Unlock()
		return
	}
	s.consecutiveSuccess++
	transitioned := false
	if s.consecutiveSuccess >= recoveryThreshold && !s.healthy {
		slog.Info("Health check recovered", "deployment", name)
		s.healthy = true
		transitioned = true
	} else if s.consecutiveSuccess >= recoveryThreshold {
		s.healthy = true
	}
	emit := h.emit
	h.mu.Unlock()
	if transitioned && emit != nil {
		emit(name, "healthy")
	}
}

func (h *HealthChecker) markUnhealthy(name string) {
	h.mu.Lock()
	s, ok := h.status[name]
	if !ok {
		h.mu.Unlock()
		return
	}
	s.consecutiveSuccess = 0
	transitioned := false
	if s.healthy {
		slog.Warn("Health check failed, marking unhealthy", "deployment", name)
		transitioned = true
	}
	s.healthy = false
	emit := h.emit
	h.mu.Unlock()
	if transitioned && emit != nil {
		emit(name, "unhealthy")
	}
}

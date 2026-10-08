package mesh

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ReconnectionCallback is called when a host transitions into StatusUp
// after a probe (a fresh or recovered connection). Not called for
// UP→DEGRADED or any other transition.
//
// The construction-time probe in NewCluster is the one path that does
// not call it inline — the cluster it would hand back does not exist
// yet. Those transitions are held and delivered by
// (*Cluster).ReplayStartupReconnects, so a peer that was already up at
// boot still gets exactly one call.
type ReconnectionCallback func(hostURL string)

// NameObserver is called with a peer's own node name each time a probe
// resolves one. Fires on every successful probe, not only on a status
// transition, because the name is learned by the FIRST probe — including
// the startup sweep, which produces no transition to hang a callback on.
// Implementations must be cheap and idempotent; the common call reports
// a name that is already on file.
type NameObserver func(hostURL, nodeName string)

// HealthMonitor monitors cluster host health. Liveness hysteresis and
// status mutation live on the registry; this type only schedules probes
// and reacts to the Transition returned by registry.Observe.
//
// Lifecycle: the monitor owns a single derived-from-background context.
// Start launches the monitor goroutine; Stop cancels the context and
// waits for in-flight probes to drain. All per-probe contexts derive
// from hm.ctx so Stop propagates cancellation to ad-hoc Refresh calls
// as well as the ticker-driven checkAllNodes fan-out.
type HealthMonitor struct {
	connector            *Connector
	registry             *EndpointRegistry
	circuitBreakers      *CircuitBreakerManager // unused here post UP-skip removal; breakers are driven by dispatch in strategies.go
	checkInterval        time.Duration
	reconnectionCallback ReconnectionCallback
	nameObserver         NameObserver
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
}

// HealthMonitorConfig holds health monitor configuration
type HealthMonitorConfig struct {
	Connector            *Connector
	Registry             *EndpointRegistry
	CircuitBreakers      *CircuitBreakerManager
	CheckInterval        time.Duration
	ReconnectionCallback ReconnectionCallback
	NameObserver         NameObserver
}

// NewHealthMonitor creates a new health monitor
func NewHealthMonitor(config *HealthMonitorConfig) *HealthMonitor {
	if config.CheckInterval == 0 {
		config.CheckInterval = constants.HealthCheckInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &HealthMonitor{
		connector:            config.Connector,
		registry:             config.Registry,
		circuitBreakers:      config.CircuitBreakers,
		checkInterval:        config.CheckInterval,
		reconnectionCallback: config.ReconnectionCallback,
		nameObserver:         config.NameObserver,
		ctx:                  ctx,
		cancel:               cancel,
	}
}

// Start launches the monitor's periodic probe goroutine.
func (hm *HealthMonitor) Start() {
	hm.wg.Add(1)
	go hm.monitorLoop()
}

// monitorLoop periodically probes cluster endpoints. Recovers from panics
// so one bad probe cannot permanently stop monitoring.
func (hm *HealthMonitor) monitorLoop() {
	defer hm.wg.Done()

	ticker := time.NewTicker(hm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			hm.safeCheckAllNodes()
		case <-hm.ctx.Done():
			return
		}
	}
}

func (hm *HealthMonitor) safeCheckAllNodes() {
	defer utils.RecoverAndLog("mesh.HealthMonitor.checkAllNodes")
	hm.checkAllNodes()
}

// checkAllNodes probes every non-local, non-circuit-open endpoint on each
// tick. UP-skip was removed so polling acts as the correctness floor for
// dropped-notify cases: a worker that silently mutates state still gets
// reconciled within HealthCheckInterval. Cost at small clusters (N≤10)
// is one HTTP probe per worker per cadence — negligible.
func (hm *HealthMonitor) checkAllNodes() {
	endpoints := hm.registry.GetAllEndpoints()
	if len(endpoints) == 0 {
		return
	}

	var toCheck []*Endpoint
	for _, ep := range endpoints {
		if ep.IsLocal {
			continue
		}
		toCheck = append(toCheck, ep)
	}
	if len(toCheck) == 0 {
		return
	}

	hm.fanOutProbes(toCheck, hm.checkInterval)
}

// fanOutProbes runs probe on each target URL concurrently with
// perProbeTimeout and blocks until all probes return. Per-probe contexts
// derive from hm.ctx so Stop cancels the fan-out; hm.wg also accounts
// for each goroutine so Stop's wait sees them drain even if the caller
// is no longer there.
func (hm *HealthMonitor) fanOutProbes(targets []*Endpoint, perProbeTimeout time.Duration) {
	var local sync.WaitGroup
	for _, ep := range targets {
		url := ep.URL
		local.Add(1)
		hm.wg.Add(1)
		go func() {
			defer local.Done()
			defer hm.wg.Done()
			defer utils.RecoverAndLog("mesh.HealthMonitor.probe")
			ctx, cancel := context.WithTimeout(hm.ctx, perProbeTimeout)
			defer cancel()
			hm.probe(ctx, url)
		}()
	}
	local.Wait()
}

// probe runs one health probe against url, feeds the result into the
// registry's liveness state machine, and acts on the resulting transition.
func (hm *HealthMonitor) probe(ctx context.Context, url string) {
	conn, err := hm.connector.ConnectToClusterNode(ctx, url)

	// If the monitor is shutting down, ctx cancellation will surface as
	// the probe error — don't attribute shutdown to the peer by driving
	// the liveness machine toward DOWN.
	if err != nil && ctx.Err() != nil {
		return
	}

	var result ProbeResult
	if err != nil {
		result = ProbeResult{OK: false, Err: err}
	} else {
		result = ProbeResult{OK: true, Quality: conn.Quality, Snapshot: conn}
		if hm.nameObserver != nil && conn.NodeName != "" {
			hm.nameObserver(url, conn.NodeName)
		}
	}

	t := hm.registry.Observe(url, result)
	if t.IsZero() {
		return
	}

	switch {
	case t.Err != nil:
		log.Printf("cluster endpoint %s %s->%s after %s: %v",
			url, t.From, t.To, t.DurationInFrom.Round(time.Millisecond), t.Err)
	case t.Snapshot != nil && t.Snapshot.Version != nil:
		log.Printf("cluster endpoint %s %s->%s after %s (node=%s version=%s)",
			url, t.From, t.To, t.DurationInFrom.Round(time.Millisecond),
			t.Snapshot.NodeName, t.Snapshot.Version.String())
	default:
		log.Printf("cluster endpoint %s %s->%s after %s",
			url, t.From, t.To, t.DurationInFrom.Round(time.Millisecond))
	}

	if t.To == StatusUp && hm.reconnectionCallback != nil {
		hm.reconnectionCallback(url)
	}
}

// Refresh synchronously re-probes the given endpoints (skipping local
// ones), regardless of current status. Used for ?refresh=true and for
// operator-triggered single-host refresh. An empty urls argument means
// "all registered endpoints". Per-probe contexts derive from hm.ctx,
// so Stop mid-Refresh cancels the in-flight probes and unblocks us.
func (hm *HealthMonitor) Refresh(urls ...string) {
	// Refresh-after-Stop would silently re-arm hm.wg for fail-fast
	// goroutines and break the "Stop means drained" invariant for
	// any caller that observed Stop completion. Guard at entry.
	if hm.ctx.Err() != nil {
		return
	}
	var targets []*Endpoint
	if len(urls) == 0 {
		for _, ep := range hm.registry.GetAllEndpoints() {
			if !ep.IsLocal {
				targets = append(targets, ep)
			}
		}
	} else {
		targets = make([]*Endpoint, 0, len(urls))
		for _, url := range urls {
			ep, err := hm.registry.GetEndpointByURL(url)
			if err != nil || ep.IsLocal {
				continue
			}
			targets = append(targets, ep)
		}
	}
	if len(targets) == 0 {
		return
	}
	hm.fanOutProbes(targets, constants.ClusterHealthCheckTimeout)
}

// Stop cancels the monitor's context (propagating to in-flight probes)
// and waits for every goroutine it spawned to return. Idempotent:
// context.CancelFunc collapses second calls into no-ops and
// WaitGroup.Wait on a zero counter returns immediately.
func (hm *HealthMonitor) Stop() {
	hm.cancel()
	hm.wg.Wait()
}

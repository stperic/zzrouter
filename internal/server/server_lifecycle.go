// Server lifecycle — Start, Stop, maintenance, and startup helpers.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/gpu"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/update"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// Start starts subsystems and serves HTTP. Blocks until shutdown.
// ctx is the caller's lifecycle context — cancelling it flows through
// to every subsystem that honors the shutdown signal (pricing refresh,
// cache warmers, cluster node, role manager). Cancelling ctx is
// equivalent to server shutdown for background goroutines; Stop itself
// additionally drains HTTP and runs the Stop-group helpers in reverse.
//
// The coordinator-group mode check lives here (not inside the group
// helper) so the role subscriber can call startCoordinatorSubsystems
// directly without re-acquiring role.Manager's mutex and deadlocking.
func (s *Server) Start(ctx context.Context) error {
	s.startTime = utils.Now()
	// Replace the Background-rooted shutdownCtx set at construction
	// with a child of the caller's ctx so cancellation flows in.
	if s.shutdownCancel != nil {
		s.shutdownCancel()
	}
	s.shutdownCtx, s.shutdownCancel = context.WithCancel(ctx)

	if err := s.startCommonSubsystems(); err != nil {
		return err
	}
	if s.role.Current().IsCoordinator() {
		//nolint:contextcheck // subsystems live for the node's lifetime and stop via s.shutdownCtx, not this call's ctx
		if err := s.startCoordinatorSubsystems(); err != nil {
			return err
		}
	}
	// Workers ping the coordinator on boot so its endpoint snapshot
	// reflects live state immediately instead of waiting on the 30s
	// health poll. Fires once the public listener is bound (before
	// Serve accepts traffic) — no self-probe, no scheme guessing.
	if s.role.Current().IsWorker() && s.cluster.signaler != nil {
		//nolint:contextcheck // boot ping is fire-and-forget; the select below already honours s.shutdownCtx
		go func() {
			select {
			case <-s.listenerReady:
			case <-s.shutdownCtx.Done():
				return
			}
			s.cluster.signaler.NotifyCacheRefresh()
		}()
	}

	s.logNodeStart()
	return s.startServing()
}

// Stop mirrors Start in reverse.
func (s *Server) Stop() error {
	slog.Info("Stopping zzrouter server gracefully")

	ctx, cancel := context.WithTimeout(context.Background(), constants.ShutdownMaxTimeout)
	defer cancel()

	s.draining.Store(true)

	// Pre-drain spend state BEFORE HTTP shutdown. tracker.save() is a
	// small atomic temp+rename (ms-scale), so draining it here guarantees
	// the most-recent spend is on disk even if stopServing burns the
	// remaining ctx budget. In-flight requests that settle after this
	// point still dirty the tracker; the final save() inside
	// access.Stop below picks them up under normal shutdown. The
	// inference log is an in-memory ring buffer with no disk
	// persistence, so a handful of settlements between pre-drain and
	// SIGKILL are lost — acceptable for budget estimation, not material
	// for billing at current volumes.
	if s.access != nil {
		s.access.FlushSpend()
	}

	httpErr := s.stopServing(ctx)

	// Workers send a synchronous goodbye to the coordinator so it
	// marks this worker's endpoint DOWN immediately, bypassing the
	// 2-consecutive-miss liveness hysteresis that would otherwise
	// delay DOWN detection until the next health-poll tick. Capped
	// internally at 3s; failures revert to normal poll-based
	// detection. Cache is still alive here — it stops in
	// stopCommonSubsystems below.
	if s.role.Current().IsWorker() && s.cluster.signaler != nil {
		s.cluster.signaler.SendGoodbye(ctx)
	}

	if s.shutdownCancel != nil {
		s.shutdownCancel()
	}

	var coordStopErr error
	if s.role.Current().IsCoordinator() {
		coordStopErr = s.stopCoordinatorSubsystems(ctx)
	}

	// Drain async endpoint refreshes (HealthMonitor.Stop above unblocks them).
	s.refreshWG.Wait()
	stopErr := errors.Join(coordStopErr, s.stopCommonSubsystems(ctx))

	slog.Info("Node stopped gracefully")
	return errors.Join(httpErr, stopErr)
}

// startServing binds the HTTP listener, signals listenerReady, and
// blocks serving until shutdown. Splitting Listen from Serve lets
// worker boot-notify wait on listenerReady instead of polling
// /health — no scheme guessing, no TLS self-probe.
func (s *Server) startServing() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.httpServer.Addr, err)
	}
	close(s.listenerReady)
	s.warnIfCompatOpenToNetwork(ln.Addr())
	if s.config.Node.IsTLSEnabled() {
		slog.Info("Starting HTTPS server", "addr", s.httpServer.Addr, "cert", s.config.Node.TLSCert)
		err = s.httpServer.ServeTLS(ln, s.config.Node.TLSCert, s.config.Node.TLSKey)
	} else {
		err = s.httpServer.Serve(ln)
	}
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// warnIfCompatOpenToNetwork says so at startup when anyone who can reach
// addr may run local models without a key: the default, and easy to miss.
func (s *Server) warnIfCompatOpenToNetwork(addr net.Addr) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP.IsLoopback() || s.config.Cluster.IsWorker() || anonymousRefusal(s.config, s.access) != "" {
		return
	}
	slog.Warn("inference routes (/v1/*, /api/*) answer requests without a key on a network address; "+
		"set auth.require_compat_auth: true in node.yaml and give clients virtual keys, or bind to 127.0.0.1",
		"addr", addr.String())
}

// stopServing drains in-flight HTTP requests.
func (s *Server) stopServing(ctx context.Context) error {
	err := s.httpServer.Shutdown(ctx)
	if err != nil {
		slog.Error("HTTP shutdown error (will force close)", "error", err)
		if closeErr := s.httpServer.Close(); closeErr != nil {
			slog.Error("Error force-closing HTTP server", "error", closeErr)
		}
	}
	return err
}

// logNodeStart logs server startup with cluster mode in one consolidated line.
func (s *Server) logNodeStart() {
	addr := s.httpServer.Addr

	var modeStr string
	if s.config.Cluster.IsMaster() {
		configuredWorkers := len(s.config.Cluster.Endpoints)
		if configuredWorkers == 0 {
			modeStr = "master node (standalone)"
		} else {
			connectedWorkers := 0
			if s.cluster.coordinator != nil {
				endpoints := s.cluster.coordinator.GetState().GetEndpoints().GetAllEndpoints()
				for _, ep := range endpoints {
					if !ep.IsLocal && ep.Status == mesh.StatusUp {
						connectedWorkers++
					}
				}
			}

			if connectedWorkers == configuredWorkers {
				modeStr = fmt.Sprintf("master node + %d worker node%s",
					connectedWorkers,
					map[bool]string{true: "", false: "s"}[connectedWorkers == 1])
			} else {
				modeStr = fmt.Sprintf("master node + %d/%d worker nodes connected)",
					connectedWorkers, configuredWorkers)
			}
		}
	} else if s.config.Cluster.IsWorker() {
		modeStr = "worker node"
	} else {
		modeStr = "standalone"
	}

	slog.Info("zzRouter server started", "addr", addr, "mode", modeStr)
}

// hourlyMaintenanceEveryNTicks gates passes that should run on an hourly
// cadence against the MaintenanceInterval tick — currently 4 × 15min.
const hourlyMaintenanceEveryNTicks = 4

// maintenanceLoop runs periodic cleanup tasks behind Start/Stop lifecycle.
type maintenanceLoop struct {
	tick      func(tickCount int) // cleanup callback
	stopCh    chan struct{}
	done      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	started   bool // set under startOnce
}

// newMaintenanceLoop creates a loop that calls tick on every interval.
func newMaintenanceLoop(tick func(int)) *maintenanceLoop {
	return &maintenanceLoop{
		tick:   tick,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start launches the background goroutine. Fire-and-forget — degraded-ok.
// Calling Start twice is a no-op.
func (ml *maintenanceLoop) Start(_ context.Context) {
	if ml == nil {
		return
	}
	ml.startOnce.Do(func() {
		ml.started = true
		go ml.run()
	})
}

// Stop signals the loop to exit and waits for it to finish. Safe to
// call multiple times; safe to call if Start was never invoked.
func (ml *maintenanceLoop) Stop() {
	if ml == nil {
		return
	}
	ml.stopOnce.Do(func() {
		close(ml.stopCh)
	})
	if ml.started {
		<-ml.done
	}
}

func (ml *maintenanceLoop) run() {
	defer close(ml.done)
	ticker := time.NewTicker(constants.MaintenanceInterval)
	defer ticker.Stop()

	tickCount := 0
	for {
		select {
		case <-ml.stopCh:
			return
		case <-ticker.C:
			tickCount++
			ml.tick(tickCount)
		}
	}
}

// runMaintenance performs all periodic cleanup tasks. tickCount is a
// monotonic counter (one per maintenance tick) used to gate heavier
// passes to a coarser cadence.
func (s *Server) runMaintenance(tickCount int) {
	if s.services.Deployments != nil {
		s.services.Deployments.GetTracker().CleanCompletedDeployments(1 * time.Hour)
	}
	if s.model.Downloads != nil {
		s.model.Downloads.CleanCompleted()
	}
	if s.providers.appMgr != nil {
		s.providers.appMgr.Instances().CleanupExpiredFailures()
	}
	if s.cluster.coordinator != nil {
		dt := s.cluster.coordinator.GetState().GetDownloadTracker()
		dt.CleanCompleted()
		dt.CleanOld(1 * time.Hour)
	}
	if s.providers.appMgr != nil {
		lm := s.providers.appMgr.LogManager()
		_ = lm.CleanupOldLogs()
		// Size-based mid-run rotation for active provider-instance logs
		// runs hourly. Copy-truncate preserves the child's inherited
		// O_APPEND fd. Node log ("zzrouter-" prefix) is owned by
		// lumberjack and must be skipped here.
		if tickCount%hourlyMaintenanceEveryNTicks == 0 {
			_ = lm.RotateOversizedCopyTruncate("zzrouter-")
		}
	}
	if s.cluster.coordinator != nil {
		s.cluster.coordinator.GetState().GetCircuitBreakers().PruneStale(1 * time.Hour)
		// Re-push the provider tree to every worker. The interesting
		// case is the one no event covers: a config.yaml edited on disk
		// while this node was stopped is not a change, so nothing fires
		// and the workers keep the old bytes. Every tick rather than a
		// coarser gate because the push is a byte comparison on the
		// receiving end when nothing differs, and a worker running the
		// wrong provider config is a silent wrong answer, not a slow one.
		s.reconcileProviderTree("maintenance tick")
	}
}

// ---------------------------------------------------------------------------
// Mode-aware subsystem groups
// ---------------------------------------------------------------------------

// handleRoleTransition is the role.Manager subscriber. It runs
// synchronously under role.Manager's transition mutex; concurrent
// Set callers are serialized on the mutex and observe the state
// after this returns.
//
// Promote (→ Coordinator): invokes startCoordinatorSubsystems with
// s.shutdownCtx — coordinator subsystem goroutines are bound to the
// server's lifetime, not the role's. Demote re-Stops them via their
// restartable lifecycles.
//
// Demote (→ non-Coordinator): invokes stopCoordinatorSubsystems with
// a Background-derived ctx carrying the standard shutdown drain
// timeout. Transition.Reason is logged for audit.
//
// Start errors on promote are logged, not panicked — a failed
// promote leaves the node in a partially-started state, which is
// surfaced on the next readiness probe. Future role-transition
// readiness would return the error to the Set caller; v1 logs.
func (s *Server) handleRoleTransition(t role.Transition) {
	slog.Info("cluster role transition",
		"from", t.From, "to", t.To, "reason", t.Reason)

	switch {
	case t.To.IsCoordinator() && !t.From.IsCoordinator():
		if err := s.startCoordinatorSubsystems(); err != nil {
			slog.Error("promote failed", "error", err)
		}
	case !t.To.IsCoordinator() && t.From.IsCoordinator():
		ctx, cancel := context.WithTimeout(context.Background(), constants.RoleDemoteTimeout)
		defer cancel()
		if err := s.stopCoordinatorSubsystems(ctx); err != nil {
			slog.Error("demote failed", "error", err)
		}
	}
}

// startCoordinatorSubsystems starts subsystems that only run on
// coordinator or standalone nodes. Returns the joined error from any
// subsystem whose Start can fail; today none do, but the signature is
// the extension point for role-transition readiness errors.
//
// Callers MUST check s.role.Current().IsCoordinator() before invoking.
// The gate lives in Server.Start and handleRoleTransition, not here,
// because the subscriber runs under role.Manager's mutex and cannot
// safely call role.Current() again (deadlock).
func (s *Server) startCoordinatorSubsystems() error {
	s.openAuditSink()
	s.access.Start(s.shutdownCtx)
	s.providers.fallback.Start(s.shutdownCtx)
	if s.model.Groups != nil {
		// Ephemeral-route reaper. Coordinator-only because the
		// model-groups mutator surface lives on the coord. The
		// reaper runs under its own cancel so role-demote stops it
		// without waiting for the global shutdownCtx — otherwise a
		// re-promote would stack a second reaper on the same store.
		reaperCtx, cancel := context.WithCancel(s.shutdownCtx) //nolint:gosec // The lifecycle owner stores and calls cancel during teardown.
		s.providers.groupReaperCancel = cancel
		s.providers.groupReaperDone = s.model.Groups.StartReaper(reaperCtx, modelgroup.DefaultReaperInterval)
	}
	s.services.Search.Start(s.shutdownCtx)
	s.services.Discovery.Start() // sync.Once-deduped against the common-subsystem warm; kept for Start/Stop symmetry with Discovery.Stop in stopCoordinatorSubsystems
	s.inference.affinity.Start()
	s.mcpGateway.Start(s.shutdownCtx)
	s.model.Pricing.Start(s.shutdownCtx)
	// Seed the self-slot so /health + ListApps hit it without waiting
	// for the first config mutation to fire publishSelf.
	s.publishSelfSnapshot()
	// Deliver the reconnection callback for every peer that was already
	// up when the mesh probed during construction. That probe is where
	// boot convergence has to come from — a config.yaml edited on disk
	// while this node was stopped loads as the new truth without ever
	// being a change, so no listener fires and every worker keeps the
	// old bytes — but the constructor could not run the callback
	// itself, so it held those transitions for here. The mTLS listener
	// started in startCommonSubsystems, so there is something to push
	// with.
	if s.cluster.coordinator != nil {
		s.cluster.coordinator.ReplayStartupReconnects()
	}
	return nil
}

// openAuditSink opens the JSONL audit log and pushes it into any
// services that were constructed during route registration. Idempotent
// — calling twice without an intervening close is a no-op after the
// first call. Runs at every coordinator-subsystem start so a node
// that transitions Unclaimed → Coordinator via pairing picks up
// audit coverage.
func (s *Server) openAuditSink() {
	if _, ok := s.auditSink.(audit.Null); !ok {
		return // already opened
	}
	if s.auditConfigDir == "" {
		return
	}
	path := filepath.Join(s.auditConfigDir, "audit.jsonl")
	sink, err := audit.NewFileSink(path)
	if err != nil {
		slog.Warn("audit: open failed; mutations will not be audited", "path", path, "error", err)
		return
	}
	s.auditSink = sink
	if s.services.Keys != nil {
		s.services.Keys.SetAuditSink(sink)
	}
	if s.services.Teams != nil {
		s.services.Teams.SetAuditSink(sink)
	}
	slog.Info("audit log opened", "path", path)
}

// closeAuditSink closes the audit sink (if open) and resets the
// services' sink to audit.Null. Paired with openAuditSink across a
// role-transition cycle.
func (s *Server) closeAuditSink() {
	if s.services.Keys != nil {
		s.services.Keys.SetAuditSink(audit.Null{})
	}
	if s.services.Teams != nil {
		s.services.Teams.SetAuditSink(audit.Null{})
	}
	if closer, ok := s.auditSink.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			slog.Warn("audit sink close failed", "error", err)
		}
	}
	s.auditSink = audit.Null{}
}

// startCommonSubsystems starts lifecycle-managed subsystems that run on
// every node regardless of cluster mode.
func (s *Server) startCommonSubsystems() error {
	if err := s.role.Start(s.shutdownCtx); err != nil {
		return fmt.Errorf("start role manager: %w", err)
	}
	if err := s.cluster.node.Start(s.shutdownCtx); err != nil {
		return fmt.Errorf("start cluster node: %w", err)
	}
	// Cluster HTTP subsystem. Common group (not coordinator) because
	// the listener serves Coordinator + Unclaimed + Worker; only
	// Disabled skips, and the Node internally no-ops Start when in
	// Disabled mode. Single-use by contract — one Start per Server
	// lifetime; Disabled↔non-Disabled config flips already require a
	// process restart, which aligns.
	if err := s.cluster.listener.Start(s.shutdownCtx); err != nil {
		return fmt.Errorf("start cluster listener: %w", err)
	}
	// Validate coord-only AdvertiseURL at boot so an operator who
	// misconfigures it finds out here, not at the first pairing or —
	// worse — at the first renewal 15 days after a successful pair.
	if s.config.Cluster.IsCoordinator() && s.config.Cluster.AdvertiseURL != "" {
		if err := clusternode.ValidateAdvertiseURL(s.config.Cluster.AdvertiseURL); err != nil {
			return fmt.Errorf("cluster.advertise_url: %w", err)
		}
	}
	s.updateScheduler = update.NewScheduler(&s.config.Update, clock.System(),
		update.WithJobsRegistry(s.jobs),
	)
	s.updateScheduler.Start(s.shutdownCtx)
	if s.nodeConfigStore != nil {
		s.nodeConfigStore.OnChange("updateScheduling", s.reconcileUpdateScheduling)
	}
	if s.node.IsCoordinator() && s.nodeConfigStore != nil {
		var err error
		s.updateRollouts, err = update.NewRollouts(filepath.Join(filepath.Dir(s.nodeConfigStore.FilePath()), "update-rollouts.json"), s.node.Name(), clusterUpdates{s}, s.jobs, clock.System())
		if err != nil {
			return err
		}
		if err = s.updateRollouts.Start(s.shutdownCtx); err != nil {
			return err
		}
	}
	s.providers.appMgr.Start(s.shutdownCtx)
	s.providers.cooldowns.Start()
	s.otelProvider.Start(s.shutdownCtx)
	// Warm the GPU inventory on every node — workers populate the
	// gpus/gpu_count fields on /zzrouter/v1/internal/health that the
	// coordinator's mesh probe parses into Endpoint.Snapshot. Without
	// this the cluster registry shows VRAM as "-" for every worker
	// because gpu.List(false) returns an empty Inventory until the
	// async probe finishes. Coordinator-only since c58ba412 was a
	// regression — restore role-agnostic warming here so the probe
	// actually has data to serve.
	gpu.StartAsync()
	// Cache.Start runs on every mode — coord warms/serves the model
	// catalog, worker holds the lifecycle for async refreshes.
	if s.model.Cache != nil {
		s.model.Cache.Start(s.shutdownCtx)
	}
	// Filesystem-watcher invalidation: pair the cache lifetime with an
	// fsnotify watch so out-of-band rm against the models root flips
	// the catalog truthful. Errors are logged + ignored — the periodic
	// refresh remains the safety net.
	if s.model.Cache != nil {
		root, err := modelregistry.GetModelsRootDir()
		if err == nil {
			w, werr := newModelsWatcher(root, s.model.Cache.Invalidate)
			if werr != nil {
				slog.Warn("models watcher: init failed", "root", root, "error", werr)
			} else if w != nil {
				s.modelsWatcher = w
				s.modelsWatcher.Start(s.shutdownCtx)
			}
		}
	}
	// Signaler.Start tracks worker→coord notify goroutines so Stop
	// can drain them cleanly.
	if s.cluster.signaler != nil {
		s.cluster.signaler.Start(s.shutdownCtx)
	}
	s.maintenance = newMaintenanceLoop(s.runMaintenance)
	s.maintenance.Start(s.shutdownCtx)
	return nil
}

// stopCoordinatorSubsystems stops coordinator-only subsystems. Order
// is the reverse of startCoordinatorSubsystems — subsystems stop in
// LIFO so a consumer is never torn down before its dependencies.
//
// Callers (Server.Stop, handleRoleTransition on demote) decide when
// to invoke. No internal role check — the idempotent-Stop convention
// makes every Stop a safe no-op when the paired Start never ran,
// and moving the gate to callers avoids the role.Manager mutex
// deadlock the subscriber would otherwise hit.
//
// Returns error for signature symmetry with startCoordinatorSubsystems
// and to keep the door open for stop-failure aggregation; today every
// leaf Stop is best-effort.
//
//nolint:unparam // symmetric with startCoordinatorSubsystems; reserved for stop-failure aggregation
func (s *Server) stopCoordinatorSubsystems(ctx context.Context) error {
	s.model.Pricing.Stop()
	s.mcpGateway.Stop()
	s.inference.affinity.Stop()
	s.services.Discovery.Stop()
	s.services.Search.Stop(ctx)
	s.providers.fallback.Stop(ctx)
	if cancel := s.providers.groupReaperCancel; cancel != nil {
		// Signal the reaper to exit (independent of shutdownCtx so
		// role-demote works) and wait synchronously for it to
		// return. ctx deadline bounds the wait; a stuck reaper drops
		// to a warn.
		cancel()
		done := s.providers.groupReaperDone
		select {
		case <-done:
		case <-ctx.Done():
			slog.Warn("ephemeral-route reaper did not stop before shutdown deadline")
		}
		s.providers.groupReaperCancel = nil
		s.providers.groupReaperDone = nil
	}
	// Flush key/team stores before access.Stop. Service-layer mutations
	// already Save synchronously; this is defense-in-depth for any
	// in-memory-only fields (VirtualKey.LastSeenAt is the current
	// example — declared as "hot in-memory, persisted on save cycle").
	// Skip when the store is empty so fresh-install hosts don't get a
	// surprise keys.yaml/teams.yaml written on shutdown — the factory's
	// os.ErrNotExist fast-path at server_factory.go:311 depends on that
	// file staying absent until the first mutation creates it. Errors
	// are logged and swallowed: a shutdown-time flush that fails is
	// still better than a crash that would have lost the state too.
	if s.keyStore != nil && len(s.keyStore.List()) > 0 {
		if err := s.keyStore.Save(); err != nil {
			slog.Warn("shutdown flush of keys.yaml failed", "error", err)
		}
	}
	if s.teamStore != nil && len(s.teamStore.List()) > 0 {
		if err := s.teamStore.Save(); err != nil {
			slog.Warn("shutdown flush of teams.yaml failed", "error", err)
		}
	}
	s.closeAuditSink()
	s.access.Stop(ctx)
	return nil
}

// stopCommonSubsystems stops subsystems that run on every node.
// ctx carries the shutdown deadline for subsystems that need graceful drain.
// Returns the joined error from subsystems whose Stop can fail; others
// are swallowed silently (they already log internally on failure paths).
func (s *Server) stopCommonSubsystems(ctx context.Context) error {
	var errs []error
	// Provider processes first — they're child processes that must be
	// stopped before their parent goroutines are gone.
	if err := s.providers.appMgr.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("provider app manager: %w", err))
	}
	s.maintenance.Stop()
	if s.updateRollouts != nil {
		s.updateRollouts.Stop()
	}
	if s.updateScheduler != nil {
		s.updateScheduler.Stop()
	}
	// Cluster listener stops before the resource group so /internal/*
	// dispatches (wired in mTLS PR 3) stop arriving before the
	// ResourceTracker + mDNS state those handlers consume is torn
	// down. Today the listener's routes are self-contained and either
	// order works; the ordering is chosen to be stable across the PR 3
	// landing. Node.Stop honors the caller's ctx (capped internally at
	// 5s) so Server.Stop's outer shutdown deadline bounds the drain.
	if err := s.cluster.listener.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("cluster listener: %w", err))
	}
	if err := s.cluster.node.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("cluster node: %w", err))
	}
	if s.cluster.signaler != nil {
		s.cluster.signaler.Stop(ctx)
	}
	if s.modelsWatcher != nil {
		s.modelsWatcher.Stop()
	}
	if s.model.Cache != nil {
		s.model.Cache.Stop(ctx)
	}
	s.inference.logStore.Stop()
	if s.jobs != nil {
		s.jobs.Stop()
	}
	s.model.Downloads.Stop()
	s.providers.cooldowns.Stop()
	if err := s.otelProvider.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("opentelemetry: %w", err))
	}
	if err := s.role.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("role manager: %w", err))
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Free functions — startup helpers and middleware
// ---------------------------------------------------------------------------

// getOutboundIP detects the preferred outbound IP address.
func getOutboundIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer func() { _ = conn.Close() }()
		if localAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return localAddr.IP.String(), nil
		}
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("failed to get network interfaces: %w", err)
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
}

// requestSizeLimitMiddleware limits request body size (memory exhaustion guard).
func requestSizeLimitMiddleware(maxSize int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize)
		}
		c.Next()
	}
}

// requestBodyReadDeadlineMiddleware installs a per-connection read deadline (Slowloris guard).
func requestBodyReadDeadlineMiddleware(deadline time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		_ = http.NewResponseController(c.Writer).SetReadDeadline(utils.Now().Add(deadline))
		c.Next()
	}
}

// customLogger returns a Gin logger middleware that normalizes IPv6 localhost to IPv4.
func customLogger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		clientAddr := param.Request.RemoteAddr
		if after, ok := strings.CutPrefix(clientAddr, "[::1]:"); ok {
			clientAddr = "127.0.0.1:" + after
		} else if clientAddr == "::1" {
			clientAddr = "127.0.0.1"
		}
		return fmt.Sprintf("%v [GIN] %3d | %13v | %21s | %-7s %s\n",
			param.TimeStamp.Format("2006/01/02 15:04:05"),
			param.StatusCode,
			param.Latency,
			clientAddr,
			param.Method,
			param.Path,
		)
	})
}

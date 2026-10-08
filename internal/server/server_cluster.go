package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/routing"
)

// buildRouterForMode constructs a concrete Router matching the given mode.
// Worker nodes get LocalOnlyRouter; everything else (standalone, coordinator)
// gets ClusterAwareRouter.
func (s *Server) buildRouterForMode(isWorker bool, serverPort string) routing.Router {
	httpClient := s.connManager.GetHTTPClient()
	if isWorker {
		slog.Info("Router: LocalOnlyRouter", "node", s.config.Node.Name)
		return routing.NewLocalOnlyRouterWithClient(s.config.Node.Name, serverPort, s.ServeLocalRequest, httpClient)
	}
	slog.Info("Router: ClusterAwareRouter", "node", s.config.Node.Name)
	return routing.NewClusterAwareRouterWithClient(s.config.Node.Name, s.getClusterClient(), serverPort, s.ServeLocalRequest, httpClient)
}

// handleClusterModeReload flips s.role when mesh.mode in node.yaml
// changes. Registered as a NodeConfigStore listener alongside
// reconfigureRouter; ordering is independent because s.cluster.router
// is wrapped in routing.Swappable (subsystems read it at call time,
// not at construction).
//
// Config reload is the one non-mTLS Set trigger that exercises the
// role subscriber end-to-end pre-mTLS-PR-3. Pairing/renewal/revocation
// handlers, when they land, drop their own Set calls into the
// existing machinery — no new plumbing.
//
// Reload contract (docs/plan_reload_semantics.md):
//   - no role manager wired → Ignored (partial bootstrap).
//   - target role == current role → Ignored (no-op mutation).
//   - role.Set fails → Rejected.
//   - role.Set succeeds → Applied.
func (s *Server) handleClusterModeReload(cfg *pkgConfig.NodeConfig) pkgConfig.ReloadDisposition {
	if s.role == nil {
		return pkgConfig.DispositionIgnored
	}
	paths := clusternode.PathsFromConfigDir(pkgConfig.Paths().GetConfigDir())
	next := role.RoleFromConfig(cfg.Cluster, clusternode.IsPaired(paths))
	if next == s.role.Current() {
		return pkgConfig.DispositionIgnored
	}
	if err := s.role.Set(context.Background(), next, "config reload: mesh.mode="+string(cfg.Cluster.Mode)); err != nil {
		slog.Error("role.Set from config reload failed", "target", next, "error", err)
		return pkgConfig.DispositionRejected
	}
	return pkgConfig.DispositionApplied
}

// reconfigureRouter rebuilds the active router if the cluster mode transitioned
// between worker and non-worker. Called as a NodeConfigStore listener so the
// router follows runtime join/leave without restart.
//
// Transitions between disabled ↔ coordinator are no-ops because both use
// ClusterAwareRouter; only the worker boundary changes the router type.
//
// Reload contract (docs/plan_reload_semantics.md):
//   - routerSwap not wired → Ignored (partial bootstrap).
//   - worker boundary unchanged → Ignored.
//   - worker boundary crossed → Applied after swap.
func (s *Server) reconfigureRouter(cfg *pkgConfig.NodeConfig) pkgConfig.ReloadDisposition {
	if s.cluster.routerSwap == nil {
		return pkgConfig.DispositionIgnored
	}
	// Detect current side of the worker boundary by concrete type. Only two
	// Router implementations exist; a third would need to update this check.
	wantWorker := cfg.Cluster.IsWorker()
	_, isWorker := s.cluster.routerSwap.Current().(*routing.LocalOnlyRouter)
	if isWorker == wantWorker {
		return pkgConfig.DispositionIgnored
	}

	serverPort := fmt.Sprintf("%d", cfg.Node.Port)
	next := s.buildRouterForMode(wantWorker, serverPort)
	s.cluster.routerSwap.Swap(next)
	slog.Info("Router reconfigured", "mode", cfg.Cluster.Mode, "worker", wantWorker)
	return pkgConfig.DispositionApplied
}

// getClusterClient returns the unified cluster client (Phase 4)
// STRICT MASTER-WORKER: Only master can initiate cluster communication
func (s *Server) getClusterClient() mesh.ClusterClient {
	if s.cluster.coordinator == nil {
		return nil
	}

	// Defensive: Ensure config is not nil
	if s.config == nil {
		slog.Info("Cluster client denied: config is nil")
		return nil
	}

	// Only master can initiate cluster communication
	// Workers can only respond to requests, never initiate them
	if !s.node.IsCoordinator() {
		slog.Info("Cluster client denied: this host is not the master (mesh.enabled=false)")
		return nil
	}

	return s.cluster.coordinator.GetClient()
}

// ServeClusterRequest implements mesh.LocalHandler for in-process local dispatch
// This is the critical performance optimization that avoids network overhead
// Internal calls are marked as trusted to bypass authentication checks
func (s *Server) ServeClusterRequest(ctx context.Context, req *mesh.Request) (*mesh.Response, error) {
	// Create HTTP request from cluster request
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.Path, strings.NewReader(string(req.Body)))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Copy headers
	maps.Copy(httpReq.Header, req.Headers)

	// Mark as trusted internal request via context value — cannot be spoofed externally
	// This is safe because this function is only called for in-process local dispatch
	httpReq = httpReq.WithContext(context.WithValue(httpReq.Context(), internalRequestKey, true)) //nolint:contextcheck // deriving from httpReq.Context() IS the inherited context; lint misreads WithValue wrap

	// Add query string
	if req.Query != "" {
		httpReq.URL.RawQuery = req.Query
	}

	// Create a response recorder
	recorder := &responseRecorder{
		headers:    make(http.Header),
		body:       &strings.Builder{},
		statusCode: 200, // Default status
	}

	// Use ServeHTTP instead of HandleContext to properly initialize all Gin internals
	// HandleContext with CreateTestContext causes panic due to uninitialized slices
	s.engine.ServeHTTP(recorder, httpReq)

	// Convert to cluster response
	return &mesh.Response{
		StatusCode: recorder.statusCode,
		Headers:    recorder.headers,
		Body:       []byte(recorder.body.String()),
		SourceNode: s.node.Name(),
	}, nil
}

// ServeLocalRequest implements routing.LocalHandler for in-process local dispatch
// This is the V2 version that integrates with the routing layer
func (s *Server) ServeLocalRequest(ctx context.Context, req *routing.Request) (*routing.Response, error) {
	// Convert routing.Request to mesh.Request for reuse of ServeClusterRequest
	clusterReq := &mesh.Request{
		Method:  req.Method,
		Path:    req.Path,
		Headers: req.Headers,
		Body:    req.Body,
	}

	// Call existing ServeClusterRequest
	clusterResp, err := s.ServeClusterRequest(ctx, clusterReq)
	if err != nil {
		return nil, err
	}

	// Convert mesh.Response back to routing.Response
	return &routing.Response{
		StatusCode: clusterResp.StatusCode,
		Headers:    clusterResp.Headers,
		Body:       clusterResp.Body,
		Node:       clusterResp.SourceNode,
	}, nil
}

// responseRecorder implements http.ResponseWriter for capturing responses
type responseRecorder struct {
	statusCode int
	headers    http.Header
	body       *strings.Builder
}

func (r *responseRecorder) Header() http.Header {
	return r.headers
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(data)
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
}

package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/detect"
	"github.com/stperic/zzrouter/pkg/utils"
)

// finalizeHook wraps the callback for atomic.Pointer storage (funcs can't be stored directly).
type finalizeHook struct {
	fn func(name string)
}

// subscribeConfigStore wires the AppsConfigStore listeners that reconcile
// derived state on every config mutation. Called once during startup,
// BEFORE any mutation so discovery-time auto-enables are observed.
//
// Registration order is the invocation order — provider manager runs
// before model registry so port-pool and protocol-registry state lands
// first. Nil subsystems are skipped so partial bootstraps still work.
func (s *Server) subscribeConfigStore() {
	if s.configStore == nil {
		return
	}
	if s.providers.appMgr != nil {
		s.configStore.OnChange("providerAppManager", s.providers.appMgr.ReloadConfig)
	}
	if s.model.Registry != nil {
		s.configStore.OnChange("modelRegistry", s.model.Registry.ReloadConfig)
	}
	// The model cache subscribes where it is built (server_factory.go):
	// it does not exist yet here, so a registration here never happened.
	// Registering later also keeps it after the registry, so registry-side
	// changes land before reads flow through the cache.
	// Coord republishes its self-slot so cache-B readers see the new state.
	s.configStore.OnChange("server.publishSelf", func(_ *pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
		if s.cluster.coordinator == nil {
			return pkgConfig.DispositionIgnored
		}
		s.publishSelfSnapshot()
		return pkgConfig.DispositionApplied
	})
	// Provider-tree fan-out (plan §5.2): every AppsConfigStore mutation
	// pushes the new config.yaml + schema.yaml bytes to every worker
	// cache so PATCH on coord is immediately visible to launches on
	// worker. Single-flight coalesces rapid bursts. No-op on non-coord.
	//
	// Mutations are necessary but not sufficient, which is what
	// reconcileProviderTree exists for: a file edited on disk while the
	// node was stopped fires no event, and neither does a worker that
	// was unreachable when the event did fire.
	syncFanOut := newProviderSyncFanOut(
		s.configStore,
		func() bool { return s.cluster.coordinator != nil && !s.node.IsWorker() },
		s.GetClusterEndpoints,
		s.coordWorkerMTLSClient,
		s.getClusterClient,
	)
	s.configStore.OnChange("server.providerSyncFanOut", syncFanOut.Listener())
	// Kept on the server: a mutation is not the only reason a worker can
	// be out of step. See reconcileProviderTree.
	s.providers.syncFanOut = syncFanOut
}

// registerAppsFromConfig hooks the one post-load probe the server still
// performs: if an Ollama daemon is running and the user hasn't already
// enabled it, auto-enable. Everything else (managed installs, cloud
// providers, explicitly-enabled providers) is already reflected in the
// config by the time this runs. Subsystem reconciliation happens
// automatically via the AppsConfigStore listener registered by
// subscribeConfigStore.
func (s *Server) registerAppsFromConfig(_ *pkgConfig.NodeConfig) {
	if s.configStore == nil {
		slog.Info("No provider config found, skipping provider registration")
		return
	}
	s.maybeAutoEnableOllama()
}

// FinalizeOnboarding is the SINGLE terminal-success hook for any provider
// onboarding session (on-demand install, cloud verify, external connect,
// future session types). Callers:
//
//   - Internal install handler          — after installer.Install returns nil
//   - Internal execute-step handler     — after final step passes VerifyAll
//   - Public cloud verify handler       — after verify returns status=ok
//   - Internal PATCH enabled=true       — admin toggle (shares mechanism)
//
// Contract: call at the terminal success of your session, ONCE, on the node
// that owns the work. Never call configStore.SetProviderEnabled
// directly — funnel through here so every session type inherits the
// listener chain and error propagation uniformly.
//
// MUST remain idempotent and side-effect-minimal. The admin PATCH toggle
// shares this function; if you add a session-specific side effect here
// (event emission, audit log, notification) you MUST first split the
// admin PATCH path out so re-enables don't fire spurious session events.
//
// A post-finalize hook (SetOnFinalizeCallback) fires on success; on
// workers it dispatches NotifyMasterCacheRefresh.
//
// Returns an error if the config mutation fails; the caller MUST surface
// this to the HTTP response so a silent state drift ("installed but
// disabled") is never reported to the user as success. Unknown provider
// names surface as a wrapped config.ErrProviderNotFound for 404 mapping.
//
// Reconciliation (port pools, protocol registry, version cache, local
// caches) happens automatically via the AppsConfigStore listener chain
// after a successful SetProviderEnabled — by the time this returns nil,
// the in-memory state is reconciled. The enable is this node's own (see
// setProviderEnabled).
func (s *Server) FinalizeOnboarding(name string) error {
	if err := s.setProviderEnabled(name, true); err != nil {
		return fmt.Errorf("enable %s: %w", name, err)
	}
	slog.Info("Provider onboarding finalized", "name", name)
	s.lastMutation.Store(utils.Now().UnixNano())
	if hook := s.onFinalize.Load(); hook != nil {
		hook.fn(name)
	}
	return nil
}

// nodeEnableTimeout bounds a worker's enable request: the coordinator's
// wait for the provider sync, plus one short round trip.
const nodeEnableTimeout = providerSyncWaitTimeout + constants.HTTPShortTimeout

// setProviderEnabled records this node's choice with the file's owner: a
// worker's copy of a synced provider is the coordinator's (HandleProviderSync).
func (s *Server) setProviderEnabled(name string, enabled bool) error {
	if s.configStore == nil {
		return fmt.Errorf("config store not initialized")
	}
	if sc, ok := s.configStore.Config().LookupApp(name); !ok || !s.node.IsWorker() || !sc.SyncedToWorkers() {
		_, err := s.configStore.SetProviderEnabled(name, s.node.Name(), enabled)
		return err
	}
	if s.cluster.signaler == nil {
		return fmt.Errorf("this worker has no coordinator connection to record the change")
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, nodeEnableTimeout)
	defer cancel()
	if err := s.cluster.signaler.RecordProviderEnabled(ctx, name, enabled); err != nil {
		return fmt.Errorf("record on the coordinator: %w", err)
	}
	return nil
}

// handleNodeProviderEnabled records a worker's own enable choice, named by
// its certificate, and answers once that worker has it, so the worker's
// FinalizeOnboarding returns reconciled. PUT /internal/providers/:name/enabled.
func (s *Server) handleNodeProviderEnabled(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), providerSyncWaitTimeout)
	defer cancel()
	if s.configStore == nil {
		ServiceUnavailable(c, "providers config store not ready")
		return
	}
	ep := s.certifiedWorker(peerCertFromRequest(c.Request))
	if ep == nil {
		Forbidden(c, "the client certificate does not name a worker of this cluster")
		return
	}
	node := ep.NodeName
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !BindJSONStrict(c, &req) {
		return
	}
	if req.Enabled == nil {
		BadRequest(c, "enabled is required")
		return
	}
	name := c.Param("name")
	sc, ok := s.configStore.Config().LookupApp(name)
	if !ok {
		NotFound(c, fmt.Sprintf("provider %q not found", name))
		return
	}
	// A worker records only its entry in a tree it receives; the rest is the coordinator's own.
	if !sc.SyncedToWorkers() {
		Forbidden(c, fmt.Sprintf("provider %q is not synced to workers; a worker enables its own copy", name))
		return
	}
	changed, err := s.configStore.SetProviderEnabled(name, node, *req.Enabled)
	if err != nil {
		respondFinalizeErr(c, "record failed", err)
		return
	}
	// A recorded change starts its own sync. A choice the tree already held
	// starts none, and the worker may be the one an earlier round missed.
	if !changed {
		s.reconcileProviderTree("enable choice from " + node)
	}
	if !s.providers.syncFanOut.AwaitWorker(ctx, node) {
		RespondToError(c, httperr.Error{Status: http.StatusServiceUnavailable, Code: string(httperr.CodeProviderSyncPending),
			Message: fmt.Sprintf("recorded on the coordinator, but the provider sync has not reached %s yet; the next sync delivers it", node)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "node": node, "enabled": *req.Enabled})
}

// handleInternalRefreshCache publishes the inventory named by the worker's certificate.
func (s *Server) handleInternalRefreshCache(c *gin.Context) {
	if s.node.IsWorker() {
		BadRequest(c, "workers don't have cache to refresh")
		return
	}
	ep := s.certifiedWorker(peerCertFromRequest(c.Request))
	if ep == nil {
		Forbidden(c, "the client certificate does not name a worker of this cluster")
		return
	}
	slog.Info("Received worker cache refresh notice", "node", ep.NodeName)
	s.RefreshClusterEndpoint(ep.URL)
	if err := s.model.Cache.RefreshNode(c.Request.Context(), ep.NodeName); err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "Cache refreshed successfully"})
}

// certifiedWorker is the endpoint whose name is cert's CN and whose address
// cert covers, or nil when none is.
func (s *Server) certifiedWorker(cert *x509.Certificate) *mesh.Endpoint {
	if cert == nil || cert.Subject.CommonName == "" {
		return nil
	}
	for _, ep := range s.GetClusterEndpoints() {
		if ep != nil && !ep.IsLocal && ep.NodeName == cert.Subject.CommonName && nodeURLMatchesPeerCert(ep.URL, cert) {
			return ep
		}
	}
	return nil
}

// SetOnFinalizeCallback registers a hook fired after FinalizeOnboarding
// or FinalizeOffboarding returns nil. Atomic; safe to call concurrently.
func (s *Server) SetOnFinalizeCallback(fn func(name string)) {
	if fn == nil {
		s.onFinalize.Store(nil)
		return
	}
	s.onFinalize.Store(&finalizeHook{fn: fn})
}

// FinalizeOffboarding is the SINGLE terminal-success hook for any provider
// teardown session (uninstall, cloud disconnect, admin PATCH enabled=false).
// The symmetric dual of FinalizeOnboarding.
//
// For cloud providers this also removes the API key from .env. All other
// reconciliation happens via the AppsConfigStore listener chain — by the
// time this returns nil, the in-memory state is reconciled.
//
// Returns an error if the config mutation fails; callers MUST surface it
// to the HTTP response so a "files removed but still enabled in config"
// drift is never reported to the user as success. Unknown provider names
// surface as a wrapped config.ErrProviderNotFound for 404 mapping.
func (s *Server) FinalizeOffboarding(name string) error {
	if s.configStore == nil {
		return fmt.Errorf("config store not initialized")
	}
	// Remove cloud credentials before disabling
	cfg := s.configStore.Config()
	if cfg != nil {
		if svc, exists := cfg.LookupApp(name); exists && svc.IsCloudProvider() {
			if svc.Runtime != nil && svc.Runtime.API != nil {
				if envVar := extractEnvVarFromAPI(svc.Runtime.API); envVar != "" {
					_ = pkgConfig.RemoveEnvVar(envVar)
				}
			}
		}
	}

	if err := s.setProviderEnabled(name, false); err != nil {
		return fmt.Errorf("disable %s: %w", name, err)
	}
	slog.Info("Provider offboarding finalized", "name", name)
	s.lastMutation.Store(utils.Now().UnixNano())
	if hook := s.onFinalize.Load(); hook != nil {
		hook.fn(name)
	}
	return nil
}

// extractEnvVarFromAPI extracts the env var name from an API config's token or custom headers.
func extractEnvVarFromAPI(api *pkgConfig.APIConfig) string {
	if api.Token != "" {
		if ev := extractEnvVar(api.Token); ev != "" {
			return ev
		}
	}
	for _, v := range api.CustomHeaders {
		if ev := extractEnvVar(v); ev != "" {
			return ev
		}
	}
	return ""
}

// maybeAutoEnableOllama pings a local Ollama daemon's /api/version
// endpoint and flips `enabled: true` for the ollama provider if one
// answers. This is the single carve-out after removing the generic
// discovery subsystem: Ollama is the one provider that runs as an
// independent external daemon a user might have installed outside
// zzRouter, so we probe for it explicitly rather than asking every
// provider "are you there?". Everything else is either a managed
// install (enabled at install time) or a cloud provider (enabled when
// credentials are supplied).
//
// Does nothing if the ollama provider is absent, explicitly disabled,
// already enabled, or not declaring a version endpoint in its YAML.
func (s *Server) maybeAutoEnableOllama() {
	cfg := s.configStore.Config()
	if cfg == nil {
		return
	}
	svc, exists := cfg.LookupApp("ollama")
	if !exists || svc.IsEnabled() || svc.IsExplicitlyDisabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()
	if _, ok := detect.ProbeOllama(ctx, &svc); !ok {
		return
	}
	if err := s.FinalizeOnboarding("ollama"); err != nil {
		slog.Info("Failed to auto-enable Ollama", "error", err)
	}
}

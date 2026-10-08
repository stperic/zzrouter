// Server wiring — executor and service constructors that bridge Server fields
// into narrow-dependency constructors.  Also contains fallback health-target
// resolution which wires multiple Server subsystems together.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
)

// newParamsExecutor wires a ParamsExecutor with narrow deps from Server.
func (s *Server) newParamsExecutor() *ParamsExecutor {
	var protocolLookup func(string) (protocol.FullProvider, bool)
	var services providerServices
	if s.providers.appMgr != nil {
		protocolLookup = s.providers.appMgr.Protocol
		services = s.providers.appMgr
	} else {
		protocolLookup = func(string) (protocol.FullProvider, bool) { return nil, false }
	}
	executor := NewParamsExecutor(
		func() *pkgConfig.AppsConfig { return s.appsConfig },
		s.configStore,
		s.node.Nodename,
		s.knownClusterNodes,
		s.cluster.router,
		services,
		protocolLookup,
		s.httpClient,
		s.node.IsWorker,
		func() string {
			if s.cluster.listener == nil {
				return ""
			}
			return s.cluster.listener.CoordinatorURL()
		},
	).WithSyncWait(s.awaitProviderSync).WithCatalog(s.catalogWeights)
	executor.installAuthority = s.checkNodeInstallAuthority
	return executor
}

// catalogWeights reports the node holding real weights (not a variant) a
// model name, or an alias of it, names.
func (s *Server) catalogWeights(ctx context.Context, name string) (string, bool, error) {
	if s.model == nil || s.model.Cache == nil {
		return "", false, fmt.Errorf("model catalog unavailable")
	}
	return s.model.Cache.WeightsNamed(ctx, name)
}

// runsRefresher reports and restarts the runs a provider write affects,
// across the cluster.
func (s *Server) runsRefresher() *runsRefresher {
	return &runsRefresher{runs: s.services.Runs, jobs: s.jobs, local: s.node.Nodename,
		waitJob: func(ctx context.Context, id, node string) error {
			return waitRoutedJob(ctx, s.cluster.router, id, node)
		},
	}
}

// knownClusterNodes returns the set of currently-known cluster node names
// for unknown_node validation on PATCH /providers/:n/parameters. The local
// node is always included; workers known via the EndpointRegistry are
// added on coordinators.
func (s *Server) knownClusterNodes() map[string]bool {
	out := map[string]bool{}
	if local := s.node.Nodename(); local != "" {
		out[local] = true
	}
	for _, ep := range s.GetClusterEndpoints() {
		if ep == nil {
			continue
		}
		// Canonical NodeName only — aliases are not persisted to the
		// provider tree, so accepting them here would let PATCH write
		// non-canonical keys that Resolve() never matches at launch.
		if ep.NodeName != "" {
			out[ep.NodeName] = true
		}
	}
	return out
}

// refreshResourceMetrics returns a closure that forces an
// immediate resource re-probe. Nil-safe when cluster tracking is
// disabled so the load/unload paths can always call through.
func (s *Server) refreshResourceMetrics() func() {
	if s.cluster.resources == nil {
		return nil
	}
	return func() { _ = s.cluster.resources.CollectNow() }
}

// newRunsExecutor wires a RunsExecutor with narrow deps from Server.
func (s *Server) newRunsExecutor() *RunsExecutor {
	return NewRunsExecutor(
		s.providers.appMgr,
		s.node.Nodename,
		func() *pkgConfig.AppsConfig { return s.appsConfig },
		s.cluster.router != nil,
		s.logsHandlers.streamInstanceLogs,
		s.logsHandlers.getInstanceLogLines,
		s.refreshResourceMetrics(),
	)
}

// newLoadExecutor wires a LoadExecutor with narrow deps from Server.
func (s *Server) newLoadExecutor() *LoadExecutor {
	return NewLoadExecutor(
		s.providers.appMgr,
		func(p string) bool { return isProviderEnabledInConfig(s.appsConfig, p) },
		s.model.Registry,
		s.backend.Resolve,
		s.resolveForLaunch,
		s.launchOnDemandContainerWithParams,
		s.model.Cache.Invalidate,
		s.node.Nodename,
		s.refreshResourceMetrics(),
	)
}

// newInternalExecutor wires an InternalExecutor with narrow deps from Server.
func (s *Server) newInternalExecutor() *InternalExecutor {
	getResourceMetrics := func() any {
		if s.cluster.resources == nil {
			return nil
		}
		return s.cluster.resources.GetMetrics()
	}
	executor := NewInternalExecutor(
		s.model.Registry,
		s.node.Nodename,
		s.ollamaDaemon,
		s.httpClient,
		func() *huggingface.Connector {
			if s.model.Registry == nil {
				return nil
			}
			return s.model.Registry.GetHuggingFaceConnector()
		},
		func(app string) bool { return isCloudProvider(s.appsConfig, app) },
		s.configStore,
		s.model.Cache.RefreshCacheSync,
		getResourceMetrics,
	)
	executor.invalidateCache = s.model.Cache.Invalidate
	executor.inventoryStatus = s.providerInventoryStatus
	if s.providers.appMgr != nil {
		executor.modelDetails = s.providers.appMgr.ModelFeatureDetails
	}
	return executor
}

// newOllamaService wires an OllamaService with narrow deps from Server.
func (s *Server) newOllamaService() OllamaService {
	return NewOllamaService(
		s.ollamaDaemon,
		s.getRemoteModelInfo,
		s.proxyToRemoteNode,
		s.node.Nodename,
		s.node.IsLocalNode,
		s.httpStreamingClient,
		s.getClusterNodeURL,
		s.getClusterNodeMTLSURL,
		s.getMTLSClient,
		s.proxyToOllamaProvider,
		s.httpClient,
		s.cluster.router != nil,
		s.dispatchOllamaInference,
		s.invalidateModelCache,
	)
}

// invalidateModelCache forces a fresh source rescan and warms the
// catalog so subsequent /v1/models calls see the result without a
// per-call scan. Used by the post-pull hook on /api/pull. The eager
// ListModels mirrors RescanLocal's pattern — pure Invalidate alone
// has been observed to leave the catalog stale until a future
// ListModels triggers the rescan, which loses the unification
// guarantee for agents calling /v1/* immediately after /api/pull.
func (s *Server) invalidateModelCache() {
	if _, err := s.RescanLocal(); err != nil {
		// The pull already succeeded; a rescan failure here just means
		// the model won't appear on /v1/models until the next periodic
		// scan or explicit /zzrouter/v1/models/scan call.
		slog.Warn("post-pull model rescan failed", "error", err)
	}
}

// newDeploymentsExecutor wires a DeploymentsExecutor with narrow deps from Server.
func (s *Server) newDeploymentsExecutor() *DeploymentsExecutor {
	e := NewDeploymentsExecutor(
		s.node.Nodename,
		func(app string) bool { return isCloudProvider(s.appsConfig, app) },
		s.isLocalNodeCompatible,
		s.buildIncompatibleDeployError,
		s.model.Downloads,
		s.model.Registry,
		s.model.AutoRoute,
		s.ollamaDaemon,
		func() *pkgConfig.AppsConfig { return s.appsConfig },
		s.configStore,
		s.model.Cache.RefreshCacheSync,
		s.jobs,
	)
	e.ensureFeatures = s.providers.appMgr.EnsureModelFeatures
	return e
}

// buildHealthTargets creates health check targets from model group configurations.
func (s *Server) buildHealthTargets() []fallback.HealthTarget {
	var targets []fallback.HealthTarget

	for _, group := range s.model.Groups.List() {
		if group.HealthCheck == nil {
			continue
		}

		for _, rep := range group.Replicas {
			if rep.OnDemand {
				continue
			}

			baseURL, header := s.deploymentHealthTarget(rep.App, rep.Node)
			if baseURL == "" {
				continue
			}

			path := group.HealthCheck.Path
			if path == "" {
				path = "/health"
			}
			interval := group.HealthCheck.Interval.Duration
			if interval == 0 {
				interval = 30 * time.Second
			}
			timeout := group.HealthCheck.Timeout.Duration
			if timeout == 0 {
				timeout = 5 * time.Second
			}

			targets = append(targets, fallback.HealthTarget{
				DeploymentName: rep.Name,
				URL:            baseURL,
				Path:           path,
				Interval:       interval,
				Timeout:        timeout,
				Header:         header,
			})
		}
	}

	return targets
}

// deploymentHealthTarget returns where a deployment's health is probed
// and the credential the probe carries. A replica on another node is
// probed at that node's own health route, which takes none.
func (s *Server) deploymentHealthTarget(app, node string) (string, http.Header) {
	resolved, ok := s.backend.Resolve(app)
	if ok && resolved.Cloud {
		return resolved.Endpoint, resolved.Upstream.Header()
	}
	if node != "" && !s.node.IsLocalNode(node) {
		base, err := s.getClusterNodeURL(node)
		if err != nil {
			return "", nil
		}
		return base, nil
	}
	if ok {
		return resolved.Endpoint, resolved.Upstream.Header()
	}
	return "", nil
}

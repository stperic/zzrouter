// Server wiring — executor and service constructors that bridge Server fields
// into narrow-dependency constructors.  Also contains fallback health-target
// resolution which wires multiple Server subsystems together.

package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol"
	"github.com/stperic/zzrouter/pkg/utils"
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
	executor.publishCatalog = s.publishCatalog
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
	e := NewRunsExecutor(
		s.providers.appMgr,
		s.node.Nodename,
		func() *pkgConfig.AppsConfig { return s.appsConfig },
		s.cluster.router != nil,
		s.logsHandlers.streamInstanceLogs,
		s.logsHandlers.getInstanceLogLines,
		s.refreshResourceMetrics(),
	)
	e.publishCatalog = s.publishCatalog
	return e
}

// newLoadExecutor wires a LoadExecutor with narrow deps from Server.
func (s *Server) newLoadExecutor() *LoadExecutor {
	return NewLoadExecutor(
		s.providers.appMgr,
		s.model.Registry,
		s.node.Nodename,
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
		s.publishCatalog,
		getResourceMetrics,
	)
	executor.modelCache = s.model.Cache
	return executor
}

// newOllamaService wires an OllamaService with narrow deps from Server.
func (s *Server) newOllamaService() OllamaService {
	return NewOllamaService(
		s.ollamaDaemon,
		s.model.Resolver.Resolve,
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
		s.publishCatalog,
	)
}

func (s *Server) publishCatalog() error {
	_, err := s.RescanLocal()
	return err
}

func publishOr503(c *gin.Context, publish func() error) bool {
	if publish != nil {
		if err := publish(); err != nil {
			ServiceUnavailable(c, err.Error())
			return false
		}
	}
	return true
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
		s.ollamaDaemon,
		func() *pkgConfig.AppsConfig { return s.appsConfig },
		s.configStore,
		s.publishCatalog,
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

// notifyCoordinatorAsync tells the coordinator this worker's catalog changed
// without blocking the caller, which may hold its own locks; Stop drains it.
func (s *Server) notifyCoordinatorAsync() {
	if s.cluster.signaler == nil || s.draining.Load() {
		return
	}
	s.refreshWG.Add(1)
	go func() {
		defer s.refreshWG.Done()
		defer utils.RecoverAndLog("signal.NotifyCacheRefresh")
		_ = s.cluster.signaler.NotifyCacheRefresh(s.shutdownCtx)
	}()
}

// observeModelStart records one attempt and refreshes resources after a cold start.
func (s *Server) observeModelStart(ctx context.Context, req prov_apps.EnsureRequest) (context.Context, func(prov_apps.EnsureResult, error)) {
	ctx, span := llm.StartModelLoadSpan(ctx, req.Model, req.Provider)
	started := utils.Now()
	return ctx, func(result prov_apps.EnsureResult, err error) {
		defer span.End()
		elapsed := time.Since(started)
		status := "success"
		if err != nil {
			status = "failed"
			llm.SetSpanError(span, err)
		} else {
			id, port := "", result.Port
			if result.Run != nil {
				id, port = result.Run.ID, result.Run.Port
			}
			llm.AddModelLoadResult(span, float64(elapsed.Milliseconds()), id, port)
			llm.SetSpanOK(span)
			if refresh := s.refreshResourceMetrics(); refresh != nil { //nolint:contextcheck // detached like triggerResourceRefresh; CollectNow takes no context
				go refresh()
			}
		}
		if metrics := llm.GetMetrics(); metrics != nil {
			metrics.RecordModelLoad(ctx, req.Model, req.Provider, status, elapsed.Seconds())
			if err == nil && result.Run != nil {
				metrics.RecordInstanceStart(ctx, req.Model, req.Provider)
			}
		}
	}
}

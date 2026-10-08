// package server — Server construction (NewServerWithOptions).
//
// The constructor is intentionally kept in its own file so server.go
// can focus on the struct definition and interface delegates.

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/connectivity"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model"
	"github.com/stperic/zzrouter/pkg/model/autoroute"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	modelcache "github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/search"
	"github.com/stperic/zzrouter/pkg/observability"
	llmobs "github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/logger"
	"github.com/stperic/zzrouter/pkg/observability/proxy"
	"github.com/stperic/zzrouter/pkg/observability/quotametrics"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
	obsspend "github.com/stperic/zzrouter/pkg/observability/spend"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/routing"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/utils/clock"
	"github.com/stperic/zzrouter/pkg/version"
)

// defaultMaxRequestSize caps a request body when node.yaml sets no
// max_request_size: a memory-exhaustion guard, not a protocol limit.
const defaultMaxRequestSize = 10 << 20

// NewServerWithOptions creates a new zzrouter host server with options.
//
//nolint:gocyclo,cyclop // subsystem wiring function; decomposing the switch list hides the order invariants documented inline
func NewServerWithOptions(cfg *pkgConfig.NodeConfig) (*Server, error) {
	// Debug mode is already initialized by CLI startup code
	// (either from --debug flag or ZZROUTER_DEBUG env var)

	if err := pkgConfig.ValidateConfiguredKeys(cfg); err != nil {
		return nil, err
	}

	// Create centralized connection manager (single source of truth)
	var connOpts []connectivity.ConnectionManagerOption
	if cfg.Cluster.TLSCACert != "" {
		tlsCfg, err := security.TLSClientConfig(cfg.Cluster.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("cannot load cluster CA certificate %s: %w", cfg.Cluster.TLSCACert, err)
		}
		connOpts = append(connOpts, connectivity.WithTLSConfig(tlsCfg))
		slog.Info("Cluster TLS CA configured", "ca_cert", cfg.Cluster.TLSCACert)
	}
	connMgr := connectivity.NewConnectionManager(connOpts...)

	nodeStore := pkgConfig.NewNodeConfigStoreFromConfig(cfg)

	normalizers := NewResponseNormalizers()
	normalizer.RegisterMLX(normalizers)

	// shutdownCtx is initialized from Background so request handlers
	// that reach for it before Server.Start runs (notably under
	// httptest, which never calls Start) see a live ctx. Start
	// replaces it with a child of the caller's ctx, and Stop cancels.
	s := &Server{
		nodeConfigStore:     nodeStore,
		config:              cfg,
		httpClient:          connMgr.GetHTTPClient(),
		httpStreamingClient: connMgr.GetStreamingClient(),
		connManager:         connMgr, // Store reference for metrics
		listenerReady:       make(chan struct{}),
	}
	s.shutdownCtx, s.shutdownCancel = context.WithCancel(context.Background())
	s.node = NewNodeIdentity(cfg)

	// Cluster role. Derived from static config at construction;
	// pairing completion and renewal failure handlers promote/demote
	// via s.role.Set. Every read site uses s.role.Current() — no
	// direct config sampling.
	paths := clusternode.PathsFromConfigDir(pkgConfig.Paths().GetConfigDir())
	initialRole := role.RoleFromConfig(cfg.Cluster, clusternode.IsPaired(paths))
	roleMgr, err := role.NewManager(initialRole)
	if err != nil {
		return nil, fmt.Errorf("init role manager: %w", err)
	}
	s.role = roleMgr

	s.backend = backend.NewResolver(func() *pkgConfig.AppsConfig { return s.appsConfig })
	s.inference.normalizers = normalizers
	s.proxy = NewProxyClient(s.httpStreamingClient, s.coordWorkerMTLSClient, normalizers, s.trackProviderRateLimit,
		func() bool { return s.config.Coordinator.Routing.InjectUsageMetadata },
		s.GetNodename)
	s.inference.affinity = newResponseAffinity(0)

	// Initialize the model subsystem. Pricing wires up front; Cache and
	// NodeCache are assigned in server_wiring.go once cluster setup has
	// built the dependencies they need.
	s.model = &model.Subsystem{}
	if cfg.Models.Pricing.IsEnabled() {
		cm := pkgConfig.NewConfigManager("zzrouter")
		cacheDir := filepath.Join(cm.GetNodeConfigDir(), "pricing")
		s.model.Pricing = pricing.NewStore(&cfg.Models.Pricing, cacheDir)
		// Overrides live with the other operator-authored state
		// (keys.yaml, teams.yaml, model_groups.yaml), not in the cache
		// dir the upstream table is refetched into.
		overridesPath := filepath.Join(cm.GetNodeConfigDir(), "pricing_overrides.yaml")
		if err := s.model.Pricing.LoadOverrides(overridesPath); err != nil {
			slog.Warn("Failed to load pricing_overrides.yaml", "error", err, "path", overridesPath)
		}
	}

	// Initialize inference log store (in-memory ring buffer)
	if cfg.InferenceLog.IsEnabled() {
		maxEntries, maxPayloads := cfg.InferenceLog.GetMaxEntries(), cfg.InferenceLog.GetMaxPayloads()
		s.inference.logStore = inferencelog.NewStore(maxEntries, maxPayloads)
		s.inference.logBridge = NewInferenceLogBridge(s.inference.logStore, cfg.Node.Name, cfg.InferenceLog.ShouldCapturePrompts(), s.model.Pricing)
		llmobs.SetInferenceLogHook(s.inference.logBridge)
		llmobs.SetCaptureResponses(cfg.InferenceLog.ShouldCapturePrompts())
		slog.Info("Inference log enabled", "max_entries", maxEntries,
			"capture_prompts", cfg.InferenceLog.ShouldCapturePrompts(), "max_payloads", maxPayloads)
	}

	// Jobs registry is unconditional — every node runs a producer and
	// the HTTP surface is role-gated at route mount time. Construction
	// is cheap (maps + goroutine); failure here is a programming error.
	jobsReg, err := jobs.NewRegistry(jobs.Config{
		NodeName: cfg.Node.Name,
		Clock:    clock.System(),
	})
	if err != nil {
		return nil, fmt.Errorf("jobs registry: %w", err)
	}
	s.jobs = jobsReg

	// Attach a long-lived KindInferenceLog handle to the bridge so every
	// completed inference mirrors onto /zzrouter/v1/jobs/:id/stream. The
	// handle lives for the lifetime of the registry; Firehose policy
	// means no ring and no replay — subscribers always tail from now.
	// Handle-open failure here is a programming bug (registry is fresh,
	// KindInferenceLog is a constant, never-stopped), so propagate.
	if s.inference.logBridge != nil {
		h, herr := jobsReg.Start(context.Background(), jobs.KindInferenceLog, "",
			jobs.Meta{"node": cfg.Node.Name, "stream": "inference_log"})
		if herr != nil {
			return nil, fmt.Errorf("open inference-log jobs handle: %w", herr)
		}
		s.inference.logBridge.SetJobHandle(h)
		s.inference.logJobID = h.ID()
	}

	// Configure model registry with models.cache and models.shared from config
	// This must happen early, before any model operations
	modelregistry.SetModelsConfig(cfg.Models.GetCache(), cfg.Models.GetShared())

	// Apply provider install knobs from node.yaml providers.* — install dir,
	// corporate proxy + CA bundle, and venv hardening toggle. Must run before
	// the ProviderAppManager is constructed (below) because manager_init and
	// some installer paths read ProviderRootDir() at construction time.
	install.SetProviderRootOverride(cfg.Providers.InstallDir)
	install.SetHardenVenvDefault(cfg.Providers.ShouldHardenVenv())
	if cfg.Providers.Proxy.IsSet() {
		install.SetExecEnv(install.BuildProxyEnv(
			cfg.Providers.Proxy.HTTPProxy,
			cfg.Providers.Proxy.HTTPSProxy,
			cfg.Providers.Proxy.NoProxy,
			cfg.Providers.Proxy.CAFile,
		))
	}

	// Load apps configuration via centralized store
	store, err := pkgConfig.NewAppsConfigStoreFromStandardLocations()
	if err != nil {
		logger.Warn("No provider config found", "error", err)
	} else {
		s.configStore = store
		// TODO: restore connection-marker overlay hook (install.ApplyConnectionOverlay
		// + AppsConfigStore.SetPreNotifyHook) — the supporting code is missing from
		// origin/main at merge time.
		s.appsConfig = store.Config()
	}

	// Register cloud search providers from config (OpenAI-compatible /v1/models)
	search.RegisterCloudProvidersFromConfig(func() *pkgConfig.AppsConfig { return s.appsConfig })

	// Initialize ProviderAppManager (prov_apps — unified lifecycle for providers)
	providerAppMgr, err := prov_apps.NewProviderAppManager(
		s.appsConfig,
		prov_apps.WithHTTPClient(s.httpClient),
		prov_apps.WithJobsRegistry(s.jobs),
		prov_apps.WithCatalogChanged(func() {
			if s.model.Cache != nil {
				s.model.Cache.Invalidate()
			}
			s.publishSelfSnapshot()
			if s.cluster.signaler != nil {
				s.cluster.signaler.NotifyCacheRefresh()
			}
		}),
		prov_apps.WithNodename(s.node.Nodename),
		prov_apps.WithInstallPolicy(cfg.Providers.InstallPolicyFile),
		// The launch path and the inference path both key instances by
		// model name, and only inference resolved aliases -- so a model
		// launched by its repo id and then asked for by the same repo id
		// was started twice, the second time under its file stem. One
		// canonicalizer, applied where the launch is keyed.
		prov_apps.WithModelCanonicalizer(s.resolveCanonicalModelName),
		prov_apps.WithModelValidator(s.checkCatalogVariantConflict),
		prov_apps.WithModelSource(s.modelSource),
		prov_apps.WithParamLocalizer(assetParamLocalizer(s.configStore)),
		prov_apps.WithRuntimeChecks(providerRuntimeChecks(s.configStore)),
		prov_apps.WithMemoryBudget(providerMemoryBudget(s.configStore)),
	)
	if err != nil {
		logger.Warn("Failed to initialize provider app manager", "error", err)
	} else {
		s.providers.appMgr = providerAppMgr
	}

	// Initialize download tracker with persistence
	{
		configMgr := pkgConfig.NewConfigManager("zzrouter")
		dataDir := configMgr.GetNodeDataDir()
		stateFilePath := filepath.Join(dataDir, "downloads.json")
		s.model.Downloads = modelregistry.NewDownloadTrackerWithPersistence(stateFilePath)
	}

	// ModelCache is an EventCache — invalidated by mutation events,
	// never by TTL. The cfg.Cache.ModelListTTLSeconds field is
	// retained in config for backward compatibility; warn once if a
	// stale node.yaml still sets it so operators aren't left wondering
	// why their knob isn't taking effect.
	if cfg.Cache.ModelListTTLSeconds != 0 { //nolint:staticcheck // SA1019: the field IS deprecated; this is the one read-site that warns about it
		slog.Warn("cache.model_list_ttl_seconds is deprecated and ignored; the model catalog cache is now event-invalidated",
			"configured_value", cfg.Cache.ModelListTTLSeconds) //nolint:staticcheck // see above
	}
	// ModelCache is fully wired after cluster setup (see below)

	// Initialize integrity verifier for zero-trust model cache operations
	s.model.Verifier = modelcache.NewIntegrityVerifier()

	// Initialize model registry. NewRegistry seeds itself from the current
	// AppsConfig via ReloadConfig — the same hook subscribeConfigStore
	// wires up for runtime changes, so initial boot and post-install
	// reloads share a single code path.
	modelRegistry, err := modelregistry.NewRegistry(func() *pkgConfig.AppsConfig { return s.appsConfig })
	if err != nil {
		logger.Warn("Failed to create model registry", "error", err)
	} else {
		s.model.Registry = modelRegistry
	}

	// Subscribe to config changes BEFORE discovery so auto-enables are observed
	// by the provider manager and local caches via a single listener.
	s.subscribeConfigStore()

	// Seed the version cache for providers already enabled in the config.
	// Without this, HTTP surfaces that read ProviderVersion return "unknown"
	// for every pre-enabled provider until the user triggers a config
	// mutation (the listener chain only fires on *changes*). The boot
	// seed is the one synchronous read of install.ReadInstalledVersion
	// and the Ollama probe for any enabled ollama provider.
	if s.providers.appMgr != nil {
		if err := s.providers.appMgr.DiscoverAndRegister(context.Background()); err != nil {
			slog.Warn("Provider version cache seeding failed", "error", err)
		}
	}

	// Probe for a running Ollama daemon. If one answers and the user
	// hasn't already enabled the ollama provider, flip enabled=true —
	// which re-fires subscribeConfigStore's listener chain and caches
	// the version. Reconciliation (port pools, protocol registry,
	// caches) is handled by the listener.
	s.registerAppsFromConfig(cfg)

	// Set Gin to release mode to disable debug output
	gin.SetMode(gin.ReleaseMode)

	// Register validator translator so binding errors reference JSON field names
	// instead of Go struct field names. Must be called before any request binding.
	RegisterValidatorTranslator()

	// Initialize the dialect-aware responder set. It is the single
	// place where /v1/*, /api/*, and /zzrouter/v1/* error envelopes
	// are configured; see responder_setup.go for the wiring rules.
	s.responders = newResponderSet()

	s.initHTTPEngine(cfg)

	if err := s.initClusterNetworking(cfg); err != nil {
		return nil, err
	}

	// Initialize model resolver (default uses cache-based resolution)
	defaultResolver := resolver.NewDefault(s.getRemoteModelInfo)
	s.model.Resolver = defaultResolver

	// Load model groups and virtual keys from their respective config files.
	s.model.Groups = modelgroup.NewGroupStore()
	s.model.Groups.SetCloudAppChecker(func(app string) bool {
		return s.appsConfig.GetCloud(app) != nil
	})
	{
		cm := pkgConfig.NewConfigManager("zzrouter")
		configDir := cm.GetNodeConfigDir()

		// Load model_groups.yaml
		groupsPath := filepath.Join(configDir, "model_groups.yaml")
		s.model.Groups.SetPath(groupsPath) // Always set path so CRUD can create the file
		if err := s.model.Groups.LoadFromFile(groupsPath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("Failed to load model_groups.yaml", "error", err)
			}
		} else {
			slog.Info("Model groups loaded", "count", s.model.Groups.Len(), "path", groupsPath)
		}
		s.model.Resolver = resolver.NewGroup(s.model.Groups, defaultResolver)
		s.model.NodeCache = cache.NewNodeResourceCache()

		// Wire Cache — must happen after NodeCache is built. AutoRoute is
		// set via SetAutoRoute below (breaks the construction cycle:
		// autoroute.New needs Cache.GetAllModels and Cache.Invalidate).
		s.model.Cache = cache.New(cache.Config{
			InventoryStatus: s.providerInventoryStatus,
			ModelDetails: func(provider, model string, details map[string]any) map[string]any {
				if s.providers.appMgr == nil {
					return details
				}
				return s.providers.appMgr.ModelFeatureDetails(provider, model, details)
			},
			Node:                      s.node,
			Registry:                  func() *modelregistry.Registry { return s.model.Registry },
			AppsConfig:                func() *pkgConfig.AppsConfig { return s.appsConfig },
			ClusterClient:             s.getClusterClient,
			NodeCache:                 s.model.NodeCache,
			Resources:                 s.cluster.resources,
			NodeConfig:                cfg,
			ResolveEndpointToNodename: s.resolveEndpointToNodename,
		})
		// What a provider config change does to the catalog (an enabled
		// provider, a variant defined) reaches it through this listener.
		// subscribeConfigStore ran before the cache existed, so it is
		// registered here.
		if s.configStore != nil {
			s.configStore.OnChange("modelCache", s.model.Cache.ReloadConfig)
		}

		// Worker→coord signaling is a cluster concern, not a cache one.
		// Construct here so the factory owns the lazy accessors.
		s.cluster.signaler = clustersignal.New(clustersignal.Config{
			IsWorker: func() bool { return s.node.IsWorker() },
			MTLSClient: func() *http.Client {
				if s.cluster.listener == nil || s.cluster.listener.Mode() != clusternode.Worker {
					return nil
				}
				client, err := s.cluster.listener.DialClient(clusterid.RoleCoordinator.OU())
				if err != nil {
					return nil
				}
				return client
			},
			CoordinatorURL: func() string {
				if s.cluster.listener == nil {
					return ""
				}
				return s.cluster.listener.CoordinatorURL()
			},
			WorkerPublicURL: func() string {
				return s.workerPublicURL()
			},
		})

		s.model.AutoRoute = autoroute.New(s.model.Groups, s.config.Coordinator.Routing.GetRoutePrefix(), s.model.Cache.Invalidate, s.model.Cache.GetAllModels)
		s.model.Cache.SetAutoRoute(s.model.AutoRoute)

		// Set callback to invalidate/refresh cache when downloads complete
		s.model.Downloads.SetOnCompleteCallback(func() {
			if cfg.Cluster.IsWorker() {
				s.cluster.signaler.NotifyCacheRefresh()
			} else {
				s.model.Cache.Invalidate()
			}
		})

		// Workers notify the coord that its per-endpoint snapshot is stale.
		s.SetOnFinalizeCallback(func(_ string) {
			if s.IsWorker() {
				s.cluster.signaler.NotifyCacheRefresh()
			}
		})

		// Load keys.yaml
		keysPath := filepath.Join(configDir, "keys.yaml")
		s.keyStore = keys.NewFileKeyStore(keysPath)
		if err := s.keyStore.Load(); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("Failed to load keys.yaml", "error", err)
			}
		} else {
			keyCount := len(s.keyStore.List())
			if keyCount > 0 {
				slog.Info("Virtual keys loaded", "count", keyCount)
			}
		}

		// Load teams.yaml
		teamsPath := filepath.Join(configDir, "teams.yaml")
		s.teamStore = teams.NewFileTeamStore(teamsPath)
		if err := s.teamStore.Load(); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.Warn("Failed to load teams.yaml", "error", err)
			}
		} else if teamCount := len(s.teamStore.List()); teamCount > 0 {
			slog.Info("Teams loaded", "count", teamCount)
		}

		// === Audit sink ===
		// Opened/closed in start/stopCoordinatorSubsystems so runtime
		// role transitions (Unclaimed → Coordinator via pairing) get
		// audit coverage. The factory leaves the sink at audit.Null;
		// workers stay on Null for their entire lifecycle.
		s.auditSink = audit.Null{}
		s.auditConfigDir = configDir

		// === AccessControl: unified auth + quota enforcement ===
		staticKeySet := buildStaticKeySet(cfg)
		spendPath := filepath.Join(configDir, "spend.json")
		spendTracker := quota.NewSpendTracker(spendPath)
		rateLimiter := quota.NewRateLimiter()
		qConcurrency := quota.NewConcurrencyLimiter()
		enforcer := quota.NewEnforcer(rateLimiter, qConcurrency, spendTracker)
		if rec, err := quotametrics.New(); err != nil {
			logger.Warn("Failed to init quota metrics recorder; /metrics will not include quota families", "error", err)
		} else {
			enforcer.SetMetricsRecorder(rec)
		}
		s.spendEvents = NewSpendEventBus()
		enforcer.SetBreachObserver(s.spendEvents)
		s.access = NewAccessControl(staticKeySet, s.keyStore, s.teamStore, s.model.Groups, enforcer, cfg,
			ModelHooks{IsCloudBacked: s.modelIsCloudBacked, Identity: s.modelIdentity})

		// Wire gateway into the inference log bridge for post-response settlement
		if s.inference.logBridge != nil {
			s.inference.logBridge.SetAccessControl(s.access)
		}

		// Wire the per-key budget snapshotter so the LiteLLM-mirrored
		// observable gauges (zz.api.key.max.budget.metric +
		// zz.remaining.api.key.budget.metric) report live SpendLimit /
		// remaining-budget rows on every Prometheus scrape. The gauges
		// register lazily against the current global meter provider.
		// Workers also reach this branch but their keystore is empty
		// in production (virtual keys only on coordinators), so the
		// snapshotter returns no rows and the gauges emit nothing —
		// the unconditional registration is harmless.
		obsspend.SetBudgetSnapshotter(newBudgetSnapshotter(s.keyStore, s.teamStore, s.access))
	}

	s.initFallbackProxy()

	// Auth handlers (used by route registration)
	s.auth = &AuthHandlers{
		access:          s.access,
		config:          s.config,
		nodeConfigStore: s.nodeConfigStore,
		responders:      s.responders,
	}

	// Setup routes
	s.setupRoutes()

	// Wire the cluster-port inference handler. The clusternode listener
	// requires this for Unclaimed and Worker modes because a successful
	// pairing flips Unclaimed→Worker at runtime and the rebuilt
	// listener picks up the handler at that moment. Installed here
	// (post-setupRoutes) so all executor-backing state is already
	// wired.
	if s.cluster.listener != nil && s.cluster.listener.Mode() != clusternode.Disabled {
		var adminAPI http.Handler
		if s.cluster.listener.Mode() == clusternode.Coordinator {
			adminAPI = buildCoordInternalEngine(s)
		} else {
			adminAPI = buildInternalEngine(s)
		}
		if err := s.cluster.listener.SetAdminAPIHandler(adminAPI); err != nil {
			return nil, fmt.Errorf("install cluster admin-api handler: %w", err)
		}
		// Worker-track modes additionally serve the OpenAI/Ollama compat
		// surface on the cluster mTLS port — coord-proxied inference
		// flows through here. Coordinator mode skips this; coord serves
		// compat on its admin port for external clients.
		if s.cluster.listener.Mode() != clusternode.Coordinator {
			compat := buildWorkerCompatEngine(s)
			if err := s.cluster.listener.SetWorkerCompatHandler(compat); err != nil {
				return nil, fmt.Errorf("install worker compat handler: %w", err)
			}
		}
	}

	// Create HTTP server with timeouts to prevent connection exhaustion
	// NOTE: WriteTimeout/ReadTimeout are NOT set because SSE/streaming endpoints
	// (model inference, log streaming, deploy progress) hold connections open for minutes
	//
	// Worker bind narrowing: paired workers bind admin port to 127.0.0.1
	// only. Local CLI (zzrouter cluster decommission, etc.) still works;
	// the port is invisible to external network scans. Unclaimed binds the
	// configured address because the operator may be running pair from
	// another LAN host. Coordinator + Disabled use the configured address
	// (admin port is the external surface for them).
	//
	// Caveat: Mode is read at factory build time. A node that boots
	// Unclaimed and pairs at RUNTIME (Unclaimed→Worker via completePairing)
	// keeps the public admin listener until process restart. Operators
	// restart workers after first pair anyway; documented gap, not a bug.
	bindAddr := cfg.Node.Bind
	if s.cluster.listener != nil && s.cluster.listener.Mode() == clusternode.Worker {
		bindAddr = "127.0.0.1"
	}
	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", bindAddr, cfg.Node.Port),
		Handler:           s.engine,
		ReadHeaderTimeout: constants.ServerReadHeaderTimeout,
		IdleTimeout:       constants.ServerIdleTimeout,
	}
	if cfg.Node.IsTLSEnabled() {
		s.httpServer.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	return s, nil
}

// tryInitClusterCoordinator initializes the unified cluster coordinator
// for master nodes. Failure is degraded-ok: the server runs without
// cluster features, errors are logged but not returned, matching the
// pre-extraction behavior where the constructor logged and continued.
// connector carries the mTLS dispatch client + cluster port when the
// cluster listener came up in Coordinator mode; nil means the degraded
// Master-without-Coordinator-listener path — the cluster still runs
// but with a default http.Client and no cluster-port probing.
func (s *Server) tryInitClusterCoordinator(cfg *pkgConfig.NodeConfig, connector *mesh.Connector) {
	if !cfg.Cluster.IsMaster() {
		return
	}
	// Determine the actual IP address for cluster registration
	// If Node.Node is 0.0.0.0 (bind all), detect the actual IP
	localNode := cfg.Node.Bind
	if localNode == "0.0.0.0" || localNode == "" {
		if detectedIP, err := getOutboundIP(); err == nil {
			localNode = detectedIP
		}
	}
	scheme := "http"
	if cfg.Node.IsTLSEnabled() {
		scheme = "https"
	}
	localNodeURL := fmt.Sprintf("%s://%s:%d", scheme, localNode, cfg.Node.Port)

	// Convert config.ClusterConfig to mesh.Config
	clusterCfg := &mesh.Config{
		Enabled:       cfg.Cluster.IsMaster(),
		BindAddr:      cfg.Cluster.BindAddr,
		BindPort:      cfg.Cluster.BindPort,
		AdvertisePort: cfg.Cluster.AdvertisePort,
		Members:       cfg.Cluster.Members,
		Endpoints:     cfg.Cluster.Endpoints.Addresses(),
		NodeName:      cfg.Node.Name,
		NameObserver:  s.cachePeerName,
	}

	// Copy strategy defaults if present
	if len(cfg.Cluster.StrategyDefaults) > 0 {
		clusterCfg.StrategyDefaults = make([]mesh.StrategyDefault, len(cfg.Cluster.StrategyDefaults))
		for i, sd := range cfg.Cluster.StrategyDefaults {
			clusterCfg.StrategyDefaults[i] = mesh.StrategyDefault{
				PathPrefix: sd.PathPrefix,
				Default:    sd.Default,
			}
		}
	}

	// Create cluster with reconnection callback to refresh routing table/cache
	clusterCoordinator, err := mesh.NewCluster(clusterCfg, localNodeURL, s, func(hostURL string) {
		slog.Info("Cluster node reconnected, refreshing cache", "node", hostURL)
		s.model.Cache.RefreshAsync()
		// A node reaching StatusUp is the first moment it can be told
		// anything, and it may have missed every config push it was
		// down for. Fires on the first probe after this coordinator
		// starts too, which is what makes an edit-then-restart converge
		// in seconds rather than at the next maintenance tick.
		s.reconcileProviderTree("node reconnected: " + hostURL)
	}, connector)
	if err != nil {
		slog.Warn("Warning: Failed to initialize unified cluster", "cluster", err)
	} else {
		s.cluster.coordinator = clusterCoordinator
	}
}

// initHTTPEngine creates the gin engine and installs all middleware chains.
func (s *Server) initHTTPEngine(cfg *pkgConfig.NodeConfig) {
	s.engine = gin.New()
	// HandleMethodNotAllowed makes gin's NoMethod handler fire when
	// a request hits a registered path with an unregistered method
	// (e.g. DELETE on /v1/chat/completions where only POST + GET are
	// registered). Without this flag the request falls through to
	// NoRoute and returns 404, which breaks SDK retry classifiers
	// that branch on 405 vs 404.
	s.engine.HandleMethodNotAllowed = true
	s.engine.Use(
		customLogger(),
		dropCoordinatorHeaders(),
		httperr.AttachByPath(s.responders.dispatcher),
		// GroupAwareRecovery replaces gin.Recovery. On panic it looks
		// up the correct dialect via the PathDispatcher and emits a
		// 500 through that dialect's Responder.Internal method, so
		// an /v1/* panic returns the OpenAI envelope instead of
		// HTML. The raw panic value is logged server-side keyed by
		// request_id and never echoed to the client.
		httperr.GroupAwareRecovery(s.responders.dispatcher, func(reqID string, panicValue any, stack []byte) {
			slog.Error("panic recovered",
				"request_id", reqID,
				"panic", panicValue,
				"stack", string(stack),
			)
		}),
	)

	// Install dialect-aware handlers for unmatched routes so 404/405
	// on /v1/* returns the OpenAI envelope, /api/* returns the flat
	// Ollama shape, and everything else falls through to the
	// Problem Details fallback. Public /zzrouter/v1/* routes on a
	// worker trip workerManagementMisdirected so operators get a
	// helpful pointer at the coord instead of a bare 404.
	s.engine.NoRoute(func(c *gin.Context) {
		if workerManagementMisdirected(c, s) {
			return
		}
		r := s.responders.dispatcher.For(c.Request.URL.Path)
		r.NotFound(c, fmt.Sprintf("endpoint not found: %s %s", c.Request.Method, c.Request.URL.Path))
	})
	s.engine.NoMethod(func(c *gin.Context) {
		r := s.responders.dispatcher.For(c.Request.URL.Path)
		r.MethodNotAllowed(c, fmt.Sprintf("method %s not allowed for %s", c.Request.Method, c.Request.URL.Path))
	})

	// Initialize OpenTelemetry observability if enabled
	if cfg.Observability.IsEnabled() {
		otelCfg := &observability.Config{
			Enabled:     cfg.Observability.Enabled,
			ServiceName: cfg.Observability.ServiceName,
			OTLP: observability.OTLPConfig{
				Endpoint: cfg.Observability.OTLP.Endpoint,
				Insecure: cfg.Observability.OTLP.Insecure,
				Headers:  cfg.Observability.OTLP.Headers,
			},
			Tracing: observability.TracingConfig{
				Enabled:    cfg.Observability.Tracing.Enabled,
				SampleRate: cfg.Observability.Tracing.SampleRate,
			},
			Metrics: observability.MetricsConfig{
				Enabled:        cfg.Observability.Metrics.Enabled,
				PrometheusPort: cfg.Observability.Metrics.PrometheusPort,
				ExportInterval: cfg.Observability.Metrics.ExportInterval,
			},
		}
		if otelCfg.ServiceName == "" {
			otelCfg.ServiceName = "zzrouter"
		}

		ctx := context.Background()
		otelProvider, err := observability.NewProvider(ctx, otelCfg, version.Current.String())
		if err != nil {
			logger.Warn("Failed to initialize OpenTelemetry observability", "error", err)
		} else {
			s.otelProvider = otelProvider
			logger.Info("OpenTelemetry observability enabled",
				"service_name", otelCfg.ServiceName,
				"tracing", otelCfg.Tracing.Enabled,
				"metrics", otelCfg.Metrics.Enabled,
			)

			if otelCfg.IsTracingEnabled() {
				s.engine.Use(observability.TracingMiddleware(otelCfg.ServiceName))
			}
			if otelCfg.IsMetricsEnabled() {
				s.engine.Use(observability.MetricsMiddleware())
				// zz.proxy.failed.requests.metric — fires on every
				// 4xx/5xx response. Sits beside MetricsMiddleware so
				// the failure counter shares the otel-enabled gate
				// rather than being unconditionally registered.
				s.engine.Use(proxy.Middleware())
				// zz.proxy.total.requests.metric — fires on every LLM-
				// route response regardless of outcome, with status_code
				// label. Together with the failed counter above and a
				// PromQL division, this gives the LiteLLM dashboard
				// trio (total / failed / failure-rate).
				s.engine.Use(obsspend.TotalRequestsMiddleware())
			}
		}
	}

	// SECURITY: Add request size limit middleware to prevent memory exhaustion attacks
	maxRequestSize := int64(defaultMaxRequestSize)
	if cfg.Node.MaxRequestSize > 0 {
		maxRequestSize = cfg.Node.MaxRequestSize
	}
	s.engine.Use(requestSizeLimitMiddleware(maxRequestSize))
	s.engine.Use(requestBodyReadDeadlineMiddleware(2 * time.Minute))

	// CORS policy runs before everything auth-related and before
	// ClusterModeGate so preflights to gated routes still get a 204
	// with the right headers even when the node is not in an allowed
	// role for the real request. Zero-cost when disabled.
	s.engine.Use(CORSMiddleware(cfg.Security.CORS))

	// Register cluster detection middleware (detects cluster-internal requests)
	// Must be registered BEFORE authentication to populate context for auth checks
	s.engine.Use(ClusterDetectionMiddleware())
}

// initClusterNetworking sets up mTLS cluster listener, resource tracker, and routing.
func (s *Server) initClusterNetworking(cfg *pkgConfig.NodeConfig) error {
	// clusterResourceGroup pairs resource tracking + mDNS discovery
	// behind a single Start/Stop. Server retains access-handle pointers
	// for downstream wiring — ownership of the lifecycle belongs to the
	// group. The authoritative cluster HTTP subsystem (pkg/clusternode)
	// lands alongside this group as a separate field in a later commit.
	s.cluster.resources = mesh.NewResourceTracker(cfg.Node.Name, constants.ResourceTrackerInterval)
	utils.LogDebugf("[ResourceTracker] Initialized for node: %s", cfg.Node.Name)

	// Build the mTLS cluster HTTP subsystem FIRST. Construction loads
	// (or generates) the node identity keypair + certs and, for
	// coordinators, the CA — cheap side effects, no listener bound.
	// Discovery is constructed after so the coordinator's CA SPKI
	// fingerprint can be injected into its mDNS TXT records; workers
	// and discovery-only callers pass an empty fingerprint.
	clusterListenerCfg, err := buildClusternodeConfig(cfg)
	if err != nil {
		return fmt.Errorf("cluster listener config: %w", err)
	}
	// Mirror clusternode runtime mode transitions into role.Manager so
	// role.Current() is the single source of truth at the Server
	// level. Fires on pairing completion and revert-to-unclaimed; see
	// pkg/cluster/node.Config.OnModeChange.
	clusterListenerCfg.OnModeChange = s.onClusterModeChange
	clusterListener, err := clusternode.New(clusterListenerCfg)
	if err != nil {
		return fmt.Errorf("cluster listener: %w", err)
	}
	s.cluster.listener = clusterListener

	if cfg.MDNSDiscovery {
		s.cluster.discovery = network.NewNodeDiscovery(
			cfg.Node.Name,
			cfg.Cluster.IsCoordinator(),
			cfg.Cluster.BindPort,
		)
	}
	s.cluster.node = newClusterResourceGroup(s.cluster.resources, s.cluster.discovery, cfg.Node.Port)

	// Build the mTLS-enabled connector used for coordinator→worker
	// /internal/* dispatch. Captured at construction so the Connector
	// is immutable post-NewCluster (no setter-race with the health
	// monitor goroutine).
	var coordConnector *mesh.Connector
	if cfg.Cluster.IsMaster() && clusterListener.Mode() == clusternode.Coordinator {
		dispatchClient, err := clusterListener.DialClient(clusterid.RoleWorker.OU())
		if err != nil {
			return fmt.Errorf("build cluster dispatch client: %w", err)
		}
		coordConnector = mesh.NewConnector(mesh.ConnectorConfig{
			HTTPClient:  dispatchClient,
			ClusterPort: cfg.Cluster.BindPort,
		})
	}
	s.tryInitClusterCoordinator(cfg, coordConnector)

	// Initialize resource-aware router
	s.cluster.resRouter = routing.NewResourceAwareRouter(cfg.Node.Name, &cfg.Coordinator.Routing)
	utils.LogDebugf("[ResourceRouter] Initialized with mode: %s", cfg.Coordinator.Routing.GetDefaultMode())

	// Initialize HTTP-based routing
	utils.LogDebugf("[Routing] Initializing request router...")
	serverPort := fmt.Sprintf("%d", s.config.Node.Port)
	initialRouter := s.buildRouterForMode(s.config.Cluster.IsWorker(), serverPort)
	s.cluster.routerSwap = routing.NewSwappable(initialRouter)
	s.cluster.router = s.cluster.routerSwap
	s.nodeConfigStore.OnChange("reconfigureRouter", s.reconfigureRouter)
	s.nodeConfigStore.OnChange("handleClusterModeReload", s.handleClusterModeReload)
	s.role.Subscribe(s.handleRoleTransition)

	return nil
}

// initFallbackProxy creates cooldown managers, rate limit tracker, and the
// fallback proxy with routing strategies for model group routing.
func (s *Server) initFallbackProxy() {
	s.providers.cooldowns = fallback.NewCooldownSet()
	s.providers.rateTracker = fallback.NewRateLimitTracker()

	if s.model.Groups == nil {
		return
	}

	loadTracker := fallback.NewLoadTracker()
	latencyTracker := fallback.NewLatencyTracker(100) // 100-sample rolling window

	// Register routing strategies
	strategies := fallback.NewStrategyRegistry()
	strategies.Register(&fallback.PriorityStrategy{})
	strategies.Register(fallback.NewLeastLoadStrategy(loadTracker))
	strategies.Register(fallback.NewFastestStrategy(latencyTracker))

	// Build health check targets from model group configurations
	var healthChecker *fallback.HealthChecker
	if targets := s.buildHealthTargets(); len(targets) > 0 {
		healthChecker = fallback.NewHealthChecker(targets)
	}

	s.providers.loadTracker = loadTracker
	s.providers.latencyTracker = latencyTracker
	s.providers.healthChecker = healthChecker
	s.providers.routeEvents = route_events.NewBus()
	if s.model.Groups != nil {
		s.model.Groups.SetEventBus(s.providers.routeEvents)
	}
	if s.model.AutoRoute != nil {
		s.model.AutoRoute.SetEventBus(s.providers.routeEvents)
	}
	wireRouteEventEmitters(s.providers.routeEvents, s.providers.cooldowns.Deployments(), healthChecker)
	s.providers.fallback = chain.New(newChainServerDeps(s), s.providers.cooldowns.Deployments(), s.providers.cooldowns.Providers(), strategies, loadTracker, latencyTracker, healthChecker)

	// Wire latency tracker to inference log bridge for the fastest strategy
	if s.inference.logBridge != nil {
		s.inference.logBridge.SetLatencyTracker(latencyTracker)
	}
}

// wireRouteEventEmitters bridges the string-typed callbacks the tracker
// packages (pkg/fallback) accept into route_events.Event values fanned
// onto bus. Lives in internal/server because the translation layer is
// the only consumer that knows about both the trackers and the bus —
// keeping pkg/fallback free of an observability import that would
// invite a cycle.
func wireRouteEventEmitters(bus *route_events.Bus, cooldowns *fallback.CooldownManager, health *fallback.HealthChecker) {
	if bus == nil {
		return
	}
	if cooldowns != nil {
		cooldowns.SetEventEmitter(func(eventType, replica, reason string) {
			et := route_events.EventCooldownStarted
			if eventType == "cooldown_ended" {
				et = route_events.EventCooldownEnded
			}
			detail := map[string]string{"replica": replica}
			if reason != "" {
				detail["reason"] = reason
			}
			bus.Publish(route_events.Event{Type: et, Replica: replica, Detail: detail})
		})
	}
	if health != nil {
		health.SetEventEmitter(func(replica, healthState string) {
			bus.Publish(route_events.Event{
				Type:    route_events.EventHealthChanged,
				Replica: replica,
				Detail:  map[string]string{"health_state": healthState},
			})
		})
	}
}

// cachePeerName records a peer's own node name in node.yaml as probes
// resolve it.
//
// The registry learns a name only by probing, so between a restart and
// the first successful probe GET /nodes has nothing to report but the
// endpoint URL — and a URL is rejected by every `node` selector in the
// API. Caching it means the name survives the gap.
//
// Best-effort: this runs on the probe path, and failing to cache a name
// must never take a peer out of the cluster. The store no-ops when the
// name is unchanged, so the steady state writes nothing.
func (s *Server) cachePeerName(hostURL, nodeName string) {
	if s.nodeConfigStore == nil {
		return
	}
	parsed, err := url.Parse(hostURL)
	if err != nil || parsed.Host == "" {
		return
	}
	changed, err := s.nodeConfigStore.SetClusterEndpointName(parsed.Host, nodeName)
	if err != nil {
		slog.Warn("could not cache peer name", "node", nodeName, "address", parsed.Host, "err", err)
		return
	}
	if changed {
		slog.Info("cached peer name", "node", nodeName, "address", parsed.Host)
	}
}

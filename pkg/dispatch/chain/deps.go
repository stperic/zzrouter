// Package chain routes requests through candidate backends with replay-safe retries.
//
// The package is "gin-tolerant" — ProxyWithFallback consumes a
// *gin.Context for the hot request path because the exhausted-deployment
// error renderer writes a gin.H JSON envelope. Fully gin-free is a
// future refactor; the value of this extraction is breaking the opaque
// *Server pointer into the three narrow dependency interfaces below.
package chain

import (
	"context"
	"net/http"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// Topology captures the cluster-shape questions the Proxy asks during a
// request: who am I, is this node me, where does a remote node live, and
// how much memory does each node have available.
type Topology interface {
	// NodeName returns the local node's advertised hostname.
	NodeName() string

	// IsLocalNode reports whether the given nodename refers to this
	// process.
	IsLocalNode(node string) bool

	// ClusterURL returns the base URL of a node's cluster port
	// (https://host:port), the only surface a worker serves inference on,
	// or empty when the node is unknown.
	ClusterURL(node string) string

	// NodeMetrics returns the cached resource snapshot for the named
	// node, or nil when no metrics are available. Consulted for the
	// memory-fitness filter before attempting an on-demand start on a
	// remote node.
	NodeMetrics(node string) *mesh.ResourceMetrics

	// NodeCacheReady reports whether the node resource cache is wired up.
	// False on standalone installs, during early startup, and in tests
	// that don't wire a cache. Lets the memory-fitness filter
	// short-circuit the per-candidate NodeMetrics loop when no metrics
	// will ever come back. Matches the pre-extraction short-circuit at
	// fallback_proxy.go's old `f.server.model.NodeCache == nil` check.
	NodeCacheReady() bool
}

// ProviderConfig exposes the static provider metadata the Proxy needs
// per deployment attempt.
type ProviderConfig interface {
	FailureClassifier(app string) *httperr.Classifier
	// Backend resolves a provider (app) that has an endpoint of its own
	// (cloud, external, service) to that endpoint and what of the
	// caller's request travels there. On-demand providers return false.
	Backend(app string) (*backend.Resolved, bool)

	// RequestDefaults returns the request-body defaults the provider's
	// config sets for model on a request to path, applied to fields the
	// client did not send. Nil when there are none.
	RequestDefaults(app, model, path string) map[string]any

	// InjectUsageMetadata reports the coordinator-routing config flag
	// controlling whether zz_* fields get injected into streaming usage
	// chunks.
	InjectUsageMetadata() bool
}

// Runtime exposes the behavioural collaborators the Proxy delegates to
// during a single request.
type Runtime interface {
	BufferedShape(meta wire.RoutingMetadata, recorder *llm.InferenceRecorder) func([]byte) []byte

	// FinishRelay reports a truncated relay in the client dialect.
	FinishRelay(w http.ResponseWriter, req *http.Request, resp *http.Response, res wire.Result)

	// ProxyForwardDetached sends the outbound request to the backend
	// but does NOT begin writing to the client. The caller inspects the
	// response status and decides whether to commit or fall back.
	// Failed sends preserve write evidence in fallback.RequestError.
	ProxyForwardDetached(ctx context.Context, method, url string, headers http.Header, up backend.Upstream, body []byte) (*http.Response, error)

	// Normalizers returns the response-normalizer registry used by the
	// commit helpers in pkg/dispatch/wire.
	Normalizers() *normalizer.Registry

	// ValidateModel admits model before alias resolution, warm reuse or forwarding.
	ValidateModel(ctx context.Context, model string) error

	// LoadFailure shares the host's inference error mapping for lifecycle refusals.
	LoadFailure(error) httperr.Error

	// AppInstance returns model's run on provider, any status; callers filter.
	AppInstance(model, provider, endpoint string) (*instance.Instance, bool)

	// LocalRun returns the ready run of a local model, starting one; nil for a provider without runs.
	LocalRun(model, provider, endpoint string) (*instance.Instance, error)

	// RunUpstream returns where a run's requests go and what of the caller's request travels there.
	RunUpstream(inst *instance.Instance) (string, backend.Upstream, error)

	// LeaseInstance shares local request admission with the direct instance paths.
	LeaseInstance(req *http.Request, inst *instance.Instance) (*instance.Lease, error)

	// InstanceBody prepares each attempt through the host's instance request path.
	InstanceBody(req *http.Request, lease *instance.Lease, body []byte) []byte

	// StartOnDemand returns immediate refusals before the manager owns the launch.
	StartOnDemand(model, provider, endpoint string) error

	// TrackProviderRateLimit consumes a backend response and updates the
	// provider-level rate-limit snapshot used by the status API.
	TrackProviderRateLimit(app string, resp *http.Response)
}

// ServerDeps is the composed contract the Proxy requires from its host.
// Splitting into three subsystem interfaces makes test fakes auditable
// per-subsystem (see fallback_test.go); the composition form is what
// New() accepts.
type ServerDeps interface {
	Topology
	ProviderConfig
	Runtime
}

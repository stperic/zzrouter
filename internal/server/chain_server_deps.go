// Server-side implementation of chain.ServerDeps.
//
// Splits the method surface that chain.Proxy consumes out of the opaque
// *Server pointer into three composed interfaces (Topology,
// ProviderConfig, Runtime) — see pkg/dispatch/chain/deps.go for the
// contract. Adding a method here MUST be reflected in deps.go; the
// interface is intentionally narrow to keep the boundary auditable.

package server

import (
	"context"
	"net/http"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// chainServerDeps wraps *Server as a chain.ServerDeps. One instance is
// created during server construction and passed to chain.New.
type chainServerDeps struct {
	s *Server
}

func newChainServerDeps(s *Server) *chainServerDeps {
	return &chainServerDeps{s: s}
}

// ===== Topology =====

func (d *chainServerDeps) NodeName() string {
	return d.s.node.Nodename()
}

func (d *chainServerDeps) IsLocalNode(node string) bool {
	return d.s.node.IsLocalNode(node)
}

func (d *chainServerDeps) ClusterURL(node string) string {
	return d.s.resolveNodeToClusterURL(node)
}

func (d *chainServerDeps) NodeMetrics(node string) *mesh.ResourceMetrics {
	if d.s.model.NodeCache == nil {
		return nil
	}
	return d.s.model.NodeCache.Get(node)
}

func (d *chainServerDeps) NodeCacheReady() bool {
	return d.s.model.NodeCache != nil
}

// ===== ProviderConfig =====

func (d *chainServerDeps) Backend(app string) (*backend.Resolved, bool) {
	return d.s.backend.Resolve(app)
}

func (d *chainServerDeps) RequestDefaults(app, model, path string) map[string]any {
	return d.s.requestDefaults(context.Background(), app, model, path)
}

func (d *chainServerDeps) InjectUsageMetadata() bool {
	return d.s.config.Coordinator.Routing.InjectUsageMetadata
}

// ===== Runtime =====

// ProxyForwardDetached preserves the send error and its request-write evidence.
func (d *chainServerDeps) ProxyForwardDetached(ctx context.Context, method, url string, headers http.Header, up backend.Upstream, body []byte) (*http.Response, error) {
	return d.s.proxy.ForwardDetached(ctx, method, url, headers, up, body)
}

func (d *chainServerDeps) Normalizers() *normalizer.Registry {
	return d.s.inference.normalizers
}

func (d *chainServerDeps) ValidateModel(ctx context.Context, model string) error {
	return d.s.validateModel(ctx, model)
}

func (*chainServerDeps) LoadFailure(err error) httperr.Error { return loadFailureWire(err) }

func (d *chainServerDeps) AppInstance(model, provider, endpoint string) (*instance.Instance, bool) {
	if d.s.providers.appMgr == nil {
		return nil, false
	}
	// Resolved as a start resolves it: an alias, a variant's base or a daemon's tag finds the run.
	return d.s.providers.appMgr.GetInstanceByModel(provider, model, prov_apps.Endpoint(endpoint))
}

// LocalRun is the ready run serving a local model, or nil when its provider serves it without one.
func (d *chainServerDeps) LocalRun(model, provider, endpoint string) (*instance.Instance, error) {
	if !d.s.hasRuns(provider) {
		return nil, nil //nolint:nilnil // no run is the answer for a provider without runs
	}
	return d.s.loadModelViaNodeAPI(model, provider, endpoint)
}

func (d *chainServerDeps) RunUpstream(inst *instance.Instance) (string, backend.Upstream, error) {
	return d.s.providers.appMgr.Upstream(inst)
}

func (d *chainServerDeps) LeaseInstance(req *http.Request, inst *instance.Instance) (*instance.Lease, error) {
	return d.s.openInstance(req, inst)
}

func (d *chainServerDeps) InstanceBody(req *http.Request, lease *instance.Lease, body []byte) []byte {
	req = req.WithContext(context.WithValue(req.Context(), CtxKeyOriginalBody, body))
	prepared, _ := d.s.instanceRequest(req, lease.Instance())
	return prepared
}

func (d *chainServerDeps) StartOnDemand(model, provider, endpoint string) error {
	_, err := d.s.providers.appMgr.EnsureRun(context.Background(), prov_apps.EnsureRequest{Model: model, Provider: provider, Endpoint: prov_apps.Endpoint(endpoint), Detached: true, Origin: prov_apps.OriginRequest})
	return err
}

func (d *chainServerDeps) TrackProviderRateLimit(app string, resp *http.Response) {
	d.s.trackProviderRateLimit(app, resp)
}

func (d *chainServerDeps) FailureClassifier(app string) *httperr.Classifier {
	return d.s.failureClassifier(app)
}

func (*chainServerDeps) FinishRelay(w http.ResponseWriter, req *http.Request, resp *http.Response, res wire.Result) {
	if isStreamingResponse(resp) {
		endBrokenStream(w, req, res)
	} else {
		failBrokenBody(w, req, res)
	}
}

func (d *chainServerDeps) BufferedShape(meta wire.RoutingMetadata, recorder *llm.InferenceRecorder) func([]byte) []byte {
	return normalizedBody(d.s.inference.normalizers, meta.Provider, meta, recorder)
}

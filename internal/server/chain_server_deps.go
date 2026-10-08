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
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/dispatch/normalizer"
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

func (d *chainServerDeps) ProxyForwardDetached(ctx context.Context, method, url string, headers http.Header, up backend.Upstream, body []byte) (*http.Response, error) {
	return d.s.proxy.ForwardDetached(ctx, method, url, headers, up, body)
}

func (d *chainServerDeps) Normalizers() *normalizer.Registry {
	return d.s.inference.normalizers
}

func (d *chainServerDeps) ValidateModel(ctx context.Context, model string) error {
	return d.s.validateModel(ctx, model)
}

func (d *chainServerDeps) AppInstance(model string) (*instance.Instance, bool) {
	if d.s.providers.appMgr == nil {
		return nil, false
	}
	// Canonicalized like every by-name lookup, so an alias, or a variant
	// its base's process serves, finds the run.
	return d.s.providers.appMgr.GetInstanceByModel(model)
}

func (d *chainServerDeps) InstanceRequest(req *http.Request, inst *instance.Instance, body []byte) []byte {
	req = req.WithContext(context.WithValue(req.Context(), CtxKeyOriginalBody, body))
	prepared, _ := d.s.instanceRequest(req, inst)
	return prepared
}

func (d *chainServerDeps) NewLoadExecutor() chain.LoadExecutor {
	// Each call returns a fresh adapter closure. The underlying Server-
	// side LoadExecutor is constructed per call to match the pre-
	// extraction behaviour of fallback_proxy's startModelAsync.
	return func(ctx context.Context, model, provider string) error {
		exec := d.s.newLoadExecutor()
		_, err := exec.LoadLocalModel(ctx, &LoadModelRequest{
			ModelName: model,
			Provider:  provider,
		})
		return err
	}
}

func (d *chainServerDeps) TrackProviderRateLimit(app string, resp *http.Response) {
	d.s.trackProviderRateLimit(app, resp)
}

// Package backend resolves a provider key to its upstream endpoint and
// optional auth config. Reads from a live AppsConfig closure so callers
// always see the latest provider config state.
package backend

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// Resolver resolves a provider key to its upstream endpoint and optional
// API config. It reads from the live AppsConfig via a closure so it always
// sees the latest provider config state.
type Resolver struct {
	appsConfig func() *config.AppsConfig
}

// NewResolver constructs a Resolver.
func NewResolver(appsConfig func() *config.AppsConfig) *Resolver {
	return &Resolver{appsConfig: appsConfig}
}

// Resolved is the result of looking up a provider for pass-through
// forwarding. Endpoint is the base URL (scheme://host[:port]); callers
// suffix the client's request path at dispatch time. Upstream says what
// of the caller's request travels there. Cloud marks a hosted API, where
// no node of the cluster serves the request.
type Resolved struct {
	Endpoint string
	Upstream Upstream
	Cloud    bool
}

// NewRequest builds a request zzRouter makes on its own to path on the
// resolved endpoint. There is no caller, so it carries only the
// provider's own credential, when it declares one.
func (r *Resolved) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.Endpoint, "/")+path, body)
	if err != nil {
		return nil, err
	}
	maps.Copy(req.Header, r.Upstream.Header())
	return req, nil
}

// Resolve looks up a provider by key and returns a Resolved ready for
// forwarding. Returns (nil, false) when the provider is not registered,
// not enabled, or has no endpoint.
func (r *Resolver) Resolve(providerKey string) (*Resolved, bool) {
	cfg := r.appsConfig()
	if cfg == nil || providerKey == "" {
		return nil, false
	}
	providerCfg, exists := cfg.LookupApp(providerKey)
	if !exists || !providerCfg.IsEnabled() {
		return nil, false
	}
	return Target(&providerCfg)
}

// Target is Resolve for a provider config in hand, enabled or not, for
// the calls zzRouter makes before or regardless of enabling it.
func Target(svc *config.ServiceConfig) (*Resolved, bool) {
	if svc == nil || !svc.HasEndpoint() || svc.Runtime == nil || svc.Runtime.Endpoint == "" {
		return nil, false
	}
	if svc.IsCloudProvider() {
		return &Resolved{
			Endpoint: config.NormalizeEndpoint(svc.Runtime.Endpoint),
			Upstream: Credential(svc.Runtime.API),
			Cloud:    true,
		}, true
	}
	return &Resolved{Endpoint: svc.Runtime.Endpoint, Upstream: ForProvider(svc.Runtime.API)}, true
}

// PassthroughTarget composes the upstream URL for a pass-through proxy hop:
// trims any trailing slash on the base, appends the client's request path,
// then the raw query string when present.
func PassthroughTarget(base, path, rawQuery string) string {
	url := strings.TrimRight(base, "/") + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	return url
}

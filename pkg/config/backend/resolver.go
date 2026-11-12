// Package backend resolves a provider key to its upstream endpoint and
// optional auth config. Reads from a live AppsConfig closure so callers
// always see the latest provider config state.
package backend

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
)

// Resolver resolves a provider key to its upstream endpoint and optional
// API config. It reads from the live AppsConfig via a closure so it always
// sees the latest provider config state.
type Resolver struct {
	appsConfig func() *config.AppsConfig
	records    ConnectionRecords
}

// NewResolver constructs a Resolver.
func NewResolver(appsConfig func() *config.AppsConfig, sources ...ConnectionRecords) *Resolver {
	r := &Resolver{appsConfig: appsConfig}
	if len(sources) > 0 {
		r.records = sources[0]
	}
	return r
}

// Resolved is the result of looking up a provider for pass-through
// forwarding. Endpoint is the base URL (scheme://host[:port]); callers
// suffix the client's request path at dispatch time. Upstream says what
// of the caller's request travels there. Cloud marks a hosted API, where
// no node of the cluster serves the request.
type Resolved struct {
	Endpoint          string
	Upstream          Upstream
	Cloud             bool
	Configured        bool
	Revision          string
	ModelsPath        string
	ModelsResponseKey string
	TestPath          string
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
	svc, record, values, err := r.connection(providerKey)
	if err != nil || !svc.IsEnabled() {
		return nil, false
	}
	target := connectionTarget(svc, record, values)
	if target == nil || target.Cloud && !target.Configured {
		return nil, false
	}
	return target, true
}

// Target is Resolve for a provider config in hand, enabled or not, for
// the calls zzRouter makes before or regardless of enabling it.
func Target(svc *config.ServiceConfig) (*Resolved, bool) {
	if svc == nil {
		return nil, false
	}
	values := map[string]string{}
	if svc.Connection != nil {
		for _, f := range svc.Connection.Fields {
			values[f.ID] = externalValue(f)
		}
	}
	target := connectionTarget(svc, secrets.Record{Mode: secrets.External}, values)
	return target, target != nil
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

// SameEndpoint compares physical endpoints without folding case-sensitive URL paths.
func SameEndpoint(a, b string) bool {
	normalize := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return raw
		}
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = strings.TrimRight(u.RawPath, "/")
		return u.String()
	}
	return a != "" && b != "" && normalize(a) == normalize(b)
}

package backend

import (
	"net/http"

	"github.com/stperic/zzrouter/pkg/config"
)

// Upstream is where a forwarded request lands, which decides what of the
// caller's request travels with it. Every forward is built from one. The
// zero value is Engine, which sends no credential.
type Upstream struct {
	hop hop
	api *config.APIConfig
}

type hop uint8

const (
	hopEngine hop = iota
	hopCallerKey
	hopCredential
	hopCluster
)

// Engine is an endpoint that takes no credential: a run zzRouter launched,
// or a provider that declares auth_type none. The caller's key is for
// zzRouter and stops there.
func Engine() Upstream { return Upstream{hop: hopEngine} }

// CallerKey is an endpoint someone else runs that checks the caller's own
// key, so that key travels. Nothing else of zzRouter's does.
func CallerKey() Upstream { return Upstream{hop: hopCallerKey} }

// Credential is an endpoint given the provider's own credential in place
// of the caller's.
func Credential(api *config.APIConfig) Upstream {
	return Upstream{hop: hopCredential, api: api}
}

// Cluster is a worker's cluster engine, reached over mTLS. Everything
// travels: the worker dispatches on the routing hints, and a provider it
// serves may check the caller's key.
func Cluster() Upstream { return Upstream{hop: hopCluster} }

// ForProvider returns the upstream a provider's endpoint is, as its
// runtime.api declares. No api block means CallerKey.
func ForProvider(api *config.APIConfig) Upstream {
	switch {
	case api == nil || api.AuthType == config.AuthTypeCaller:
		return CallerKey()
	case api.AuthType == config.AuthTypeNone:
		return Engine()
	default:
		return Credential(api)
	}
}

// IsCluster reports whether the upstream is a worker's cluster engine.
func (u Upstream) IsCluster() bool { return u.hop == hopCluster }

// ForwardsCallerKey reports whether the caller's own key, and no other
// zzRouter key, travels. A Cluster upstream carries everything instead.
func (u Upstream) ForwardsCallerKey() bool { return u.hop == hopCallerKey }

// API returns the credential the upstream is given, or nil.
func (u Upstream) API() *config.APIConfig { return u.api }

// Header returns the headers that carry the upstream's own credential,
// empty when it is sent none. Every request zzRouter sends upstream
// takes its credential from here.
func (u Upstream) Header() http.Header {
	h := http.Header{}
	u.api.ApplyAuthHeaders(h)
	return h
}

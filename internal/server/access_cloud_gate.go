// access_cloud_gate.go — the server-side answer to "is this model
// cloud-backed", handed to AccessControl as a closure so the access
// layer keeps no dependency on the model cache or provider config.

package server

import (
	"context"

	"github.com/stperic/zzrouter/pkg/constants"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
)

// modelIsCloudBacked reports whether a model name resolves to a provider
// that serves from a third-party API rather than from this cluster.
//
// Answers false whenever the answer is not knowable — an unpopulated
// cache, an unknown model, a provider that is not registered. That is
// deliberate: this predicate only ever gates anonymous traffic, and a
// model nobody can resolve will fail in dispatch a moment later with a
// far better error than "authenticate first". Refusing on uncertainty
// would turn every cold-cache miss into a spurious 401.
func (s *Server) modelIsCloudBacked(ctx context.Context, modelName string) bool {
	// s.model is a pointer subsystem, so it is checked before any field
	// on it is read.
	if s == nil || s.model == nil || modelName == "" {
		return false
	}
	// A model group is checked first, and by name: the gate runs before
	// the resolver, so what arrives here is whatever the caller typed. A
	// group alias is not a cache entry, so looking only in the cache
	// would miss it and wave the request through to whatever the group
	// fans out to -- which is how an anonymous caller reached a cloud
	// replica by naming the group instead of the model.
	// Nil-checked on the concrete type: a typed-nil store handed to an
	// interface parameter is not itself nil.
	if s.model.Groups != nil && groupHasCloudReplica(s.model.Groups, modelName, s.providerIsCloud) {
		return true
	}
	if s.model.Cache == nil {
		return false
	}
	// Bounded, but derived from the request: LookupModel can trigger a
	// full cache populate, and a caller that has hung up should not leave
	// a scan running inside an auth gate.
	ctx, cancel := context.WithTimeout(ctx, constants.HTTPShortTimeout)
	defer cancel()

	m, err := s.model.Cache.LookupModel(ctx, modelName)
	if err != nil || m == nil {
		return false
	}
	if m.IsCloud {
		return true
	}
	return s.providerIsCloud(m.Provider)
}

// groupHasCloudReplica reports whether name is a model group with at
// least one cloud-backed replica. Any is enough: the strategy decides
// which replica serves a given call, so one cloud replica can bill the
// operator on any request to the group.
//
// Takes the reader and the provider predicate rather than a *Server so
// the rule can be exercised without standing up a group store.
func groupHasCloudReplica(groups modelgroup.GroupReader, name string, isCloud func(string) bool) bool {
	g := groups.Get(name)
	if g == nil {
		return false
	}
	for _, r := range g.Replicas {
		if isCloud(r.App) {
			return true
		}
	}
	return false
}

// providerIsCloud reports whether a provider key names a cloud provider.
func (s *Server) providerIsCloud(provider string) bool {
	cfg := s.appsConfig
	if cfg == nil || provider == "" {
		return false
	}
	svc, ok := cfg.LookupApp(provider)
	return ok && svc.IsCloudProvider()
}

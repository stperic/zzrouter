// Package model holds the model-domain state (pricing, cluster-join
// cache, model registry, groups, resolver, verifier, auto-router,
// download tracker). Lifecycle hooks live on the leaf subsystems;
// the server's coordinator Start/Stop group calls them directly.
package model

import (
	"github.com/stperic/zzrouter/pkg/model/autoroute"
	"github.com/stperic/zzrouter/pkg/model/cache"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	modelcache "github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// Subsystem is a passive field holder for the model-domain. Composed
// fields may be nil when the underlying feature is disabled in config.
// The server drives lifecycle per-leaf (Pricing.Start/Stop in the
// coordinator group, Cache.Start/Stop + Downloads.Stop in the common
// group).
type Subsystem struct {
	Pricing   *pricing.Store
	Cache     *cache.Cache
	NodeCache *cache.NodeResourceCache

	Registry  *modelregistry.Registry
	Groups    *modelgroup.GroupStore
	Resolver  resolver.Resolver
	Verifier  *modelcache.IntegrityVerifier
	AutoRoute *autoroute.Manager

	Downloads *modelregistry.DownloadTracker
}

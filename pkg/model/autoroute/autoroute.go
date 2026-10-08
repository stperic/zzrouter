// Package autoroute automatically creates and maintains model groups
// (routes) when the same model name exists on multiple nodes or
// providers. Auto-created groups carry group.AutoManaged=true so the
// manager never modifies user-created groups.
//
// This package is part of the pkg/model domain consolidation; see
// docs/plan_model_subsystem_consolidation.md.
package autoroute

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/group"
	route_events "github.com/stperic/zzrouter/pkg/observability/route_events"
)

// Manager automatically creates and maintains model groups (routes)
// when the same model name exists on multiple nodes or providers.
// This gives users immediate load-balancing without manual route
// configuration.
type Manager struct {
	groupStore      *group.GroupStore
	routePrefix     string                      // prefix for auto-route names (e.g., "route-")
	invalidateCache func()                      // triggers model cache invalidation
	getAllModels    func() []*cache.CachedModel // reads all models from cache
	events          *route_events.Bus           // optional fan-out for auto_route_* lifecycle events
}

// New constructs an auto-route Manager. Callers supply:
//   - groupStore: the underlying group catalog to mutate.
//   - routePrefix: prepended to every auto-route key (e.g., "route-").
//   - invalidateCache: called from Sync* to trigger a cache refresh;
//     the refresh is the side-effect that actually drives reconciliation
//     (via SyncAllFromCache on the refresh tail).
//   - getAllModels: reads the current cache snapshot.
func New(
	groupStore *group.GroupStore,
	routePrefix string,
	invalidateCache func(),
	getAllModels func() []*cache.CachedModel,
) *Manager {
	return &Manager{
		groupStore:      groupStore,
		routePrefix:     routePrefix,
		invalidateCache: invalidateCache,
		getAllModels:    getAllModels,
	}
}

// SetEventBus wires a route_events.Bus so the manager fans out
// auto_route_generated / auto_route_revoked events on every sync.
// The underlying group.Store still publishes its own route_created /
// route_updated / route_deleted on the same mutation — agents that
// care about "is this auto-managed?" filter on the auto_route_*
// dimension; agents that just want lifecycle observe route_*.
func (m *Manager) SetEventBus(bus *route_events.Bus) {
	m.events = bus
}

func (m *Manager) publish(et route_events.EventType, name string) {
	if m.events == nil {
		return
	}
	m.events.Publish(route_events.Event{Type: et, Route: name})
}

// routeName returns the route key for a model, prepending the route prefix.
func (m *Manager) routeName(modelName string) string {
	return m.routePrefix + modelName
}

// IsAutoGroup reports whether a group was auto-created by this manager.
// Callers outside this package (e.g. the group CRUD controller) use it
// to refuse user mutations on auto-managed groups; the implicit-claim
// path flips AutoManaged=false on mutation so the predicate evolves
// without a separate ownership check.
func IsAutoGroup(g *group.ModelGroup) bool {
	return g != nil && g.AutoManaged
}

// replicaName generates a unique replica name from app and node.
// e.g., "ollama-node1", "ollama" (when node is local/empty).
func replicaName(app, node string) string {
	if node == "" {
		return app
	}
	return app + "-" + node
}

// buildReplicasFromInstances converts cached model instances into model group replicas,
// deduplicating by the generated replica name.
func buildReplicasFromInstances(instances []*cache.CachedModel) []group.Replica {
	replicas := make([]group.Replica, 0, len(instances))
	seen := make(map[string]bool)
	for _, inst := range instances {
		name := replicaName(inst.Provider, inst.Node)
		if seen[name] {
			continue
		}
		seen[name] = true
		replicas = append(replicas, group.Replica{
			Name:  name,
			Model: inst.Name,
			App:   inst.Provider,
			Node:  inst.Node,
		})
	}
	return replicas
}

// SyncAfterPull triggers a cache refresh so that SyncAllFromCache (which runs
// at the tail of RefreshCacheSync) detects the new model and creates routes
// if needed. The model/repo/node identity of the freshly-pulled model is not
// consulted — reconciliation walks the whole post-refresh cache.
func (m *Manager) SyncAfterPull() {
	if m == nil || m.invalidateCache == nil {
		return
	}
	m.invalidateCache()
}

// SyncAfterDelete triggers a cache refresh so that SyncAllFromCache cleans up
// routes for deleted models.
func (m *Manager) SyncAfterDelete(modelName string) {
	if m == nil || m.invalidateCache == nil {
		return
	}
	m.invalidateCache()
}

// SyncAllFromCache scans the already-populated model cache for models that exist on
// multiple nodes/providers and creates or removes auto-routes accordingly.
// Called from refreshCacheSync — must NOT call refreshCacheSync itself.
// Skips disk writes when nothing changed (no-op on steady state).
func (m *Manager) SyncAllFromCache() {
	if m == nil || m.getAllModels == nil || m.groupStore == nil {
		return
	}

	allModels := m.getAllModels()
	if allModels == nil {
		return
	}

	// Group models by name
	byName := make(map[string][]*cache.CachedModel)
	for _, model := range allModels {
		byName[model.Name] = append(byName[model.Name], model)
	}

	// Track which prefixed route names are still valid
	activeRoutes := make(map[string]bool)

	changed := false
	for modelName, instances := range byName {
		rName := m.routeName(modelName)
		existing := m.groupStore.Get(rName)

		// Skip user-created groups
		if existing != nil && !IsAutoGroup(existing) {
			continue
		}

		replicas := buildReplicasFromInstances(instances)

		if len(replicas) < 2 {
			if existing != nil {
				m.groupStore.Delete(rName)
				m.publish(route_events.EventAutoRouteRevoked, rName)
				changed = true
				slog.Info("[AutoRoute] Auto-route removed", "route", rName)
			}
			continue
		}

		activeRoutes[rName] = true

		// Skip if route already exists with same replica count
		if existing != nil && len(existing.Replicas) == len(replicas) && replicasMatch(existing.Replicas, replicas) {
			continue
		}

		strategy := group.StrategyLeastLoad
		if existing != nil {
			strategy = existing.Strategy
		}

		group := group.ModelGroup{
			Description: fmt.Sprintf("Route for %s across %d replicas", modelName, len(replicas)),
			Strategy:    strategy,
			Replicas:    replicas,
			AutoManaged: true,
		}

		if err := m.groupStore.Set(rName, group); err != nil {
			slog.Error("[AutoRoute] Failed to set auto-route", "route", rName, "error", err)
			continue
		}
		// Emit auto_route_generated only on first creation. Reconciles
		// that rewrite the same auto-route (e.g. replica count changed)
		// surface as route_updated via the group store; agents asking
		// "was this an autogen moment?" should pivot on the create
		// transition, not on every refresh.
		if existing == nil {
			m.publish(route_events.EventAutoRouteGenerated, rName)
		}
		changed = true
		slog.Info("[AutoRoute] Auto-route synced", "route", rName, "replicas", len(replicas))
	}

	// Clean up auto-routes for models that no longer exist in cache.
	//
	// Reaping is restricted to names this manager could have generated,
	// because activeRoutes only ever holds routePrefix+modelName: any
	// other name is unconditionally absent from it and would read as
	// "model gone". This is a second line of defence — the first is that
	// only a claimed auto-route can be handed back via DELETE /:name/owner,
	// so an unowned group never acquires the AutoManaged flag to begin
	// with. The prefix test cannot stand alone: an operator may set
	// route_prefix to "", which makes HasPrefix vacuously true.
	for name, group := range m.groupStore.List() {
		if !IsAutoGroup(&group) || !strings.HasPrefix(name, m.routePrefix) {
			continue
		}
		if !activeRoutes[name] {
			m.groupStore.Delete(name)
			m.publish(route_events.EventAutoRouteRevoked, name)
			changed = true
			slog.Info("[AutoRoute] Auto-route removed (model gone)", "route", name)
		}
	}

	if changed {
		if err := m.groupStore.Save(); err != nil {
			slog.Error("[AutoRoute] Failed to persist auto-routes", "error", err)
		}
	}
}

// replicasMatch returns true if two replica slices have the same names.
func replicasMatch(a, b []group.Replica) bool {
	if len(a) != len(b) {
		return false
	}
	names := make(map[string]bool, len(a))
	for _, r := range a {
		names[r.Name] = true
	}
	for _, r := range b {
		if !names[r.Name] {
			return false
		}
	}
	return true
}

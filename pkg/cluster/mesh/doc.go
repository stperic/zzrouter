// Package mesh implements the peer-to-peer cluster mesh.
//
// Primary type: [Cluster] — the cluster client facade that holds the
// dispatcher, health monitor, circuit breaker, resource tracker, and
// endpoint registry for the running node's view of its peers.
//
// # Scope
//
// Peer discovery state, request dispatch, health probing, and circuit
// breaking. Local node lifecycle and role claim live in sibling
// packages ([pkg/cluster/node], [pkg/cluster/role]); identity and mTLS
// material live in [pkg/cluster/id].
//
// # Lifecycle
//
// [Cluster] itself is constructed via [NewCluster] and torn down via
// `(*Cluster).Stop`. Background loops (health probing, reconnection)
// live on sub-components — [HealthMonitor] exposes its own
// `Start(ctx)`/`Stop()`; [Dispatcher] spawns its worker pool on
// construction. `Cluster.Stop` fans out to each sub-component's
// shutdown; it is idempotent.
//
// This shape does not match the single-entry [prov_apps.ProviderAppManager]
// pattern — mesh evolved as a composition of independently-testable
// sub-components. Either convention is acceptable; what matters is
// that every owned goroutine is documented on its owning sub-component
// and terminates by `Cluster.Stop`.
//
// # Lock Ordering
//
// **Independent sub-components — no nesting observed.** The package
// holds five struct-field mutexes: [Dispatcher].mu, [EndpointRegistry].mu,
// [HealthMonitor].mu, [ResourceTracker].mu, and [DownloadTracker].mu.
// Each is local to its sub-component; no code path in this package
// holds one mutex while acquiring another. Full acquisition-site trace
// lives at docs/audits/cluster_lock_graph.md.
//
// Lock discipline (do not regress):
//
//   - Each sub-component's mutex is local to that sub-component.
//   - Never reach through `Cluster` while holding a sub-component lock.
//   - Dispatcher critical sections must release the lock before calling
//     `handler.Dispatch`; HealthMonitor critical sections must release
//     before calling into [EndpointRegistry] (dispatcher.go and
//     health.go both follow this today).
//   - If you add a new lock acquisition path that crosses components,
//     document it at the call site and update docs/audits/cluster_lock_graph.md.
//
// # Architecture
//
// See docs/package_architecture.md and docs/audits/cluster.md.
package mesh

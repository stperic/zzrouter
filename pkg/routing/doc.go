// Package routing decides which node + provider should serve a given
// inference request.
//
// Primary types:
//
//   - [Router] — the abstract interface every strategy satisfies
//   - [LocalOnlyRouter]    — used by workers; always executes locally
//   - [ClusterAwareRouter] — used by coordinators; fans requests out
//     to peers via [mesh.ClusterClient]
//   - [ResourceAwareRouter] — picks among local endpoints based on
//     health, latency, and load
//   - [Swappable] — hot-swap wrapper used by the server when roles
//     flip (single → coordinator, coordinator → worker)
//
// # Scope
//
// Target selection only. Byte-level copy + protocol adaptation lives
// in [pkg/dispatch]; health, circuit-breaking, and retry live in
// [pkg/fallback]; mesh dispatch itself lives in [pkg/cluster/mesh].
//
// # Lock Ordering
//
// Two internal mutexes — one on [ResourceAwareRouter] (endpoint
// scoring cache) and one on [Swappable] (atomic pointer swap under
// lock). Independent; no cross-type acquisition.
//
// # Architecture
//
// See docs/package_architecture.md.
package routing

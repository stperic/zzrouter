// Package cluster hosts zzrouter's cluster-coordination primitives.
//
// This package is subpackage-only (no root Go files besides this one).
// Each subpackage owns one concern:
//
//   - [id]   — cluster identity and mTLS CA material (clusterid.Identity,
//     clusterid.CA). Pure leaf; no sibling imports.
//   - [mesh] — peer mesh: cluster client, dispatcher, health monitor,
//     circuit breaker, resource tracker, endpoint registry. Primary
//     type [mesh.Cluster]; lifecycle runs on sub-components
//     (HealthMonitor.Start/Stop, Dispatcher), not on Cluster itself.
//   - [node] — local node runtime and pairing: [node.Node] owns
//     Start(ctx)/Stop(ctx), pairing lifecycle, role claim/renewal.
//   - [role] — role state machine (coordinator/worker) with transition
//     subscribers. Primary type [role.Manager].
//
// # Dependency Direction
//
//	id    (leaf, stdlib only)
//	 ^
//	 |
//	node  ->  mesh (independent subtree)
//	 ^
//	 |
//	role
//
// `id` has no sibling imports. `mesh` does not import `node`, `role`, or
// `id`. `node` imports `id`. `role` imports `node`. There are no cycles.
//
// # Lock Ordering
//
// `mesh` and `node` each hold multiple struct-field mutexes; both
// have been traced and documented in their subpackage doc.go files,
// backed by the full acquisition-site analysis at
// docs/audits/cluster_lock_graph.md. Summary: `mesh` has five
// independent sub-component mutexes (no nesting); `node` has three
// independent families (Node lifecycle, pairing rate limiter, deny
// list + pairing store) with one confirmed benign nesting inside the
// rate-limiter family.
//
// Do not invent cross-subpackage ordering — no code path today
// acquires a mesh lock while holding a node lock, or vice versa.
//
// # Architecture
//
// See docs/package_architecture.md for the ruleset this tree is audited
// against (docs/audits/cluster.md).
package cluster

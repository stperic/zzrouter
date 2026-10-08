// Package role provides a single source of truth for the cluster
// mode of a zzrouter node. Mode is runtime-dynamic (promote on
// pairing completion, demote on cert expiry / revocation / config
// reload); consumers read role.Manager rather than sampling config
// fields scattered across the codebase.
//
// # Node vs role (read this before using the predicates)
//
// Every zzrouter node is a producer regardless of role: it has
// hardware, runs local deployments / installs / updates / inference,
// and owns local state (registry, cache, job stream). This is the
// base identity and is unconditional — no role predicate gates it.
//
// Role layers *additional cluster-facing responsibilities* on top:
//
//   - RoleWorker adds: serves /internal/* over mTLS, signals
//     notify/goodbye, accepts dispatched requests from a coord.
//   - RoleCoordinator is a superset. On top of worker duties it
//     also serves the public /zzrouter/v1/* API, hosts the CA,
//     runs ClusterAwareRouter, and proxies to remote workers.
//
// IsWorker() and IsCoordinator() are strict-equality predicates over
// the Role enum. They answer "does this node carry these specific
// extra cluster duties?", NOT "is this node a producer." Anything
// that asks the latter is unconditional — do not gate it on either
// predicate. For "does this node serve /internal/*", prefer the live
// cluster listener mode (see pkg/cluster/node) over role, since the
// coord-is-a-superset semantics are not encoded in IsWorker().
//
// Start/Stop are part of the subsystem contract (covered by the
// lifecycle pairing guard); they are no-ops today. Background
// workers (e.g. config-reload watchers) may be added later without
// changing the public surface.
//
// Role is derived from two distinct inputs:
//
//   - At boot, RoleFromConfig(cfg.Cluster, paired bool) reads
//     static yaml + on-disk pairing state.
//   - At runtime, RoleFromMode(clusternode.Mode) translates live
//     cluster-listener state changes (pairing completion, revert
//     to unclaimed) into Role transitions mirrored via Manager.Set.
//
// See pkg/cluster/node for the Mode enum this role mirrors, and
// internal/server/cluster_mode_mirror.go for the mirror wiring.
package role

import "fmt"

// Role is the cluster mode of a node. Values are string-backed for
// log/audit clarity.
type Role string

const (
	// RoleDisabled — cluster subsystem is off. No cluster listener,
	// no mTLS material touched. Standalone HTTP only.
	RoleDisabled Role = "disabled"

	// RoleCoordinator — this node is the cluster coordinator.
	// Serves /zzrouter/v1/* management API and hosts the CA.
	RoleCoordinator Role = "coordinator"

	// RoleUnclaimed — cluster enabled but not yet paired with a
	// coordinator. Dormant on the cluster network; operator runs
	// `zzrouter cluster pair` to enter pairing mode.
	RoleUnclaimed Role = "unclaimed"

	// RoleWorker — paired worker. Serves /internal/* over mTLS.
	RoleWorker Role = "worker"
)

// String satisfies fmt.Stringer.
func (r Role) String() string { return string(r) }

// IsCoordinator reports whether this role serves the coordinator API
// (public /zzrouter/v1/*, compat routes, CA). Strict-equality predicate
// over the Role enum. Does NOT ask "is this node a producer" — every
// node is a producer regardless of role; see the package doc.
func (r Role) IsCoordinator() bool { return r == RoleCoordinator }

// IsWorker reports whether this role is the paired-worker role (serves
// /internal/* as a leaf, signals notify/goodbye). Strict-equality
// predicate: IsWorker() is false for RoleCoordinator even though a
// coord ALSO serves /internal/* and produces local jobs (coord is a
// semantic superset). Do not use IsWorker() as a "does this node have
// local work" check — that question is unconditional; see the package
// doc for the node-vs-role distinction.
func (r Role) IsWorker() bool { return r == RoleWorker }

// IsValid reports whether r is one of the defined Role constants.
func (r Role) IsValid() bool {
	switch r {
	case RoleDisabled, RoleCoordinator, RoleUnclaimed, RoleWorker:
		return true
	default:
		return false
	}
}

// Validate returns nil if r is a defined Role, else a descriptive error.
func (r Role) Validate() error {
	if !r.IsValid() {
		return fmt.Errorf("invalid role %q", r)
	}
	return nil
}

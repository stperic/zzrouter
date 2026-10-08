package role

import (
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
)

// RoleFromMode translates a clusternode.Node runtime mode into the
// matching role.Role. The four names are one-to-one by design so
// Server can mirror Node mode transitions into role.Manager via a
// single lookup (see internal/server/cluster_mode_mirror.go).
//
// ok=false for an unknown Mode value. Callers should log and skip
// rather than silently downgrading to RoleDisabled — role.Manager.Set
// rejects unknown roles anyway; this makes the failure path explicit.
//
// This is the runtime counterpart to RoleFromConfig (bootstrap.go),
// which derives the role from static ClusterConfig at Server
// construction. Both funnel into the same Role enum; the split
// exists because the runtime transitions (pairing, decommission,
// cert expiry) flip clusternode.Mode first and then mirror into
// role.Manager, while boot skips clusternode entirely and reads
// config + on-disk pairing state.
func RoleFromMode(m clusternode.Mode) (Role, bool) {
	switch m {
	case clusternode.Disabled:
		return RoleDisabled, true
	case clusternode.Coordinator:
		return RoleCoordinator, true
	case clusternode.Unclaimed:
		return RoleUnclaimed, true
	case clusternode.Worker:
		return RoleWorker, true
	default:
		return "", false
	}
}

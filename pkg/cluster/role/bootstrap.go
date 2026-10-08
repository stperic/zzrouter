package role

import (
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// RoleFromConfig derives the initial cluster role from static node
// configuration at Server construction time and on config reload.
// Post-construction runtime transitions (pairing completion, renewal
// failure, decommission, cert full expiry) go through Manager.Set —
// this function only computes the initial or reloaded steady-state.
//
// The caller supplies `paired`: whether the on-disk pairing state
// represents a valid prior pairing that can resume as Worker. This
// keeps the role package free of filesystem probes; clusternode.IsPaired
// is the canonical probe.
//
// Mapping:
//
//   - cluster.mode=coordinator → RoleCoordinator
//   - cluster.mode=worker      → RoleWorker if paired, else RoleUnclaimed
//     (worker-track node is dormant until it pairs).
//   - cluster.mode=disabled / standalone / empty → RoleCoordinator
//     (standalone runs the same coordinator-side workloads as a real
//     coordinator; differs only in not managing workers or serving
//     /internal/*). RoleDisabled as a real state exists but flipping
//     standalone to it would skip core inference subsystems that gate
//     on IsCoordinator().
func RoleFromConfig(cc pkgConfig.ClusterConfig, paired bool) Role {
	switch {
	case cc.IsWorker():
		if paired {
			return RoleWorker
		}
		return RoleUnclaimed
	case cc.IsCoordinator():
		return RoleCoordinator
	default:
		return RoleCoordinator
	}
}

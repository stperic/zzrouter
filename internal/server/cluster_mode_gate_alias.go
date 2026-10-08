package server

// Re-export of the role-gate middleware from pkg/cluster/role.
// The canonical definition lives in pkg/cluster/role/gate.go;
// this alias preserves the ClusterModeGate name used in tests.

import (
	"github.com/stperic/zzrouter/pkg/cluster/role"
)

var ClusterModeGate = role.RoleGate

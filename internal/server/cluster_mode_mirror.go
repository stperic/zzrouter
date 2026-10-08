package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/cluster/role"
)

// onClusterModeChange reflects a clusternode.Node runtime mode
// transition into role.Manager so downstream consumers (architecture
// guards, gating middleware, subscribers) observe a single source of
// truth. Called from the Node's transition goroutine AFTER it has
// released its internal mutex — safe to take role.Manager's own lock
// via Set.
//
// Node mode flips happen in two places:
//
//   - Unclaimed → Worker: successful pairing (completePairing).
//   - Worker   → Unclaimed: revert triggered by /cluster/leave,
//     renewal deny list, or cert full expiry (revertToUnclaimed).
//
// The Mode→Role translation itself lives in role.RoleFromMode —
// this function is purely the server-side mirror wiring (context,
// Manager.Set, error logging).
func (s *Server) onClusterModeChange(to clusternode.Mode, reason string) {
	target, ok := role.RoleFromMode(to)
	if !ok {
		slog.Error("cluster mode change: unknown target mode, skipping role update",
			"target_mode", to.String(), "reason", reason)
		return
	}
	// Bounded ctx — role.Manager.Set shouldn't block beyond the
	// fan-out of a few subscribers, but don't let a pathological
	// subscriber wedge the completePairing goroutine indefinitely.
	ctx, cancel := context.WithTimeout(s.shutdownCtx, 5*time.Second)
	defer cancel()
	if err := s.role.Set(ctx, target, reason); err != nil {
		slog.Error("cluster mode change: role.Manager.Set failed",
			"target_role", target, "reason", reason, "error", err)
	}

	// Unclaimed→Worker via runtime pairing: the admin httpServer was
	// bound at factory time to cfg.Node.Bind (LAN-reachable) and stays
	// that way until process restart. Worker mode expects 127.0.0.1-only
	// admin (af3e7214). Warn loudly so operators don't leave the box in
	// a half-narrowed state for days.
	if to == clusternode.Worker && s.httpServer != nil {
		addr := s.httpServer.Addr
		if !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "[::1]:") {
			slog.Warn("admin port still bound to LAN after pairing: restart to narrow to loopback",
				"admin_addr", addr,
				"hint", "systemctl restart zzrouter-node (or your service manager equivalent)")
		}
	}
}

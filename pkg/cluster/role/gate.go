package role

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// RoleGate returns middleware that 503s when the node's current cluster
// role is not one of the allowed roles. Register it at route-registration
// time — one gate per route group, not per request decision.
//
// Role is read fresh on every request via rm.Current(), so a live
// promote/demote flips the gate behavior without an engine rebuild.
// This is the runtime-route counterpart to the compile-time routing
// swap handled by cluster.routerSwap (server_cluster.go).
//
// The gate uses the request's attached responder so the 503 body
// matches the dialect of the route group (Problem Details for
// /zzrouter/v1/*, OpenAI error for /v1/*). Install the responder
// middleware before this gate; otherwise the built-in Problem
// Details fallback is used.
func RoleGate(rm *Manager, allowed ...Role) gin.HandlerFunc {
	if rm == nil {
		panic("RoleGate: nil *Manager")
	}
	if len(allowed) == 0 {
		panic("RoleGate: no allowed roles specified")
	}
	allowedSet := make(map[Role]struct{}, len(allowed))
	for _, r := range allowed {
		allowedSet[r] = struct{}{}
	}

	return func(c *gin.Context) {
		current := rm.Current()
		if _, ok := allowedSet[current]; ok {
			c.Next()
			return
		}
		r := httperr.FromContext(c)
		if r == nil {
			// Fallback: emit a plain 503 with no dialect concern.
			c.AbortWithStatus(503)
			return
		}
		r.Unavailable(c, fmt.Sprintf("route unavailable in cluster role %q", current))
	}
}

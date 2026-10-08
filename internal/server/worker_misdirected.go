package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
)

// workerManagementMisdirected emits a helpful 421 Problem Details when
// a public /zzrouter/v1/* management request hits a worker node, which
// by design mounts only /internal/*, /health/*, and /cluster/{join,leave}.
// Returns true when it handled the request (caller should bail), false
// to let the default NoRoute 404 fire.
//
// HTTP 421 "Misdirected Request" is the right status here: the URL
// exists on the coordinator, the request was sent to the wrong host,
// and well-behaved clients treat 421 as "retry elsewhere, not here"
// rather than "doesn't exist." The detail body names the coordinator
// so operators can redirect without grepping config.
func workerManagementMisdirected(c *gin.Context, s *Server) bool {
	// Trigger on Worker AND Unclaimed: a freshly-booted worker-mode
	// node that hasn't completed pairing is in Unclaimed and equally
	// doesn't serve management routes. Coordinators (including
	// standalone/disabled modes, which RoleFromConfig maps to
	// Coordinator) keep the default 404 path.
	if s.role.Current().IsCoordinator() {
		return false
	}
	path := c.Request.URL.Path
	if !strings.HasPrefix(path, "/zzrouter/v1/") {
		return false
	}
	// The worker DOES legitimately serve some paths under /zzrouter/v1/
	// (/internal/* via the cluster listener, /cluster/{join,leave} on
	// the public engine). Let those fall through to the regular 404
	// handler if they happen to miss — this helper only catches the
	// "public management API on a worker" case.
	if strings.HasPrefix(path, "/zzrouter/v1/internal/") ||
		strings.HasPrefix(path, "/zzrouter/v1/cluster/") {
		return false
	}

	coordHint := workerCoordHint(s)
	detail := "this node is a worker; /zzrouter/v1/* management routes are coordinator-only"
	if coordHint != "" {
		detail += ": try " + coordHint
	}
	problem := utils.NewProblemDetails(http.StatusMisdirectedRequest, "Misdirected Request", detail, path)
	if reqID, exists := c.Get("request_id"); exists {
		if id, ok := reqID.(string); ok {
			problem.RequestID = id
		}
	}
	c.Header("Content-Type", "application/problem+json")
	c.JSON(http.StatusMisdirectedRequest, problem)
	return true
}

// workerCoordHint returns a human-readable identity for the paired
// coordinator — its host, without the mTLS cluster-port path. The
// coord's public admin port isn't advertised to workers, so we can't
// synthesize a full URL; giving operators the host is enough to let
// them reach the right node. Returns "" when no coord is paired yet.
func workerCoordHint(s *Server) string {
	if s.cluster.listener == nil {
		return ""
	}
	coord := s.cluster.listener.CoordinatorURL()
	if coord == "" {
		return ""
	}
	u, err := url.Parse(coord)
	if err != nil || u.Hostname() == "" {
		return coord
	}
	return "coordinator host " + u.Hostname()
}

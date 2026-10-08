package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/constants"
)

// decommissionWorkerRequest is the JSON body for
// POST /zzrouter/v1/cluster/decommission-worker. Complement to the
// pairing-accept flow: tells a paired worker to revert back to
// Unclaimed and drops it from the coordinator's endpoint registry +
// config.
type decommissionWorkerRequest struct {
	// WorkerAddr is the worker's cluster-port address ("host:port").
	WorkerAddr string `json:"worker_addr" binding:"required"`

	// PublicURL is the worker's public-API URL as known to this
	// coordinator's endpoint registry. Used for the registry drop.
	// Format: "http://host:port" or "https://host:port". If empty,
	// the handler derives "http://host:<cluster-port-minus-1>" — good
	// enough for the homogeneous-port convention; operators running
	// heterogeneous ports should pass it explicitly.
	PublicURL string `json:"public_url,omitempty"`

	// Reason is recorded on the audit log only; it does not participate
	// in the self-heal protocol.
	Reason string `json:"reason,omitempty"`
}

// handleClusterDecommissionWorker drives the coordinator-side decommission.
// On success, the worker receives /cluster/leave over mTLS, ACKs, and
// self-heals back to Unclaimed; this coordinator drops the worker from
// its endpoint registry and config so broadcast/dispatch stop routing
// there. The identity keypair on the worker is preserved, so the
// operator can re-pair the same node later.
func (h *ClusterHandlers) handleClusterDecommissionWorker(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "cluster listener not initialized")
		return
	}

	var req decommissionWorkerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), constants.ClusterActionTimeout)
	defer cancel()

	if err := h.listener.DecommissionWorker(ctx, req.WorkerAddr); err != nil {
		switch {
		case errors.Is(err, clusternode.ErrNotCoordinator):
			BadRequest(c, "coordinator mode required")
		case errors.Is(err, clusternode.ErrInvalidConfig):
			BadRequest(c, err.Error())
		default:
			// Network / TLS / worker-side rejection
			BadGateway(c, err.Error())
		}
		return
	}

	// Drop from coordinator state. Failures are non-fatal — the worker
	// has already ACKed leave, so operator intent is honored on the
	// worker side. The registry/config drift is reported so the
	// operator can reconcile.
	var regDropped, cfgDropped bool
	if h.coordinator != nil && req.PublicURL != "" {
		if unregErr := h.coordinator.UnregisterEndpoint(req.PublicURL); unregErr == nil {
			regDropped = true
		}
	}
	if found, cfgErr := h.nodeConfigStore.RemoveClusterEndpoint(req.WorkerAddr); cfgErr == nil && found {
		cfgDropped = true
	}
	if h.invalidateCache != nil {
		h.invalidateCache()
	}

	c.JSON(http.StatusOK, gin.H{
		"status":        "decommissioned",
		"worker_addr":   req.WorkerAddr,
		"endpoint_drop": regDropped,
		"config_drop":   cfgDropped,
	})
}

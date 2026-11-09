package server

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/constants"
)

// pairingAcceptRequest identifies the explicitly armed, pinned worker endpoint.
type pairingAcceptRequest struct {
	Code              string `json:"code" binding:"required"`
	WorkerURL         string `json:"worker_url" binding:"required"`
	WorkerFingerprint string `json:"worker_fingerprint" binding:"required"`
}

// pairingAcceptResponse confirms enrollment without exposing certificate material.
type pairingAcceptResponse struct {
	NodeName    string `json:"node_name"`
	Fingerprint string `json:"fingerprint"`
	ShortForm   string `json:"short_form"`
}

// handleClusterPairingAccept enrolls a worker through coordinator-initiated requests.
func (h *ClusterHandlers) handleClusterPairingAccept(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "cluster listener not initialized")
		return
	}

	var req pairingAcceptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}
	if !clusternode.IsValidPairingCode(req.Code) {
		BadRequest(c, "malformed pairing code")
		return
	}

	coordURL, err := clusternode.CoordinatorSelfCoordURL(h.listener, h.node.Bind(), h.node.Name(), h.advertiseURL)
	if err != nil {
		ServiceUnavailable(c, "cluster listener not ready: "+err.Error())
		return
	}

	acceptance, err := h.listener.Enroll(c.Request.Context(), req.WorkerURL, req.WorkerFingerprint, req.Code, coordURL)
	if err != nil {
		respondPairingAcceptError(c, err)
		return
	}

	// Admit the paired worker into the cluster membership:
	// config-store add → mesh register → cache invalidate, via the
	// shared admitClusterMember helper. Non-fatal failures are logged;
	// pairing trust is already established, so the 200 still reflects
	// the pair.
	if acceptance.RemoteAddr == "" {
		slog.Warn("pairing accept: no worker RemoteAddr captured; skipping mesh admission",
			"node", acceptance.NodeName)
	} else {
		hostAddr := net.JoinHostPort(acceptance.RemoteAddr, strconv.Itoa(constants.DefaultZZROUTERPort))
		if err := h.admitClusterMember(hostAddr); err != nil {
			slog.Warn("pairing accept: cluster admission failed",
				"node", acceptance.NodeName, "addr", hostAddr, "err", err)
		} else {
			slog.Info("pairing accept: worker admitted to cluster",
				"node", acceptance.NodeName, "addr", hostAddr)
		}
	}

	c.JSON(http.StatusOK, pairingAcceptResponse{
		NodeName:    acceptance.NodeName,
		Fingerprint: acceptance.Fingerprint,
		ShortForm:   acceptance.ShortForm,
	})
}

// respondPairingAcceptError maps enrollment validation failures to the admin contract.
func respondPairingAcceptError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, clusternode.ErrNotCoordinator):
		BadRequest(c, "coordinator mode required")

	default:
		BadRequest(c, err.Error())
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// pairingAcceptRequest identifies the explicitly armed, pinned worker endpoint.
type pairingAcceptRequest struct {
	Code              string                       `json:"code"`
	WorkerURL         string                       `json:"worker_url"`
	WorkerFingerprint string                       `json:"worker_fingerprint"`
	Transfer          *clusternode.PairingTransfer `json:"transfer,omitempty"`
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

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, clusternode.MaxPairingTransferSize+256))
	if err != nil {
		BadRequest(c, "Could not read pairing request")
		return
	}
	var req pairingAcceptRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		BadRequest(c, "Invalid pairing request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		BadRequest(c, "Expected one pairing request")
		return
	}
	if req.Transfer != nil {
		if req.Code != "" || req.WorkerURL != "" || req.WorkerFingerprint != "" {
			BadRequest(c, "Use a pairing document or individual pairing inputs, not both")
			return
		}
		if err := req.Transfer.Validate(utils.NowUTC()); err != nil {
			BadRequest(c, err.Error())
			return
		}
		req.Code, req.WorkerURL, req.WorkerFingerprint = req.Transfer.Code, req.Transfer.WorkerURL, req.Transfer.WorkerFingerprint
	}
	if !clusternode.IsValidPairingCode(req.Code) {
		BadRequest(c, "malformed pairing code")
		return
	}

	coordURL, err := clusternode.CoordinatorSelfCoordURL(h.listener, h.advertiseURL)
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
	case errors.Is(err, clusternode.ErrEnrollmentUncertain):
		ServiceUnavailable(c, "Pairing may have completed. Inspect compute nodes before trying again: "+err.Error())

	default:
		BadRequest(c, err.Error())
	}
}

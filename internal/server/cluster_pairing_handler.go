package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/constants"
)

// pairingAcceptRequest is the admin-POST body for
// /zzrouter/v1/cluster/pairing/accept. Just the code — the pending
// entry on the coord side carries everything else (node_name, CSR,
// fingerprint, SANs).
type pairingAcceptRequest struct {
	Code string `json:"code" binding:"required"`
}

// pairingAcceptResponse is the admin-facing confirmation payload. No
// cert material — that goes back to the worker via its long-poll, not
// to the admin who triggered the accept.
type pairingAcceptResponse struct {
	NodeName    string `json:"node_name"`
	Fingerprint string `json:"fingerprint"`
	ShortForm   string `json:"short_form"`
}

// pairingPendingEntry is the per-row shape of the
// /zzrouter/v1/cluster/pairing/pending response. Code is NOT
// exposed — operators read it off the worker.
type pairingPendingEntry struct {
	NodeName    string  `json:"node_name"`
	Fingerprint string  `json:"fingerprint"`
	AgeSeconds  float64 `json:"age_seconds"`
}

// handleClusterPairingAccept drives the admin side of worker-initiated
// pairing. Called after the operator reads the code off the worker
// (or from the installer log).
//
// Flow:
//  1. Bind body, validate code.
//  2. Delegate to Node.AcceptPairing — which inspects the pending
//     entry, signs the stashed CSR, and atomically burns the code +
//     wakes the parked worker long-poll.
//  3. Return {node_name, fingerprint, short_form} on 200.
//
// Error mapping:
//   - 400 bad body / code format / CSR validation / CA signing
//   - 404 ErrPairingCodeUnknown
//   - 410 ErrPairingCodeExpired
//   - 409 ErrPairingAlreadyAccepted
//   - 400 ErrNotCoordinator
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

	acceptance, err := h.listener.AcceptPairing(req.Code, coordURL)
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
		hostAddr := fmt.Sprintf("%s:%d", acceptance.RemoteAddr, constants.DefaultZZROUTERPort)
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

// handleClusterPairingList serves GET /zzrouter/v1/cluster/pairing/pending.
// Lists currently-pending pairing requests so an operator who forgot
// the code can at least see what's queued. Code is redacted at the
// store level.
func (h *ClusterHandlers) handleClusterPairingList(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "cluster listener not initialized")
		return
	}
	if h.listener.Mode() != clusternode.Coordinator {
		BadRequest(c, "pairing list requires coordinator mode (current: "+h.listener.Mode().String()+")")
		return
	}

	summaries := h.listener.PendingPairings()
	entries := make([]pairingPendingEntry, 0, len(summaries))
	for _, s := range summaries {
		entries = append(entries, pairingPendingEntry{
			NodeName:    s.NodeName,
			Fingerprint: s.Fingerprint,
			AgeSeconds:  s.Age.Seconds(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"pending": entries})
}

// respondPairingAcceptError maps clusternode's pairing sentinels to
// RFC 9457 Problem Details responses. CSR-validation / CA-signing
// errors bubble out as the underlying clusterid sentinels; we lump
// those into 400 for the admin surface so the operator sees a single
// actionable failure mode. The admin API trusts its caller with
// detail — the underlying message is surfaced verbatim.
func respondPairingAcceptError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, clusternode.ErrNotCoordinator):
		BadRequest(c, "coordinator mode required")
	case errors.Is(err, clusternode.ErrPairingCodeUnknown):
		NotFound(c, "no pending pairing request with that code")
	case errors.Is(err, clusternode.ErrPairingCodeExpired):
		RespondWithProblem(c, http.StatusGone, "Gone", "pairing code expired")
	case errors.Is(err, clusternode.ErrPairingAlreadyAccepted):
		Conflict(c, "pairing code already consumed")
	case errors.Is(err, clusternode.ErrPairingStoreStopped):
		ServiceUnavailable(c, "pairing store stopped")
	default:
		BadRequest(c, err.Error())
	}
}

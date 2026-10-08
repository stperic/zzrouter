package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
)

// revokeWorkerRequest is the JSON body for POST /zzrouter/v1/cluster/revoke-worker.
// Fingerprint is the SPKI sha256 in "sha256:<hex>" form — read from
// the pairing-pending listing or from local records. Serial is
// optional forensic metadata (cert serials roll every renewal and
// are not the match key).
type revokeWorkerRequest struct {
	Fingerprint string `json:"fingerprint" binding:"required"`
	Serial      string `json:"serial,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// handleClusterRevokeWorker appends the given fingerprint to the
// coordinator's persistent deny list. Any subsequent mTLS handshake
// presenting a cert with that SPKI is rejected by the listener and by
// outbound dial gates, and the worker-side renewal loop observes the
// structured 403 and self-heals back to Unclaimed.
//
// Idempotent: re-revoking an already-denied fingerprint updates the
// reason + revoked_at timestamp without producing an error.
func (h *ClusterHandlers) handleClusterRevokeWorker(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "cluster listener not initialized")
		return
	}

	var req revokeWorkerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		BadRequest(c, err.Error())
		return
	}

	// Normalize + validate before touching the deny list. A
	// display-only form (with the "…" ellipsis) or other non-hex
	// input would otherwise write garbage that never matches any
	// real SPKI, poisoning the on-disk deny-list file and confusing
	// audits. Accepts both bare hex and "sha256:<hex>"; persists the
	// canonical lowercase hex suffix.
	fp, err := clusterid.NormalizeFingerprint(req.Fingerprint)
	if err != nil {
		BadRequest(c, "fingerprint must be 64 hex chars (optional sha256: prefix)")
		return
	}

	if err := h.listener.Revoke(fp, req.Serial, req.Reason); err != nil {
		switch {
		case errors.Is(err, clusternode.ErrNotCoordinator):
			BadRequest(c, "coordinator mode required")
		case errors.Is(err, clusternode.ErrInvalidConfig):
			BadRequest(c, err.Error())
		default:
			InternalNodeError(c, err.Error())
		}
		return
	}

	slog.Info("cluster: worker revoked",
		"fingerprint", fp,
		"serial", req.Serial,
		"reason", req.Reason)

	c.JSON(http.StatusOK, gin.H{
		"status":      "revoked",
		"fingerprint": fp,
	})
}

package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
)

// clusterPairRequest is the admin-POST body for
// /zzrouter/v1/cluster/pair. One of Regenerate / Cancel may be set;
// both false = idempotent "enter pairing mode, reuse active window
// if one exists".
//
// CoordinatorURL + CoordinatorCAFingerprint are required on the first
// (window-opening) call and ignored on Regenerate / Cancel and on
// reuse calls against an already-active window. They live on the
// request, not in user config — pairing-time input only.
type clusterPairRequest struct {
	Regenerate               bool   `json:"regenerate"`
	Cancel                   bool   `json:"cancel"`
	CoordinatorURL           string `json:"coordinator_url,omitempty"`
	CoordinatorCAFingerprint string `json:"coordinator_ca_fingerprint,omitempty"`
}

// clusterPairResponse carries the window state back to the CLI.
// Status values:
//
//   - "new"       — BeginPairing opened a fresh window.
//   - "existing"  — BeginPairing found an active window and reused it.
//   - "cancelled" — CancelPairing cleared the window.
//   - "no_active_window" — Cancel invoked with no window to cancel.
//
// Error, when non-empty, carries the most recent terminal failure
// from the background pairing loop (e.g. the coord returned 403
// because cluster.require_secure_pairing is set). Delivered exactly
// once — the Node consumes-and-clears the error on this call — so
// the TUI surfaces an actionable message instead of a generic "no
// active window".
type clusterPairResponse struct {
	Status   string    `json:"status"`
	Code     string    `json:"code,omitempty"`
	Deadline time.Time `json:"deadline,omitempty"`
	Error    string    `json:"error,omitempty"`
	// Mode carries the node's runtime cluster mode ("unclaimed" |
	// "worker" | "coordinator" | "disabled") so the TUI can detect
	// successful completion: post-pair, the window closes and mode
	// flips to worker, which is the client's "paired" signal.
	Mode string `json:"mode,omitempty"`
}

// handleClusterPair drives the worker-side pairing lifecycle. Mounted
// on the worker's admin API — a Coordinator serving this handler
// returns 400 (not a valid invocation on that mode).
func (h *ClusterHandlers) handleClusterPair(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "cluster listener not initialized")
		return
	}

	var req clusterPairRequest
	// Empty body is valid (defaults to "re-enter pairing mode,
	// idempotent"). ShouldBindJSON returns io.EOF on empty bodies;
	// tolerate that.
	_ = c.ShouldBindJSON(&req)

	if req.Cancel {
		if err := h.listener.CancelPairing(); err != nil {
			if errors.Is(err, clusternode.ErrPairingNoActiveWindow) {
				c.JSON(http.StatusOK, clusterPairResponse{Status: "no_active_window"})
				return
			}
			respondPairBeginError(c, err)
			return
		}
		c.JSON(http.StatusOK, clusterPairResponse{Status: "cancelled"})
		return
	}

	// Short-circuit the reuse path: if a window is already active and
	// the caller isn't regenerating, return the existing code without
	// touching BeginPairing. This lets the TUI wizard poll the handler
	// at 1 Hz for status updates without re-sending the pairing inputs
	// every tick, and lets an operator re-check their in-flight window
	// without digging up the URL + fingerprint again.
	if info, hadWindow := h.listener.PairingWindowInfoSnapshot(); hadWindow && !req.Regenerate {
		c.JSON(http.StatusOK, clusterPairResponse{
			Status:   "existing",
			Code:     info.Code,
			Deadline: info.Deadline,
		})
		return
	}

	// Empty-body poll (no coord URL, no regenerate, no cancel): the
	// TUI is polling for status. Return no_active_window with the
	// current mode + any trailing background error, so the client can
	// distinguish success (mode=worker, window consumed by the coord)
	// from expiry/cancel (mode=unclaimed) without calling BeginPairing
	// again (which would 400 on a just-paired Worker).
	if req.CoordinatorURL == "" && req.CoordinatorCAFingerprint == "" && !req.Regenerate {
		resp := clusterPairResponse{
			Status: "no_active_window",
			Mode:   h.listener.Mode().String(),
		}
		if lastErr := h.listener.ConsumeLastPairingError(); lastErr != nil {
			resp.Error = lastErr.Error()
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	// Opening (or regenerating) a window. Pairing inputs come from the
	// request body — the CLI supplies them via flags or after mDNS
	// discovery. Config is NOT a source: coordinator_url /
	// ca_fingerprint are not persisted in node.yaml. Validation
	// happens inside BeginPairing; empty values surface as
	// ErrPairingConfigMissing and land as 400 below.
	opts := clusternode.BeginPairingOptions{
		CoordinatorURL: req.CoordinatorURL,
		CAFingerprint:  req.CoordinatorCAFingerprint,
		Regenerate:     req.Regenerate,
	}
	info, err := h.listener.BeginPairing(opts)
	if err != nil {
		respondPairBeginError(c, err)
		return
	}
	c.JSON(http.StatusOK, clusterPairResponse{
		Status:   "new",
		Code:     info.Code,
		Deadline: info.Deadline,
	})
}

// handleClusterReset drives a worker-initiated reset of the local
// pairing trust. Reverts a Worker node back to Unclaimed, wiping
// its stored CA + coordinator URL so the subsequent `cluster pair`
// call can bind to a different coordinator (or the same one with a
// regenerated CA). Admin-keyed. Synchronous — returns 200 only
// after mode has flipped to Unclaimed so the caller can immediately
// proceed with pairing.
//
// Error mapping:
//   - 400 if node isn't in Worker mode (nothing to reset) — uses
//     the clusternode.ErrResetNotWorker sentinel.
//   - 503 when the cluster listener isn't initialized.
//   - 500 for any other error bubbled from Node.ResetPairing.
func (h *ClusterHandlers) handleClusterReset(c *gin.Context) {
	if h.listener == nil {
		ServiceUnavailable(c, "cluster listener not initialized")
		return
	}
	if err := h.listener.ResetPairing(); err != nil {
		switch {
		case errors.Is(err, clusternode.ErrResetNotWorker):
			BadRequest(c, "node is not in worker mode: nothing to reset")
		default:
			InternalNodeError(c, err.Error())
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "reset"})
}

// respondPairBeginError maps the clusternode pairing sentinels to
// RFC 9457 Problem Details responses. Unclassified errors bubble as
// 500 with the underlying message — this endpoint is admin-only,
// detail is OK.
func respondPairBeginError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, clusternode.ErrPairingNotUnclaimed):
		// Surface the wrapped "current mode X" suffix so the CLI/TUI
		// can tell Worker-already-paired (fix: --reset) apart from
		// Coordinator/Disabled (fix: wrong node).
		BadRequest(c, err.Error())
	case errors.Is(err, clusternode.ErrPairingURLMissing):
		BadRequest(c, "coordinator_url must be supplied on the pair request (via --coordinator-url or mDNS discovery)")
	case errors.Is(err, clusternode.ErrPairingCAFingerprintMalformed):
		BadRequest(c, "coordinator_ca_fingerprint is not valid hex; expect sha256:<hex> or bare hex")
	default:
		InternalNodeError(c, err.Error())
	}
}

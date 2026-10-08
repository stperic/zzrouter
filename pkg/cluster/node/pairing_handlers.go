package clusternode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/version"
)

// pairingProtocolWindow returns the coordinator's accepted cluster protocol
// range. A package-level func var rather than a direct call to
// version.LocalProtocolWindow so tests can inject a tight window
// (e.g. {Min:2, Max:2}) and exercise below-floor / above-ceiling handler
// branches without mutating the version package's compile-time consts.
// Production callers never swap it.
var pairingProtocolWindow = version.LocalProtocolWindow

// pairingLongPollHold is the server-side wait the worker's long-poll
// trades against. Design-doc §Endpoints: "30s server-side hold; worker
// reconnects on timeout."
const pairingLongPollHold = 30 * time.Second

// pairingWorkerPollInterval is echoed in waiting_for_operator responses
// so the worker reconnects promptly after a timeout without hammering.
const pairingWorkerPollInterval = 3

// pairingRequestMaxBodyBytes caps the unauthenticated POST body size.
// A real CSR + metadata runs ~4KB; the 128KB ceiling gives ~16x
// headroom while still rejecting obvious memory-exhaustion floods
// from unpaired sources. The TLS layer and rate limiter already bound
// handshake rate, but both fire AFTER the HTTP body has started
// streaming — MaxBytesReader fires during the JSON decode.
const pairingRequestMaxBodyBytes = 128 * 1024

// pairingRequestBody is the JSON shape workers POST on
// /cluster/pairing-request. All fields required except SANs (empty
// slice is fine — CSR validation enforces the SAN safety floor).
//
// ClusterProtocol is the sole wire-version signal: the coordinator gates
// on it via version.CheckClusterProtocol. MinClusterProtocol is echoed
// from the worker for observability only. BuildVersion is a diagnostic
// string surfaced in error bodies and logs; it never gates.
type pairingRequestBody struct {
	NodeName           string   `json:"node_name"`
	Fingerprint        string   `json:"fingerprint"`
	CSRPEM             string   `json:"csr_pem"`
	SANs               []string `json:"sans"`
	Code               string   `json:"code"`
	ClusterProtocol    int      `json:"cluster_protocol"`
	MinClusterProtocol int      `json:"min_cluster_protocol"`
	BuildVersion       string   `json:"build_version,omitempty"`
	// CoordinatorCAFingerprint is the CA SPKI hash the worker pinned
	// on the bootstrap handshake. Empty when the worker ran in TOFU
	// mode. The coordinator compares this to its own fingerprint when
	// cluster.accept_insecure_pairing is false; empty or mismatched
	// values are rejected with 403. Honest-signal only (a malicious
	// worker can always lie) — the check gates operator intent, not
	// security per se.
	CoordinatorCAFingerprint string `json:"coordinator_ca_fingerprint,omitempty"`
}

// pairingWaitingResponse is the 200 body returned when the long-poll
// hold elapses without an Accept arriving. Worker reconnects.
type pairingWaitingResponse struct {
	Status              string `json:"status"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

// pairingApprovedResponse is the 200 body returned when an admin Accept
// fires mid-poll or before. Carries everything the worker needs to flip
// Unclaimed→Worker.
//
// ClusterProtocol + MinClusterProtocol echo the coordinator's advertised
// window so the worker can log/surface them; the worker does not gate the
// coordinator (one-direction trust — worker joins, coordinator decides).
// CoordinatorBuildVersion is diagnostic only.
type pairingApprovedResponse struct {
	Status                  string `json:"status"`
	SignedCertPEM           string `json:"signed_cert_pem"`
	CACertPEM               string `json:"ca_cert_pem"`
	CoordinatorURL          string `json:"coordinator_url"`
	ClusterProtocol         int    `json:"cluster_protocol"`
	MinClusterProtocol      int    `json:"min_cluster_protocol"`
	CoordinatorBuildVersion string `json:"coordinator_build_version,omitempty"`
}

// pairingExpiredResponse is the 200 body returned when the worker
// polls a code whose TTL has lapsed without an Accept. The worker
// surfaces this to the operator as "pairing window expired".
type pairingExpiredResponse struct {
	Status string `json:"status"`
}

// handlePairingRequest serves POST /cluster/pairing-request. Called by
// an unpaired worker; unauthenticated at the application layer (CA-
// pinned TLS on the worker's side is the trust root). Rate-limited
// per-IP and globally by the surrounding middleware.
//
// Idempotent within a pairing window: repeated calls with the same
// (code, fingerprint) return the same pending entry's long-poll.
// Different fingerprint under an existing code → 409.
func (n *Node) handlePairingRequest(c *gin.Context) {
	if n.pairingStore == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable,
			gin.H{"error": "pairing store not initialized"})
		return
	}

	// Cap body size BEFORE JSON decode. Unauthenticated endpoint on
	// the cluster port; treat the reader as hostile.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, pairingRequestMaxBodyBytes)

	var req pairingRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "invalid JSON body"})
		return
	}
	if req.Code == "" || req.Fingerprint == "" || req.CSRPEM == "" || req.NodeName == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "missing required field"})
		return
	}
	// Reject malformed codes before the store mutates anything. A
	// worker POSTing garbage would otherwise poison the pending-
	// request map with entries no admin Accept call can match.
	if !IsValidPairingCode(req.Code) {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "malformed pairing code"})
		return
	}

	// Protocol gate first: a peer outside the cluster_protocol window
	// can't parse this coordinator's responses regardless of pairing
	// policy, so surface that failure ahead of any policy checks.
	window := pairingProtocolWindow()
	if err := window.Check(req.ClusterProtocol); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error":                err.Error(),
			"coordinator_protocol": window.Max,
			"coordinator_min":      window.Min,
			"peer_protocol":        req.ClusterProtocol,
			"coordinator_version":  version.Current.String(),
		})
		return
	}

	// Insecure-pairing gate: when the coordinator is configured to
	// reject TOFU pair requests, the worker must echo the coordinator
	// CA fingerprint it pinned. Empty or mismatched values land as 403.
	// Honest-signal check — a malicious worker can lie — but gates
	// operator intent and catches the common "forgot to pass
	// --ca-fingerprint" footgun on hostile networks.
	if n.cfg.RequireSecurePairing {
		if err := verifyWorkerPinnedOurCA(req.CoordinatorCAFingerprint, n.CAFingerprint()); err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"error": err.Error()})
			return
		}
	}

	// Strip the port from RemoteAddr — we only need the host for
	// later endpoint registration; the worker's public port is fixed.
	workerHost, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
	if workerHost == "" {
		workerHost = c.Request.RemoteAddr
	}
	_, err := n.pairingStore.Record(&PairingRequest{
		Code:        req.Code,
		Fingerprint: req.Fingerprint,
		NodeName:    req.NodeName,
		CSRPEM:      req.CSRPEM,
		SANs:        req.SANs,
		RemoteAddr:  workerHost,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrPairingFingerprintConflict):
			c.AbortWithStatusJSON(http.StatusConflict,
				gin.H{"error": "fingerprint already pending under a different code"})
		case errors.Is(err, ErrPairingStoreStopped):
			c.AbortWithStatusJSON(http.StatusServiceUnavailable,
				gin.H{"error": "pairing store stopped"})
		default:
			c.AbortWithStatusJSON(http.StatusBadRequest,
				gin.H{"error": err.Error()})
		}
		return
	}

	// Long-poll hold — derive a 30s timeout off the request ctx so a
	// client disconnect propagates (ctx-canceled) while a wall-clock
	// expiry converts to the waiting response.
	holdCtx, cancel := context.WithTimeout(c.Request.Context(), pairingLongPollHold)
	defer cancel()

	result, err := n.pairingStore.WaitFor(holdCtx, req.Code)
	if err == nil {
		c.JSON(http.StatusOK, pairingApprovedResponse{
			Status:                  "approved",
			SignedCertPEM:           string(result.SignedCertPEM),
			CACertPEM:               string(result.CACertPEM),
			CoordinatorURL:          result.CoordinatorURL,
			ClusterProtocol:         version.ClusterProtocolVersion,
			MinClusterProtocol:      version.MinClusterProtocolVersion,
			CoordinatorBuildVersion: version.Current.String(),
		})
		return
	}

	switch {
	case errors.Is(err, ErrPairingCodeExpired):
		c.JSON(http.StatusOK, pairingExpiredResponse{Status: "expired"})
	case errors.Is(err, context.DeadlineExceeded):
		// Hold elapsed without Accept — entry preserved by WaitFor,
		// worker will reconnect on the same code.
		c.JSON(http.StatusOK, pairingWaitingResponse{
			Status:              "waiting_for_operator",
			PollIntervalSeconds: pairingWorkerPollInterval,
		})
	case errors.Is(err, context.Canceled):
		// Client disconnected. No response — the client is gone.
		return
	case errors.Is(err, ErrPairingStoreStopped):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable,
			gin.H{"error": "pairing store stopped"})
	case errors.Is(err, ErrPairingCodeUnknown):
		// Record succeeded but the entry vanished before WaitFor
		// parked — possible under a racing Accept+delivery cycle.
		// Treat as expired so the worker restarts the window.
		c.JSON(http.StatusOK, pairingExpiredResponse{Status: "expired"})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError,
			gin.H{"error": err.Error()})
	}
}

// caFingerprintResponse is the JSON shape returned by GET
// /cluster/ca-fingerprint. Value is non-secret — a hash of a public
// key — so the endpoint is unauthenticated and plaintext-HTTP-safe in
// spirit (the coord cluster port runs TLS regardless, but the design
// doc calls this out as not requiring confidentiality).
//
// RequireSecurePairing mirrors the coord-side policy so a pairing
// worker can discover in one round-trip whether TOFU pair requests
// will be rejected; when true, the worker must supply the fingerprint
// (OOB-verified) in its pair request or the coord returns 403.
type caFingerprintResponse struct {
	Fingerprint          string `json:"fingerprint"`
	ShortForm            string `json:"short_form"`
	RequireSecurePairing bool   `json:"require_secure_pairing,omitempty"`
}

// handleCAFingerprint serves GET /cluster/ca-fingerprint. Mounted
// under ClusterModeGate(Coordinator) so a misconfigured worker asking
// its own cluster port gets 501, not a spoofed value.
func (n *Node) handleCAFingerprint(c *gin.Context) {
	if n.ca == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable,
			gin.H{"error": "coordinator has no CA"})
		return
	}
	cert := n.ca.Certificate()
	fp := clusterid.Fingerprint(cert)
	c.JSON(http.StatusOK, caFingerprintResponse{
		Fingerprint:          fp,
		ShortForm:            clusterid.ShortForm(fp),
		RequireSecurePairing: n.cfg.RequireSecurePairing,
	})
}

// ---------------------------------------------------------------------
// Admin-side accept: operator paste of the code.
// ---------------------------------------------------------------------

// PairingAcceptance is what AcceptPairing returns to the admin handler:
// everything the operator-facing 200 response needs, plus forensic
// fields for the audit log. No live store pointer.
type PairingAcceptance struct {
	NodeName    string
	Fingerprint string
	ShortForm   string
	// RemoteAddr is the worker's IP (host only) captured on the
	// incoming /cluster/pairing-request. Empty when unknown. The admin
	// accept handler uses this to register the worker with the mesh
	// endpoint registry so it shows up in `ListNodes` without the
	// operator having to add it by hand.
	RemoteAddr string
}

// AcceptPairing burns the pairing code, validates and signs the stashed
// CSR, stashes the signed artifact on the pending entry, and wakes any
// parked long-poll. Called from the admin HTTP handler on
// POST /zzrouter/v1/cluster/pairing/accept.
//
// coordinatorURL is the URL the worker should persist for future
// renewal dials — computed by the caller (who knows the coord's public
// cluster-port URL; clusternode only knows bind addr + port).
//
// Error mapping:
//
//   - ErrNotCoordinator          — non-coord node.
//   - ErrPairingCodeUnknown      — code not pending (never recorded, consumed, or TTL-evicted).
//   - ErrPairingCodeExpired      — entry existed, TTL lapsed before accept.
//   - ErrPairingAlreadyAccepted  — second accept for the same code.
//   - Anything else              — CSR validation / CA signing failure; map to 400.
func (n *Node) AcceptPairing(code, coordinatorURL string) (*PairingAcceptance, error) {
	if n.Mode() != Coordinator || n.ca == nil || n.pairingStore == nil {
		return nil, ErrNotCoordinator
	}

	// First look up the pending entry so we can sign the stashed CSR
	// BEFORE burning the code on the store. pairingStore.Accept is the
	// atomic commit point; if signing fails we leave the entry
	// pending so a re-attempt (after the operator fixes whatever was
	// wrong) can succeed.
	pending := n.pairingStore.inspect(code)
	if pending == nil {
		return nil, ErrPairingCodeUnknown
	}

	signedCert, err := n.ca.Sign([]byte(pending.CSRPEM), clusterid.RoleWorker)
	if err != nil {
		// CSR validation errors bubble out unwrapped; admin handler
		// maps them to 400 via their own shape (ErrInvalidCSR etc).
		return nil, err
	}

	approved, err := n.pairingStore.Accept(code, signedCert, n.ca.CertPEM(), coordinatorURL)
	if err != nil {
		return nil, err
	}

	return &PairingAcceptance{
		NodeName:    approved.NodeName,
		Fingerprint: approved.Fingerprint,
		ShortForm:   clusterid.ShortForm(approved.Fingerprint),
		RemoteAddr:  pending.RemoteAddr,
	}, nil
}

// PendingPairings returns a snapshot of pending pairing requests for
// the admin `cluster pending` CLI. Code is redacted by the store.
func (n *Node) PendingPairings() []PendingSummary {
	if n.pairingStore == nil {
		return nil
	}
	return n.pairingStore.Pending()
}

// verifyWorkerPinnedOurCA enforces cluster.accept_insecure_pairing=false
// at the coord-side pair handler. Returns an error when the worker's
// echoed fingerprint is empty or doesn't match the coord's own.
// Normalization routes through the shared cluster-id normalizer so
// "sha256:<hex>", bare-hex, and case variations all compare equal.
func verifyWorkerPinnedOurCA(workerEcho, coordFingerprint string) error {
	if coordFingerprint == "" {
		// Should not happen — handler only runs on coordinators, so
		// CAFingerprint() returns non-empty. Guard defensively: if
		// the CA isn't loaded we cannot gate, so fail closed.
		return errors.New("coordinator CA not loaded; pairing unavailable")
	}
	if workerEcho == "" {
		return errors.New("this coordinator rejects TOFU pair requests; re-run with --secure + --ca-fingerprint")
	}
	// Defensive length cap. sha256 fingerprints are <100 chars even
	// with the "sha256:" prefix and any separators; anything longer
	// is either malformed or adversarial and shouldn't burn a
	// normalize-and-hex-decode pass.
	if len(workerEcho) > 256 {
		return errors.New("coordinator_ca_fingerprint too long")
	}
	workerNorm, err := clusterid.NormalizeFingerprint(workerEcho)
	if err != nil {
		return fmt.Errorf("invalid coordinator_ca_fingerprint: %w", err)
	}
	coordNorm, err := clusterid.NormalizeFingerprint(coordFingerprint)
	if err != nil {
		return fmt.Errorf("invalid coordinator fingerprint on server: %w", err)
	}
	if workerNorm != coordNorm {
		return errors.New("coordinator_ca_fingerprint does not match this coordinator's CA")
	}
	return nil
}

// CAFingerprint returns the coord's CA SPKI fingerprint in
// "sha256:<hex>" form. Empty on non-coord nodes.
func (n *Node) CAFingerprint() string {
	if n.ca == nil {
		return ""
	}
	return clusterid.Fingerprint(n.ca.Certificate())
}

// RecordPairingForTest is a cross-package test seam: lets
// internal/server tests drive the pairing admin handlers without
// spinning up a real worker to POST /cluster/pairing-request. Not for
// production use — the exported name carries the ForTest suffix so
// static analysis (and reviewers) flag accidental production callers.
func (n *Node) RecordPairingForTest(req *PairingRequest) (*PairingRequest, error) {
	if n.pairingStore == nil {
		return nil, ErrNotCoordinator
	}
	return n.pairingStore.Record(req)
}

// BuildTestCSR is a cross-package test seam: returns a CSR signed by
// the node's own identity key and CA-validatable, suitable for
// seeding test pending entries. Not for production use.
func (n *Node) BuildTestCSR(commonName string) (string, error) {
	if n.identity == nil {
		return "", ErrInvalidConfig
	}
	csr, err := n.identity.CSR(commonName, []string{commonName + ".local"}, nil)
	if err != nil {
		return "", err
	}
	return string(csr), nil
}

// WaitForPairingResultForTest is a cross-package test seam: delivers
// the PairingResult that Accept stashed for a given code. Used by
// internal/server tests that want to assert WHICH coordinator URL
// the admin-accept handler stored for the worker to persist, without
// spinning up a full worker poll loop. Not for production use.
func (n *Node) WaitForPairingResultForTest(ctx context.Context, code string) (*PairingResult, error) {
	if n.pairingStore == nil {
		return nil, ErrNotCoordinator
	}
	return n.pairingStore.WaitFor(ctx, code)
}

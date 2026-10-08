package clusternode

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/retry"
	"github.com/stperic/zzrouter/pkg/utils"
)

// renewalBackoff is the transient-failure backoff policy for the worker
// renewal loop. Jittered so fleets retrying after a coord reboot don't
// synchronize. Max equals renewBackoffMax (1h) matching the hourly
// renewCheckCadence invariant.
var renewalBackoff = retry.Backoff{
	Initial:    renewBackoffStart,
	Max:        renewBackoffMax,
	Multiplier: renewalBackoffMultiplier,
	Jitter:     renewalBackoffJitter,
}

// writeClusterFile writes data to path durably at 0600 (CA cert,
// coordinator URL, deny-list flushes). Parent dir 0700. Helper fsyncs
// file + dir.
func writeClusterFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return utils.AtomicWriteFile(path, data, 0o600)
}

// Renewal knobs. Plan §9 locks the TTL + renew-at + check-cadence:
//
//   - Worker certs live 30d.
//   - Worker checks remaining lifetime hourly.
//   - A cert with <= 15d remaining triggers renewal.
//   - Transient errors back off exponentially (1m → 2m → 4m → ... → 1h).
//   - Full expiry / deny-list 403 → self-heal to unclaimed.
const (
	renewThreshold    = 15 * 24 * time.Hour
	renewCheckCadence = time.Hour
	renewBackoffStart = time.Minute
	renewBackoffMax   = time.Hour
	renewHTTPTimeout  = 30 * time.Second

	// renewalBackoffMultiplier doubles the transient-failure wait each
	// consecutive miss, capped at renewBackoffMax.
	renewalBackoffMultiplier = 2.0
	// renewalBackoffJitter is ±20% of the computed delay so fleets of
	// workers don't retry renewals in lockstep after a coord reboot.
	renewalBackoffJitter = 0.2
)

// Renewal-specific errors the worker loop branches on. Exported so
// integration callers (internal/server) can observe them too.
var (
	ErrRenewDenyListed = errors.New("clusternode: worker cert revoked by coordinator")
	ErrRenewTransient  = errors.New("clusternode: transient renewal failure")
	ErrRenewExpired    = errors.New("clusternode: cert expired before renewal succeeded")
)

// ---------------------------------------------------------------------
// Deny list — coordinator-owned revocation state.
// ---------------------------------------------------------------------

// denyEntry is one revoked cert, keyed on its SPKI fingerprint. The
// fingerprint is stable across renewals (plan §9 binds renewals to the
// same keypair so SPKI never rolls), which is the property that makes
// revocation actually actionable for operators. Serial is recorded as
// forensic metadata only.
type denyEntry struct {
	Fingerprint string    `json:"fingerprint"`
	Serial      string    `json:"serial,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	RevokedAt   time.Time `json:"revoked_at"`
}

// denyList is the coordinator's in-memory view of revoked certs plus
// the JSON file it flushes to. Constructed lazily on first coordinator
// start; nil on non-coordinator nodes.
//
// Lock discipline: all reads and writes of entries go through the mu.
// Writes flush the full list to disk atomically (tempfile + rename) so
// concurrent reads never observe a half-written file on reload.
type denyList struct {
	mu       sync.RWMutex
	entries  map[string]denyEntry // keyed by fingerprint
	filePath string
}

// loadDenyList parses the deny list JSON file, if present. A missing
// file is not an error — the list is simply empty. Malformed JSON is
// an error: a corrupt deny list cannot be trusted, and the plan says
// the coordinator refuses to boot rather than silently accept zero
// revocations.
func loadDenyList(filePath string) (*denyList, error) {
	d := &denyList{
		entries:  make(map[string]denyEntry),
		filePath: filePath,
	}
	data, err := os.ReadFile(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read deny list: %w", err)
	}
	var rows []denyEntry
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse deny list: %w", err)
	}
	dropped := 0
	for _, e := range rows {
		if e.Fingerprint == "" {
			dropped++
			continue
		}
		d.entries[e.Fingerprint] = e
	}
	if dropped > 0 {
		slog.Warn("deny list: skipping rows with empty fingerprint",
			"path", filePath, "count", dropped)
	}
	return d, nil
}

// Contains reports whether the given SPKI fingerprint is on the deny list.
func (d *denyList) Contains(fingerprint string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.entries[fingerprint]
	return ok
}

// Add appends (or overwrites) a deny entry and flushes to disk. The
// entry must carry a non-empty Fingerprint; callers use
// pkg/cluster/id.Fingerprint(cert) to compute it.
//
// On flush failure the in-memory map is rolled back to its prior state
// so the coordinator doesn't observe a revoke that won't survive a
// restart. writeClusterFile is tempfile+rename so partial on-disk state
// is not possible; only the whole-file swap can fail.
func (d *denyList) Add(entry denyEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	prev, existed := d.entries[entry.Fingerprint]
	d.entries[entry.Fingerprint] = entry
	if err := d.flushLocked(); err != nil {
		if existed {
			d.entries[entry.Fingerprint] = prev
		} else {
			delete(d.entries, entry.Fingerprint)
		}
		return err
	}
	return nil
}

// flushLocked writes the current set to disk atomically. Caller holds
// d.mu.
func (d *denyList) flushLocked() error {
	// Serialize as a stable-order slice so the file diffs cleanly on
	// revoke/rotate and tests can assert exact contents.
	rows := make([]denyEntry, 0, len(d.entries))
	for _, e := range d.entries {
		rows = append(rows, e)
	}
	sortDenyEntries(rows)
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal deny list: %w", err)
	}
	return writeClusterFile(d.filePath, append(data, '\n'))
}

// sortDenyEntries sorts in-place by (RevokedAt, Fingerprint) so the
// on-disk file diffs cleanly on revoke/rotate and tests can assert
// exact contents.
func sortDenyEntries(rows []denyEntry) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].RevokedAt.Equal(rows[j].RevokedAt) {
			return rows[i].RevokedAt.Before(rows[j].RevokedAt)
		}
		return rows[i].Fingerprint < rows[j].Fingerprint
	})
}

// denyListPath returns the canonical path under the coordinator's
// CADir. Kept as a helper so the test fixture can assert the exact
// location.
func denyListPath(caDir string) string { return filepath.Join(caDir, "deny_list.json") }

// ---------------------------------------------------------------------
// Coordinator Revoke — the one revocation API we surface in v1.
// ---------------------------------------------------------------------

// Revoke appends a deny-list entry keyed on the worker's SPKI
// fingerprint. Serial is optional forensic metadata (it rolls on every
// renewal, so it is not the match key). Reason is a free-form operator
// note preserved verbatim on disk.
//
// Idempotent: re-revoking an already-denied fingerprint updates the
// reason + revoked_at timestamp without producing an error.
//
// Returns ErrNotCoordinator on a non-coordinator node.
func (n *Node) Revoke(fingerprint, serial, reason string) error {
	if n.Mode() != Coordinator || n.deny == nil {
		return ErrNotCoordinator
	}
	if fingerprint == "" {
		return fmt.Errorf("%w: empty fingerprint", ErrInvalidConfig)
	}
	return n.deny.Add(denyEntry{
		Fingerprint: fingerprint,
		Serial:      serial,
		Reason:      reason,
		RevokedAt:   utils.NowUTC(),
	})
}

// ---------------------------------------------------------------------
// mTLS middleware — OU check + deny-list lookup for mTLS routes.
// ---------------------------------------------------------------------

// revokedBodyMarker is the JSON field the coordinator sets when
// rejecting a client for being on the deny list. Workers treat 403
// responses as terminal (→ self-heal) ONLY when this marker is
// present; a bare 403 from a reverse proxy or the coordinator during
// a transient config reload is treated as transient instead, so an
// incidental forbidden doesn't permanently wipe the worker's state.
const revokedBodyMarker = `"revoked":true`

// mTLSOUCheck returns middleware that enforces:
//
//  1. A verified client-cert chain.
//  2. Client cert's Subject OU equals exactly requiredOU.
//  3. Client cert's SPKI fingerprint is NOT on the coordinator's deny
//     list (applied only when the node has a deny list; no-op on worker).
//
// Wired on /internal/* and /cluster/renew (worker-or-coordinator
// specific) and /cluster/leave (coordinator-only).
func (n *Node) mTLSOUCheck(requiredOU string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.TLS == nil || len(c.Request.TLS.VerifiedChains) == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": "client cert required"})
			return
		}
		peer := c.Request.TLS.PeerCertificates[0]
		if !hasOU(peer, requiredOU) {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"error": "client cert OU mismatch"})
			return
		}
		if n.deny != nil {
			if n.deny.Contains(clusterid.Fingerprint(peer)) {
				// "revoked":true is the structured marker workers
				// look for; a plain 403 is not treated as terminal
				// on the client side (see renewOnce response
				// handling).
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error":   "client cert revoked",
					"revoked": true,
				})
				return
			}
		}
		c.Next()
	}
}

// ---------------------------------------------------------------------
// /cluster/renew — coordinator receives a CSR, signs it, returns
// signed cert + CA. Worker-side renewal ticker is below.
// ---------------------------------------------------------------------

type renewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

// renewResponse carries only the signed cert — the worker ALREADY
// trusts the coordinator's CA (stored at pairing time) and accepting a
// CA back from the renew response is an attack surface with no
// corresponding upside.
type renewResponse struct {
	SignedCertPEM string `json:"signed_cert_pem"`
}

// handleRenew processes a renewal request. Mounted behind mTLSOUCheck
// with required OU = zzrouter-worker. The worker's client cert serial
// has already been checked against the deny list by the middleware, so
// by the time we arrive here the request is authorized.
//
// SECURITY: we require the CSR's public key to match the presented
// client cert's public key. Without this check, a worker authenticated
// with cert-A could submit a CSR carrying public-key-B and get a new
// cert binding its identity to key-B — effectively rotating keys to
// one the original identity-key compromise doesn't cover. Plan §9
// mandates "same keypair" across renewals; this enforces it at the
// coordinator.
func (n *Node) handleRenew(c *gin.Context) {
	var req renewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "invalid JSON body"})
		return
	}
	if req.CSRPEM == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "missing csr_pem"})
		return
	}
	// Parse the CSR so we can verify its public key against the
	// client cert's public key BEFORE asking the CA to sign.
	csr, err := parseCSRPEM([]byte(req.CSRPEM))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "invalid CSR: " + err.Error()})
		return
	}
	if c.Request.TLS == nil || len(c.Request.TLS.PeerCertificates) == 0 {
		// Middleware should have caught this; defense-in-depth.
		c.AbortWithStatusJSON(http.StatusUnauthorized,
			gin.H{"error": "no client cert"})
		return
	}
	clientPub := c.Request.TLS.PeerCertificates[0].PublicKey
	if !samePublicKey(csr.PublicKey, clientPub) {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "CSR public key does not match client cert"})
		return
	}

	signed, err := n.ca.Sign([]byte(req.CSRPEM), clusterid.RoleWorker)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			gin.H{"error": "sign CSR: " + err.Error()})
		return
	}
	// Do NOT return CACertPEM — the worker already has it. Returning
	// it opens an attack surface (worker trusts the response's CA)
	// with no corresponding upside.
	c.JSON(http.StatusOK, renewResponse{
		SignedCertPEM: string(signed),
	})
}

// parseCSRPEM parses a single CERTIFICATE REQUEST block. Shares intent
// with pkg/clusterid's parseAndVerifyCSR but is purely local to the
// renew handler's key-binding check — no signature verification here
// (pkg/clusterid's CA.Sign does it again on its own parse).
func parseCSRPEM(data []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a CERTIFICATE REQUEST PEM")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

// samePublicKey compares two crypto.PublicKey values. Uses the stdlib
// Equal method if available (all modern key types — ecdsa, ed25519,
// rsa — implement it), falling back to false for unknown types (the
// conservative choice — unknown is never equal).
func samePublicKey(a, b any) bool {
	type keyWithEqual interface {
		Equal(crypto.PublicKey) bool
	}
	ak, ok := a.(keyWithEqual)
	if !ok {
		return false
	}
	bp, ok := b.(crypto.PublicKey)
	if !ok {
		return false
	}
	return ak.Equal(bp)
}

// ---------------------------------------------------------------------
// /cluster/leave — coordinator decommissions a worker.
// ---------------------------------------------------------------------

// handleLeave receives a coordinator-initiated leave. Response is
// immediate (idempotent — a replayed leave on an already-unclaimed
// node short-circuits on mode), then self-heal runs asynchronously to
// go dormant on the cluster network.
//
// Mounted behind mTLSOUCheck with required OU = zzrouter-coordinator.
//
// Concurrent-leave safety: tryStartRevert CAS-claims the revert slot
// under n.mu. Two simultaneous leaves both observe Mode==Worker but
// only the first wins the CAS and spawns; the second returns 200
// cleanly without triggering a second teardown.
func (n *Node) handleLeave(c *gin.Context) {
	if !n.tryStartRevert("leave") {
		c.JSON(http.StatusOK, gin.H{"status": "noop"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// ErrResetNotWorker fires when ResetPairing is invoked on a node
// that isn't in Worker mode. Unclaimed needs no reset and
// Coordinator / Disabled have no pairing trust to clear — operators
// wanting a coord-side teardown should dismantle the cluster
// explicitly rather than route through the pairing surface.
var ErrResetNotWorker = errors.New("clusternode: reset requires worker mode")

// ResetPairing synchronously reverts a Worker node to Unclaimed,
// wiping its cluster CA + coordinator URL + signed cert. The
// identity key is preserved so the node's fingerprint survives the
// reset and the coord sees the same peer on re-pair.
//
// This is the admin-initiated counterpart to /cluster/leave. Unlike
// the coord-driven path (which returns 200 before the revert
// completes so the coord doesn't hang), this call blocks until
// mode has flipped to Unclaimed so the caller can safely run
// BeginPairing next.
//
// Returns ErrResetNotWorker when current mode != Worker. A revert
// already in flight (e.g. /leave + admin-reset racing) is joined
// rather than rejected.
func (n *Node) ResetPairing() error {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return errors.New("clusternode: node stopped")
	}
	cur := Mode(n.mode.Load())
	if cur != Worker {
		// Already Unclaimed / Disabled / Coordinator. If a revert is
		// already in flight (selfHealStarted), it'll have moved mode
		// out of Worker; treat that as "the caller's intent has been
		// fulfilled" rather than an error.
		if cur == Unclaimed {
			n.mu.Unlock()
			return nil
		}
		n.mu.Unlock()
		return fmt.Errorf("%w: current mode %s", ErrResetNotWorker, cur)
	}
	if n.selfHealStarted {
		// Another path is already reverting. We can't double-wg.Add;
		// reject rather than fight the ownership. The caller can
		// poll mode to see when the other revert completes.
		n.mu.Unlock()
		return errors.New("clusternode: revert already in flight: retry after it completes")
	}
	n.selfHealStarted = true
	n.wg.Add(1)
	n.mu.Unlock()
	// Run synchronously so caller observes mode=Unclaimed on return.
	// revertToUnclaimed handles wg.Done + all the state clearing.
	n.revertToUnclaimed("admin-reset")
	return nil
}

// tryStartRevert atomically claims the revert-to-unclaimed slot.
// Returns true if the caller should run the revert; false if the node
// is not in Worker mode, is being stopped, or a revert is already in
// flight. On true, the caller is responsible for nothing — this
// function has already done wg.Add(1) + `go revertToUnclaimed(reason)`.
func (n *Node) tryStartRevert(reason string) bool {
	n.mu.Lock()
	if n.stopped || n.selfHealStarted || Mode(n.mode.Load()) != Worker {
		n.mu.Unlock()
		return false
	}
	n.selfHealStarted = true
	n.wg.Add(1)
	n.mu.Unlock()
	go n.revertToUnclaimed(reason)
	return true
}

// ---------------------------------------------------------------------
// Worker → Unclaimed revert. Dormant semantics: drain mTLS listener,
// wipe ClusterDir, flip mode to Unclaimed, leave the node with NO
// cluster listener. Operator must run `zzrouter cluster pair` to
// re-enter pairing mode.
// ---------------------------------------------------------------------

// revertToUnclaimed drops the worker out of Worker mode. Triggered by:
//
//   - /cluster/leave from the coordinator
//   - renewal 403 (deny list)
//   - cert full expiry (renewal loop gave up)
//
// Identity key is PRESERVED so fingerprint survives the revert — the
// operator can re-pair the same node. Trusted CA + coordinator URL
// are wiped (they're no longer meaningful). The node goes dormant on
// the cluster network: no pairing code is generated, no cluster
// listener runs, no polling happens. Re-pairing is explicit operator
// intent (`zzrouter cluster pair`), which matches install-time
// semantics.
func (n *Node) revertToUnclaimed(reason string) {
	defer n.wg.Done()

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	oldSrv := n.httpServer
	// Stop the renewal ticker: cancelling renewalCancel unblocks the
	// ticker's Done select and lets it exit before we drop the
	// coordinator URL.
	if n.renewalCancel != nil {
		n.renewalCancel()
	}
	n.mu.Unlock()

	if oldSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = oldSrv.Shutdown(ctx)
		cancel()
	}

	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	n.httpServer = nil
	n.listener = nil
	n.listenerAddr = nil
	n.mode.Store(int32(Unclaimed))
	n.mu.Unlock()

	// Wipe the Worker-side on-disk state. The identity directory is
	// preserved — fingerprint must survive the cycle.
	_ = os.RemoveAll(n.cfg.ClusterDir)

	// Notify AFTER the unlock so callbacks can take their own locks
	// without deadlock risk against n.mu.
	n.notifyModeChange(Unclaimed, "revert: "+reason)

	slog.Info("clusternode: decommissioned by coordinator; run 'zzrouter cluster pair' to re-pair",
		"reason", reason)
}

// ---------------------------------------------------------------------
// Worker-side renewal ticker.
// ---------------------------------------------------------------------

// startRenewalTickerLocked spawns the renewal goroutine when the node
// boots as Worker. The ticker loop is wg-tracked; its lifetime is
// bounded by the renewalCtx which is cancelled on Stop or on self-heal.
//
// Each tick is wrapped in defer recover() so a panic in one tick
// doesn't wedge the worker — self-heal is the explicit recovery path
// for unrecoverable state, not crash-on-tick.
//
// PRECONDITION: caller holds n.mu. The sole caller today is Node.Start,
// which holds the lock for its entire body. A prior version took the
// lock inside this function, which self-deadlocked on worker boot
// because sync.Mutex is non-reentrant.
func (n *Node) startRenewalTickerLocked(ctx context.Context) {
	if n.Mode() != Worker {
		return
	}
	renewalCtx, cancel := context.WithCancel(ctx)
	n.renewalCancel = cancel

	n.wg.Add(1)
	go n.renewalLoop(renewalCtx)
}

// renewalLoop is the worker renewal goroutine. Runs under renewalCtx;
// exits cleanly on ctx.Done or after triggering self-heal.
func (n *Node) renewalLoop(ctx context.Context) {
	defer n.wg.Done()

	// transientFails counts consecutive renewal failures; reset on
	// success or when the cert's remaining lifetime is comfortable.
	transientFails := 0
	ticker := n.renewalClock.NewTicker(renewCheckCadence)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			n.runRenewalTick(ctx, &transientFails)
		}
	}
}

// runRenewalTick is one iteration of the renewal loop. Wrapped in
// defer-recover so a buggy branch can't kill the goroutine.
func (n *Node) runRenewalTick(ctx context.Context, transientFails *int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "clusternode: renewal tick panic: %v\n", r)
		}
	}()

	cert := n.identity.Certificate()
	if cert == nil {
		return
	}
	now := n.renewalClock.Now()
	remaining := cert.NotAfter.Sub(now)

	if remaining <= 0 {
		// Hard expiry — revert to Unclaimed. tryStartRevert
		// CAS-claims the slot; the ticker will exit when renewalCtx
		// is cancelled by the winning revert.
		n.tryStartRevert("cert expired")
		return
	}
	if remaining > renewThreshold {
		// Still comfortable; reset fail counter so a past flurry
		// doesn't leave us in a long wait.
		*transientFails = 0
		return
	}

	// Attempt renewal.
	err := n.renewOnce(ctx)
	switch {
	case err == nil:
		*transientFails = 0
	case errors.Is(err, ErrRenewDenyListed):
		// Coordinator told us we're revoked. Revert immediately.
		n.tryStartRevert("deny-listed")
	default:
		// Transient: sleep jittered backoff, bump counter for next round.
		delay := renewalBackoff.Delay(*transientFails)
		fmt.Fprintf(os.Stderr, "clusternode: renewal failed (%v), backing off %s\n", err, delay)
		_ = retry.Sleep(ctx, delay)
		*transientFails++
	}
}

// renewOnce performs a single renewal POST. Generates a fresh CSR
// against the existing keypair, calls /cluster/renew on the
// coordinator, InstallSignedCert on success.
func (n *Node) renewOnce(ctx context.Context) error {
	csr, err := n.identity.CSR(n.cfg.NodeName, n.cfg.advertiseDNSNames(), n.cfg.advertiseIPs())
	if err != nil {
		return fmt.Errorf("renew CSR: %w", err)
	}
	client, err := n.renewMTLSClient()
	if err != nil {
		return fmt.Errorf("renew client: %w", err)
	}
	body, err := json.Marshal(renewRequest{CSRPEM: string(csr)})
	if err != nil {
		return fmt.Errorf("marshal renew: %w", err)
	}
	// Client has no Timeout — bound the POST via ctx deadline instead.
	reqCtx, cancel := context.WithTimeout(ctx, renewHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		n.cfg.CoordinatorURL+"/zzrouter/v1/cluster/renew", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("renew request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRenewTransient, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode == http.StatusForbidden {
		// Treat as deny-listed ONLY if the coordinator set the
		// structured marker. A bare 403 (reverse proxy, transient
		// coordinator config reload, unexpected middleware) is
		// transient — do NOT wipe worker state on it.
		if bytes.Contains(respBody, []byte(revokedBodyMarker)) {
			return ErrRenewDenyListed
		}
		return fmt.Errorf("%w: 403 without revoke marker", ErrRenewTransient)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrRenewTransient, resp.StatusCode)
	}
	var out renewResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("%w: decode: %w", ErrRenewTransient, err)
	}

	// Verify the returned cert chains to the CA WE ALREADY TRUST.
	// Without this, a coordinator compromise (or MITM past our mTLS
	// trust pool) could swap our cert for one signed by a different
	// CA — which would still install successfully via
	// InstallSignedCert's key-match check.
	if err := n.verifyRenewedCert([]byte(out.SignedCertPEM)); err != nil {
		return fmt.Errorf("renewed cert chain verify: %w", err)
	}

	if err := n.identity.InstallSignedCert([]byte(out.SignedCertPEM)); err != nil {
		return fmt.Errorf("install renewed cert: %w", err)
	}
	return nil
}

// verifyRenewedCert parses the signed cert from the renew response and
// verifies it chains to the CA stored in ClusterDir. This is paired
// with InstallSignedCert's public-key check (cert pubkey == our key)
// to ensure the renewed cert is both "for us" and "from our coordinator."
func (n *Node) verifyRenewedCert(pemBytes []byte) error {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("not a CERTIFICATE PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(n.cfg.ClusterDir, "ca.pem"))
	if err != nil {
		return fmt.Errorf("read stored CA: %w", err)
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return errors.New("stored CA is not a CERTIFICATE PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse stored CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return fmt.Errorf("chain verify: %w", err)
	}
	return nil
}

// renewMTLSClient wraps DialClient with the renew-specific peer OU
// expectation (coordinator). Ctx deadline — not a client-level Timeout
// — bounds the single renewal POST; see renewOnce.
func (n *Node) renewMTLSClient() (*http.Client, error) {
	return n.DialClient(clusterid.RoleCoordinator.OU())
}

// renewalClock lets tests speed up the hourly tick. Production uses
// the real monotonic clock.
type renewalClock interface {
	Now() time.Time
	NewTicker(d time.Duration) renewalTicker
}

type renewalTicker interface {
	C() <-chan time.Time
	Stop()
}

// Real clock impl — trivial wrapper around time.NewTicker / utils.Now.
type realClock struct{}

func (realClock) Now() time.Time { return utils.Now() }
func (realClock) NewTicker(d time.Duration) renewalTicker {
	return realTicker{time.NewTicker(d)}
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

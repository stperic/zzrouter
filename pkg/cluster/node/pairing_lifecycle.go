package clusternode

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Pairing code shape. 80 bits = 10 bytes = exactly 16 base32 chars,
// using the standard A-Z2-7 alphabet that already avoids most
// visually-confusable characters (no 0/O/1/I/l). 80 bits is overkill
// against online guessing at the per-IP + global rate limits but
// fits cleanly in a 16-char display.
const (
	pairingBits     = 80
	pairingBytes    = pairingBits / 8
	pairingCodeLen  = 16 // ceil(80 / 5) for base32
	pairingTTL      = 15 * time.Minute
	pairingFilePerm = 0o600
)

// generatePairingCode returns a 16-char base32 code carrying 80 bits
// of entropy. The standard base32 alphabet (A-Z2-7) already excludes
// 0/1/8/9 — the characters that confuse operators on terminals with
// narrow fonts.
func generatePairingCode() (string, error) {
	var b [pairingBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	// StdEncoding with padding disabled. NoPadding because our 80-bit
	// input is a multiple of 40 bits — no padding is produced anyway.
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}

// IsValidPairingCode reports whether code matches the wire format
// produced by generatePairingCode: exactly pairingCodeLen chars,
// each in the base32 alphabet A-Z2-7. Single source of truth for
// format validation — used by both the coord-side /cluster/pairing-
// request handler (reject poisoned entries at Record time) and the
// admin-side /pairing/accept handler (reject malformed pastes
// without scanning the store).
//
// Not a secret check; format validation is strictly a noise filter
// to produce clear 400s on obvious typos.
func IsValidPairingCode(code string) bool {
	if len(code) != pairingCodeLen {
		return false
	}
	for _, r := range code {
		if !((r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7')) {
			return false
		}
	}
	return true
}

// nodeListen is the injectable version of net.Listen used by
// completePairing. Production nodes have listenFn = net.Listen; tests
// can override to simulate bind failures.
func nodeListen(n *Node, addr string) (net.Listener, error) {
	return n.listenFn("tcp", addr)
}

// Sentinel errors for BeginPairing / CancelPairing / completePairing.
// Callers match via errors.Is so the CLI + admin handler can branch
// without string-scraping.
var (
	// ErrPairingNotUnclaimed is returned when BeginPairing / CancelPairing
	// is invoked on a node that isn't Unclaimed. Surfaces as 400 at
	// the admin HTTP layer.
	ErrPairingNotUnclaimed = errors.New("clusternode: pairing requires unclaimed mode")

	// ErrPairingURLMissing is returned when BeginPairing is invoked
	// with an empty CoordinatorURL. The fingerprint is optional
	// (TOFU bootstrap), but the URL is always required.
	ErrPairingURLMissing = errors.New("clusternode: coordinator url required")

	// ErrPairingConfigMissing is kept as an umbrella sentinel that
	// wraps ErrPairingURLMissing / ErrPairingCAFingerprintMalformed.
	// Handlers that only need a coarse "bad pairing config" signal
	// can keep matching on this; callers that want to distinguish
	// URL-missing from fingerprint-malformed match the specific
	// sentinels above.
	ErrPairingConfigMissing = errors.New("clusternode: pairing config invalid")

	// ErrPairingNoActiveWindow is returned by CancelPairing when no
	// window is active. Callers may treat this as success; the admin
	// handler returns 200 with status=no_active_window.
	ErrPairingNoActiveWindow = errors.New("clusternode: no active pairing window")
)

// BeginPairingOptions is the input to BeginPairing. CoordinatorURL is
// required; CAFingerprint is optional (empty → TOFU bootstrap).
// Regenerate toggles fresh-code vs idempotent re-entry.
type BeginPairingOptions struct {
	CoordinatorURL string
	CAFingerprint  string
	Regenerate     bool
}

// PairingWindowInfo is the read-only view of an active pairing window.
// Code is returned here because the operator needs to see it — the
// redaction in PendingSummary is coord-side, for different reasons.
type PairingWindowInfo struct {
	Code     string
	Deadline time.Time
}

// pairingWindow is the worker-side record of an active pairing
// attempt. One at a time; guarded by n.pairingMu.
type pairingWindow struct {
	code     string
	deadline time.Time

	ctx    context.Context
	cancel context.CancelFunc

	// timer fires at deadline to auto-close the window if the loop
	// hasn't resolved one way or the other. Close via cancel() —
	// simpler than a second signaling channel.
	timer *time.Timer

	// done is closed by the loop goroutine when it exits (success,
	// expiry, or ctx-cancel). Used by Stop() paths to avoid leaking
	// the goroutine.
	done chan struct{}
}

// BeginPairing opens a pairing window and starts the worker-side poll
// loop. Idempotent unless Regenerate is true: re-invoking on an
// already-active window returns the existing code + deadline without
// touching the on-disk file or restarting the loop.
//
// On success, the caller gets back (code, deadline, nil). The loop
// runs until one of:
//
//   - Coord returns approved → completePairing fires, mode flips to
//     Worker, window closes.
//   - Coord returns expired (or local TTL fires) → window closes,
//     node remains Unclaimed, pairing.txt removed.
//   - CancelPairing is invoked → window closes.
//   - Node.Stop fires → window closes.
func (n *Node) BeginPairing(opts BeginPairingOptions) (PairingWindowInfo, error) {
	if n.Mode() != Unclaimed {
		return PairingWindowInfo{}, fmt.Errorf("%w: current mode %s", ErrPairingNotUnclaimed, n.Mode())
	}
	if opts.CoordinatorURL == "" {
		return PairingWindowInfo{}, fmt.Errorf("%w: coordinator_url empty", ErrPairingURLMissing)
	}
	// CAFingerprint is optional (empty → TOFU). Only validate format
	// when the operator supplied one; surface malformed values via
	// ErrPairingCAFingerprintMalformed so the handler can distinguish
	// from URL-missing.
	if opts.CAFingerprint != "" {
		if _, err := normalizeCAFingerprint(opts.CAFingerprint); err != nil {
			return PairingWindowInfo{}, err
		}
	}

	n.pairingMu.Lock()
	defer n.pairingMu.Unlock()

	if n.activePairingWindow != nil && !opts.Regenerate {
		w := n.activePairingWindow
		return PairingWindowInfo{Code: w.code, Deadline: w.deadline}, nil
	}
	if n.activePairingWindow != nil && opts.Regenerate {
		n.cancelWindowLocked()
	}

	code, err := generatePairingCode()
	if err != nil {
		return PairingWindowInfo{}, fmt.Errorf("generate code: %w", err)
	}
	if err := writePairingCodeFile(n.cfg.PairingPath, code); err != nil {
		return PairingWindowInfo{}, fmt.Errorf("persist pairing code: %w", err)
	}

	csr, err := n.identity.CSR(n.cfg.NodeName, n.cfg.advertiseDNSNames(), n.cfg.advertiseIPs())
	if err != nil {
		_ = os.Remove(n.cfg.PairingPath)
		return PairingWindowInfo{}, fmt.Errorf("generate CSR: %w", err)
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	deadline := utils.Now().Add(pairingTTL)
	timer := time.AfterFunc(pairingTTL, cancel)
	doneCh := make(chan struct{})

	w := &pairingWindow{
		code:     code,
		deadline: deadline,
		ctx:      loopCtx,
		cancel:   cancel,
		timer:    timer,
		done:     doneCh,
	}
	n.activePairingWindow = w

	sans := append([]string(nil), n.cfg.advertiseDNSNames()...)
	go n.runPairingAttempt(loopCtx, doneCh, w, pairingClientOpts{
		coordURL:      opts.CoordinatorURL,
		caFingerprint: opts.CAFingerprint,
		code:          code,
		nodeName:      n.cfg.NodeName,
		fingerprint:   n.Fingerprint(),
		csrPEM:        string(csr),
		sans:          sans,
	})

	return PairingWindowInfo{Code: code, Deadline: deadline}, nil
}

// CancelPairing cancels the active pairing window (if any) and removes
// pairing.txt. Safe to call from multiple goroutines. Returns
// ErrPairingNoActiveWindow when no window is active; callers routinely
// swallow that.
func (n *Node) CancelPairing() error {
	n.pairingMu.Lock()
	defer n.pairingMu.Unlock()
	if n.activePairingWindow == nil {
		return ErrPairingNoActiveWindow
	}
	n.cancelWindowLocked()
	return nil
}

// PairingWindowInfoSnapshot returns the active window's code +
// deadline, or ok=false if no window is active. Used by the admin
// handler's GET path and the CLI to display window state.
func (n *Node) PairingWindowInfoSnapshot() (PairingWindowInfo, bool) {
	n.pairingMu.Lock()
	defer n.pairingMu.Unlock()
	if n.activePairingWindow == nil {
		return PairingWindowInfo{}, false
	}
	w := n.activePairingWindow
	return PairingWindowInfo{Code: w.code, Deadline: w.deadline}, true
}

// ConsumeLastPairingError returns the most recent terminal error
// from the pairing loop (if any) and clears it. Used by the admin
// handler to surface strict-mode 403s (and other terminal failures)
// to the operator on the next poll after the window closed, so the
// TUI doesn't fall back to a generic "no active window" message.
//
// One-shot by design: each error is delivered exactly once so a
// stale error from a prior attempt doesn't bleed into a fresh
// window's state.
func (n *Node) ConsumeLastPairingError() error {
	n.pairingMu.Lock()
	defer n.pairingMu.Unlock()
	err := n.lastPairingError
	n.lastPairingError = nil
	return err
}

// cancelWindowLocked tears down the current window. Stops the TTL
// timer, cancels the loop ctx, removes pairing.txt, and nils the
// active-window pointer. Caller holds n.pairingMu.
func (n *Node) cancelWindowLocked() {
	w := n.activePairingWindow
	if w == nil {
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	w.cancel()
	_ = os.Remove(n.cfg.PairingPath)
	n.activePairingWindow = nil
}

// runPairingAttempt wraps runPairingLoop with the on-success handoff to
// completePairing and cleanup on every exit path. Spawned by
// BeginPairing as the pairing-loop goroutine.
func (n *Node) runPairingAttempt(ctx context.Context, doneCh chan struct{}, w *pairingWindow, opts pairingClientOpts) {
	defer close(doneCh)

	result, err := runPairingLoop(ctx, opts)
	if err != nil {
		// Any non-nil err means we did NOT flip to Worker. Clear
		// window + on-disk code so the operator can retry. Stash
		// terminal reasons (strict-mode 403, etc.) so the admin
		// handler surfaces them on the next poll — otherwise the
		// operator sees a generic "no active window" and doesn't
		// learn to re-run with --secure / --ca-fingerprint.
		n.pairingMu.Lock()
		if n.activePairingWindow == w {
			if w.timer != nil {
				w.timer.Stop()
			}
			_ = os.Remove(n.cfg.PairingPath)
			n.activePairingWindow = nil
		}
		// Ctx-canceled is the operator's own doing (Cancel or
		// Stop); don't pollute the error surface with it.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			n.lastPairingError = err
		}
		n.pairingMu.Unlock()
		return
	}

	if err := n.completePairing(result.signedCertPEM, result.caCertPEM, result.coordinatorURL); err != nil {
		// completePairing failed after the coord committed (code was
		// burned on coord side). We're in a broken state: worker
		// still Unclaimed, coord thinks paired. Operator must
		// intervene. Clear window state so the CLI doesn't keep
		// showing a live code that will never resolve.
		n.pairingMu.Lock()
		if n.activePairingWindow == w {
			if w.timer != nil {
				w.timer.Stop()
			}
			_ = os.Remove(n.cfg.PairingPath)
			n.activePairingWindow = nil
		}
		n.pairingMu.Unlock()
		fmt.Fprintf(os.Stderr, "clusternode: pairing completed on coord but local install failed: %v\n", err)
		return
	}

	// completePairing succeeded. Window state already cleared inside
	// completePairing under the pairing lock.
}

// completePairing binds the mTLS cluster listener, persists the CA +
// coordinator URL + signed cert, and flips Mode Unclaimed→Worker.
//
// An Unclaimed node has NO active cluster listener (it went dormant
// at boot); this function binds the first listener for the worker-
// track lifetime. The selfHealStarted gate is reset so the new Worker
// cycle can revert cleanly if /cluster/leave or a deny-list 403
// fires later.
//
// Ordering (each step rolls back everything prior on failure):
//
//  1. Bind the new mTLS listener — cheapest rollback (just Close).
//  2. Persist CA + coordinator_url to disk.
//  3. InstallSignedCert — last mutation before mode flip.
//  4. Under n.mu: flip mode, install server, spawn serve. `go serve`
//     runs inside the lock so a concurrent Stop → shutdownServer
//     can't observe (and nil) httpServer between the assignment and
//     the serve-goroutine spawn.
//  5. Clear pairing window, fire OnModeChange, start renewal ticker.
//
// Step ordering matters: a bind failure in step 1 leaves zero disk
// mutation. A write failure in step 2 leaves the listener bound but
// no on-disk state → closed on return. An InstallSignedCert failure
// in step 3 leaves disk CA+URL but no cert → operator runs
// `zzrouter cluster pair --regenerate` to retry, identity key and
// thus fingerprint survive.
func (n *Node) completePairing(signedCertPEM, caCertPEM []byte, coordURL string) error {
	n.mu.Lock()
	stopped := n.stopped
	n.mu.Unlock()
	if stopped {
		return errors.New("node stopped during pairing completion")
	}

	// 1. Bind the mTLS listener first — if this fails nothing else
	// has mutated and the caller can surface a clean error.
	addr := net.JoinHostPort(n.cfg.BindHost, strconv.Itoa(n.cfg.Port))
	newLn, err := nodeListen(n, addr)
	if err != nil {
		return fmt.Errorf("mtls listen %s: %w", addr, err)
	}

	// 2. Persist CA + coordinator URL to disk.
	if err := os.MkdirAll(n.cfg.ClusterDir, 0o700); err != nil {
		_ = newLn.Close()
		return fmt.Errorf("mkdir cluster dir: %w", err)
	}
	caPath := filepath.Join(n.cfg.ClusterDir, "ca.pem")
	if err := writeClusterFile(caPath, caCertPEM); err != nil {
		_ = newLn.Close()
		return fmt.Errorf("persist CA: %w", err)
	}
	urlPath := filepath.Join(n.cfg.ClusterDir, "coordinator_url")
	if err := writeClusterFile(urlPath, []byte(coordURL)); err != nil {
		_ = newLn.Close()
		return fmt.Errorf("persist coordinator URL: %w", err)
	}

	// 3. Install the signed cert. InstallSignedCert re-validates
	// that the cert's public key matches our long-term keypair.
	if err := n.identity.InstallSignedCert(signedCertPEM); err != nil {
		_ = newLn.Close()
		return fmt.Errorf("install signed cert: %w", err)
	}

	// 4. Atomic mode flip + listener install + serve-spawn under mu.
	// Spawning `go serve` WHILE holding mu closes the window where a
	// concurrent Stop → shutdownServer could observe the new
	// httpServer, call Shutdown on a server that hasn't yet called
	// ServeTLS, and leak the listener fd.
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		_ = newLn.Close()
		return errors.New("node stopped during pairing completion")
	}
	n.selfHealStarted = false
	n.mode.Store(int32(Worker))
	newSrv := &http.Server{
		Handler:           n.buildHandler(),
		TLSConfig:         n.buildTLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	n.httpServer = newSrv
	n.listener = newLn
	n.listenerAddr = newLn.Addr()
	n.servingDone = make(chan struct{})
	servingDone := n.servingDone
	n.wg.Add(1)
	go n.serve(newSrv, newLn, servingDone)
	n.mu.Unlock()

	// 5. Clear the pairing window now that the forward path is
	// committed.
	n.pairingMu.Lock()
	if n.activePairingWindow != nil {
		if n.activePairingWindow.timer != nil {
			n.activePairingWindow.timer.Stop()
		}
		_ = os.Remove(n.cfg.PairingPath)
		n.activePairingWindow = nil
	}
	n.pairingMu.Unlock()

	n.notifyModeChange(Worker, "pairing succeeded")

	// The renewal ticker was not started at Start on an Unclaimed
	// node (no Worker mode yet). Start it now that mode has flipped.
	// Tracked in wg inside startRenewalTickerLocked; bound to the
	// node's shutdown ctx captured at Start so Stop cleans it up.
	n.mu.Lock()
	// Re-check stopped: notifyModeChange above runs callbacks unlocked
	// and can be slow, opening a window where Stop sets stopped=true
	// and proceeds to wg.Wait(). startRenewalTickerLocked does
	// wg.Add(1); a concurrent wg.Wait() with counter==0 panics
	// ("sync: WaitGroup is reused before previous Wait has returned").
	if n.stopped {
		n.mu.Unlock()
		return nil
	}
	shutdownCtx := n.shutdownCtx
	if shutdownCtx == nil {
		shutdownCtx = context.Background()
	}
	n.startRenewalTickerLocked(shutdownCtx)
	n.mu.Unlock()
	return nil
}

// writePairingCodeFile writes the pairing code durably at
// pairingFilePerm. Parent dir 0700. Helper fsyncs file + dir.
func writePairingCodeFile(path, code string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("pairing mkdir: %w", err)
	}
	return utils.AtomicWriteFile(path, []byte(code+"\n"), pairingFilePerm)
}

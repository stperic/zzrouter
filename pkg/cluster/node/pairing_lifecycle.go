package clusternode

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/version"

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

// IsValidPairingCode validates the printable enrollment code format.
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

// PairingWindowInfo carries the operator inputs needed by coordinator acceptance.
type PairingWindowInfo struct {
	Code        string
	Deadline    time.Time
	WorkerURL   string
	Fingerprint string
}

// pairingWindow is the worker-side record of an active pairing
// attempt. One at a time; guarded by n.pairingMu.
type pairingWindow struct {
	listener net.Listener
	server   *http.Server
	request  pairingRequestBody
	options  BeginPairingOptions

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

// BeginPairing arms a code-protected TLS listener until enrollment, cancellation or expiry.
// Admission, regeneration and trust installation serialize through pairingMu.
func (n *Node) BeginPairing(ctx context.Context, opts BeginPairingOptions) (PairingWindowInfo, error) {
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
	if err := ctx.Err(); err != nil {
		return PairingWindowInfo{}, err
	}
	if n.Mode() != Unclaimed {
		return PairingWindowInfo{}, ErrPairingNotUnclaimed
	}

	if n.activePairingWindow != nil && !opts.Regenerate && n.activePairingWindow.ctx.Err() == nil && utils.Now().Before(n.activePairingWindow.deadline) {
		w := n.activePairingWindow
		return n.windowInfo(w), nil
	}
	if n.activePairingWindow != nil {
		n.cancelWindowLocked()
	}
	// An expired cluster certificate must not prevent explicit re-enrollment.
	if err := n.identity.ResetCertificate(); err != nil {
		return PairingWindowInfo{}, fmt.Errorf("prepare enrollment identity: %w", err)
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

	n.mu.Lock()
	parent := n.shutdownCtx
	n.mu.Unlock()
	if parent == nil {
		_ = os.Remove(n.cfg.PairingPath)
		return PairingWindowInfo{}, errors.New("start the node before opening enrollment")
	}
	loopCtx, cancel := context.WithCancel(parent)
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

	w.options = opts
	w.request = pairingRequestBody{Code: code, NodeName: n.cfg.NodeName, Fingerprint: n.Fingerprint(), CSRPEM: string(csr), SANs: n.cfg.advertiseDNSNames(), ClusterProtocol: version.ClusterProtocolVersion, MinClusterProtocol: version.MinClusterProtocolVersion, CoordinatorCAFingerprint: opts.CAFingerprint}
	n.mu.Lock()
	if n.stopped || !n.started || ctx.Err() != nil {
		n.mu.Unlock()
		n.cancelWindowLocked()
		close(doneCh)
		if err := ctx.Err(); err != nil {
			return PairingWindowInfo{}, err
		}
		return PairingWindowInfo{}, errors.New("start the node before opening enrollment")
	}
	ln, err := nodeListen(n, net.JoinHostPort(n.cfg.BindHost, strconv.Itoa(n.cfg.Port)))
	if err != nil {
		n.mu.Unlock()
		n.cancelWindowLocked()
		close(doneCh)
		return PairingWindowInfo{}, err
	}
	w.listener = ln
	n.listenerAddr = ln.Addr()
	tlsConfig := n.buildTLSConfig()
	tlsConfig.ClientAuth = tls.NoClientCert
	tlsConfig.VerifyConnection = nil
	w.server = &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { n.handleEnrollment(w, rw, r) }), TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return w.ctx }}
	n.wg.Add(1)
	n.mu.Unlock()
	go n.serveEnrollment(w) //nolint:gosec // Admitted enrollment belongs to the node lifecycle, independently of this request.
	return n.windowInfo(w), nil
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
	return n.windowInfo(w), true
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
	if w.listener != nil {
		_ = w.listener.Close()
	}
	w.cancel()
	_ = os.Remove(n.cfg.PairingPath)
	n.activePairingWindow = nil
}

// completePairing installs the coordinator-signed identity and starts the worker listener.
func (n *Node) completePairing(signedCertPEM, caCertPEM []byte, coordURL string) error {
	n.pairingMu.Lock()
	err := n.installPairingLocked(signedCertPEM, caCertPEM, coordURL)
	n.pairingMu.Unlock()
	if err == nil {
		n.pairingInstalled()
	}
	return err
}

// pairingMu precedes mu so cancellation cannot overlap the trust commit.
// Post-rename warnings still activate committed trust so runtime matches disk.
func (n *Node) installPairingLocked(signedCertPEM, caCertPEM []byte, coordURL string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return errors.New("node stopped during enrollment")
	}
	port := strconv.Itoa(n.cfg.Port)
	if w := n.activePairingWindow; w != nil && w.listener != nil {
		_, port, _ = net.SplitHostPort(w.listener.Addr().String())
		_ = w.listener.Close()
	}
	addr := net.JoinHostPort(n.cfg.BindHost, port)
	ln, err := nodeListen(n, addr)
	if err != nil {
		n.cancelWindowLocked()
		return fmt.Errorf("mTLS listen: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = ln.Close()
			n.cancelWindowLocked()
		}
	}()
	if err := os.MkdirAll(n.cfg.ClusterDir, 0o700); err != nil {
		return err
	}
	if err := writeClusterFile(filepath.Join(n.cfg.ClusterDir, "ca.pem"), caCertPEM); err != nil {
		if !errors.Is(err, atomicfile.ErrPostRenameDurability) {
			return err
		}
		fmt.Fprintf(os.Stderr, "clusternode: pairing CA committed with uncertain durability: %v\n", err)
	}
	if err := writeClusterFile(filepath.Join(n.cfg.ClusterDir, "coordinator_url"), []byte(coordURL)); err != nil {
		if !errors.Is(err, atomicfile.ErrPostRenameDurability) {
			return err
		}
		fmt.Fprintf(os.Stderr, "clusternode: pairing coordinator URL committed with uncertain durability: %v\n", err)
	}
	if err := n.identity.InstallSignedCert(signedCertPEM); err != nil {
		if !errors.Is(err, atomicfile.ErrPostRenameDurability) {
			return err
		}
		fmt.Fprintf(os.Stderr, "clusternode: pairing certificate committed with uncertain durability: %v\n", err)
	}
	n.selfHealStarted = false
	n.coordinatorURL = coordURL
	n.signals.Reset()
	n.mode.Store(int32(Worker))
	srv := &http.Server{Handler: n.buildHandler(), TLSConfig: n.buildTLSConfig(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return n.shutdownCtx }}
	n.httpServer = srv
	n.listener = ln
	n.listenerAddr = ln.Addr()
	n.servingDone = make(chan struct{})
	n.wg.Add(1)
	go n.serve(srv, ln, n.servingDone)
	committed = true
	n.cancelWindowLocked()
	return nil
}
func (n *Node) pairingInstalled() {
	n.notifyModeChange(Worker, "coordinator enrollment succeeded")
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.stopped && n.shutdownCtx != nil {
		n.startRenewalTickerLocked(n.shutdownCtx)
	}
}

// writePairingCodeFile writes the pairing code durably at
// pairingFilePerm. Parent dir 0700. Helper fsyncs file + dir.
func writePairingCodeFile(path, code string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("pairing mkdir: %w", err)
	}
	return atomicfile.WriteFile(path, []byte(code+"\n"), pairingFilePerm)
}

// Package clusternode is the self-contained cluster-port HTTP server
// for zzrouter. It owns the identity, the CA (on coordinator nodes),
// the worker-initiated pairing flow (client loop + coord store), the
// renewal ticker, and the deny list — in short, the entire
// cluster-side of the coordinator↔worker relationship.
//
// This package imports from pkg/cluster/id and has zero imports from
// internal/server. The integration seam is the inference handler
// passed via SetInferenceHandler (a plain http.Handler invoked for
// /zzrouter/v1/internal/* dispatches on worker nodes).
//
// Four modes, persisted in node.yaml as Cluster.Mode:
//
//   - Disabled    — standalone; cluster listener does not run.
//   - Coordinator — owns the CA; serves /cluster/pairing-request,
//     /cluster/ca-fingerprint, /cluster/renew; accepts
//     admin POST /cluster/pairing/accept to sign CSRs.
//   - Unclaimed   — worker-track, dormant on the cluster network. No
//     cluster listener. Enters pairing mode via
//     BeginPairing (driven by the admin CLI); the
//     worker polls the coord's long-poll endpoint under
//     CA-pinned TLS until approved or expired.
//   - Worker      — paired, auto-renews. mTLS listener serves
//     /internal/* dispatches from the coordinator.
//
// Runtime transitions: Unclaimed → Worker on successful pairing
// (completePairing binds the mTLS listener), Worker → Unclaimed on
// coord-driven decommission / cert expiry / deny-list 403
// (revertToUnclaimed tears down the mTLS listener and goes dormant).
// No runtime coordinator↔worker swap.
//
// Relationship to pkg/cluster/role: Node.Mode is the authoritative
// cluster-listener state; role.Role is the server-wide observable
// the rest of the codebase (route gates, handlers, audit logs) reads.
// The mapping is one-to-one — see role.RoleFromMode for the
// translator and internal/server/cluster_mode_mirror.go for the
// mirror wiring that keeps role.Manager in sync with this Mode.
package clusternode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

// Mode is the runtime cluster mode. Stored atomically on the Node so
// reads on the request hot path are lock-free.
type Mode int32

const (
	// Disabled is standalone mode — the cluster listener does not run.
	Disabled Mode = iota
	// Coordinator owns the CA and dials workers.
	Coordinator
	// Unclaimed is a worker-track node pre-pairing. Dormant on the
	// cluster network — no listener runs. Enters pairing mode via
	// BeginPairing (driven by the admin CLI).
	Unclaimed
	// Worker is a paired worker node. Serves the mTLS listener with
	// a CA-signed identity cert.
	Worker
)

// String returns the canonical lowercase name of the mode. Used in YAML
// persistence, the 501-denial body, logs, and test names.
func (m Mode) String() string {
	switch m {
	case Disabled:
		return "disabled"
	case Coordinator:
		return "coordinator"
	case Unclaimed:
		return "unclaimed"
	case Worker:
		return "worker"
	default:
		return fmt.Sprintf("mode(%d)", int32(m))
	}
}

// MarshalText implements encoding.TextMarshaler so Mode round-trips
// through YAML and JSON as the lowercase string form.
func (m Mode) MarshalText() ([]byte, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("%w: %d", ErrInvalidMode, int32(m))
	}
	return []byte(m.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. Unknown values are
// rejected — we don't silently default to Disabled because that would
// mask config typos.
func (m *Mode) UnmarshalText(text []byte) error {
	switch string(text) {
	case "disabled":
		*m = Disabled
	case "coordinator":
		*m = Coordinator
	case "unclaimed":
		*m = Unclaimed
	case "worker":
		*m = Worker
	default:
		return fmt.Errorf("%w: %q", ErrInvalidMode, string(text))
	}
	return nil
}

// Valid reports whether m is one of the four known modes.
func (m Mode) Valid() bool {
	return m == Disabled || m == Coordinator || m == Unclaimed || m == Worker
}

// Sentinel errors. Callers match with errors.Is.
var (
	ErrInvalidMode                = errors.New("clusternode: invalid cluster mode")
	ErrInvalidConfig              = errors.New("clusternode: invalid config")
	ErrNotCoordinator             = errors.New("clusternode: operation requires coordinator mode")
	ErrAlreadyStarted             = errors.New("clusternode: node already started")
	ErrListenerFailed             = errors.New("clusternode: listener failed")
	ErrAdminAPIHandlerMissing     = errors.New("clusternode: admin-api handler required for worker-track mode")
	ErrWorkerCompatHandlerMissing = errors.New("clusternode: worker-compat handler required for worker-track mode")
)

// Config holds the configuration for a Node. It's a runtime struct owned
// by this package; internal/server builds it from its own on-disk YAML.
// This package never imports pkg/config.
type Config struct {
	// Mode is the initial cluster mode. It may change at runtime on a
	// successful pairing (unclaimed→worker) or on coord-driven
	// decommission / cert expiry (worker→unclaimed).
	Mode Mode

	// Port is the TCP port the cluster listener binds. Zero means
	// OS-assigned (for tests); production callers pass 9091 or
	// whatever node.yaml configures.
	Port int

	// BindHost is the interface to bind on. Empty means all interfaces
	// ("" is net.Listen's "any").
	BindHost string

	// IdentityDir is where node.key / node.pem live. Required.
	IdentityDir string

	// CADir is where ca.key / ca.pem live. Required iff Mode is
	// Coordinator. Ignored otherwise.
	CADir string

	// ClusterDir is where a Worker persists its trusted CA cert and
	// coordinator URL. Required iff Mode is Worker. Ignored otherwise.
	ClusterDir string

	// CoordinatorURL is the Worker's coordinator endpoint. Required iff
	// Mode is Worker.
	CoordinatorURL string

	// NodeName is a human-readable label shown in logs, the banner, and
	// the pairing-request payload. Required iff Mode is Unclaimed,
	// Worker, or Coordinator. Callers typically set this to the OS
	// hostname or a configured friendly name.
	NodeName string

	// AdvertiseDNSNames lists DNS SANs the worker requests in its CSR.
	// Used in Unclaimed and Worker modes. Empty defaults to [NodeName].
	AdvertiseDNSNames []string

	// AdvertiseIPs lists IP SANs the worker requests in its CSR. Empty
	// defaults to [127.0.0.1, ::1] — bind-host-independent so the
	// coordinator can always reach the node over loopback during a
	// co-located test, and real IPs layer on top via config.
	AdvertiseIPs []net.IP

	// PairingPath is where the Unclaimed node writes its pairing code
	// (0600) during an active pairing window. Required iff Mode is
	// Unclaimed. The code itself is NEVER logged or printed to stdout;
	// only the path is.
	PairingPath string

	// RequireSecurePairing, when true, makes the coordinator reject
	// worker pair requests that did NOT pin the coord's CA fingerprint
	// on the bootstrap handshake. Zero-value (false) is permissive —
	// home-lab-friendly — so tests and install defaults keep working.
	// Set by the server-side config mapper when
	// cluster.accept_insecure_pairing=false in node.yaml.
	// Coordinator-only; ignored on Worker / Unclaimed.
	RequireSecurePairing bool

	// OnModeChange fires on every runtime mode transition the Node
	// performs internally — Unclaimed→Worker on successful pairing,
	// Worker→Unclaimed on coord-driven decommission or cert expiry.
	// Called synchronously from the transition goroutine right after
	// the atomic mode.Store. Callbacks must not block or take the
	// Node's internal locks — a reasonable implementation records the
	// new state and returns, doing any heavy work on its own
	// goroutine. nil = no-op.
	//
	// Does NOT fire for the initial Mode set during New — that's
	// already observable via Node.Mode() at construction.
	OnModeChange func(to Mode, reason string)
}

// validate returns an error if required fields are missing. Keeps the
// New error surface explicit.
func (c Config) validate() error {
	if !c.Mode.Valid() {
		return fmt.Errorf("%w: mode: %d", ErrInvalidConfig, int32(c.Mode))
	}
	if c.IdentityDir == "" && c.Mode != Disabled {
		return fmt.Errorf("%w: IdentityDir required for mode %s", ErrInvalidConfig, c.Mode)
	}
	if c.Mode == Coordinator && c.CADir == "" {
		return fmt.Errorf("%w: CADir required for coordinator mode", ErrInvalidConfig)
	}
	if c.Mode == Worker {
		if c.ClusterDir == "" {
			return fmt.Errorf("%w: ClusterDir required for worker mode", ErrInvalidConfig)
		}
		if c.CoordinatorURL == "" {
			return fmt.Errorf("%w: CoordinatorURL required for worker mode", ErrInvalidConfig)
		}
	}
	if c.Mode == Unclaimed || c.Mode == Worker || c.Mode == Coordinator {
		if c.NodeName == "" {
			return fmt.Errorf("%w: NodeName required for mode %s", ErrInvalidConfig, c.Mode)
		}
	}
	if c.Mode == Unclaimed && c.PairingPath == "" {
		return fmt.Errorf("%w: PairingPath required for unclaimed mode", ErrInvalidConfig)
	}
	return nil
}

// advertiseDNSNames returns the DNS SANs to embed in the node's CSR,
// applying the default of [NodeName] when AdvertiseDNSNames is empty.
func (c Config) advertiseDNSNames() []string {
	if len(c.AdvertiseDNSNames) > 0 {
		return c.AdvertiseDNSNames
	}
	if c.NodeName != "" {
		return []string{c.NodeName}
	}
	return nil
}

// advertiseIPs returns the IP SANs to embed in the node's CSR.
// When AdvertiseIPs is set, it wins (operator-configured override).
// Otherwise we layer routable interface IPs on top of loopback so the
// cert is dialable from peers without operator coordination — the
// previous loopback-only default was a footgun on multi-network
// workers (e.g. Tailscale): coord dials the worker by its routable IP
// and Go's TLS check rejects with "bad certificate" because that IP
// isn't in SAN. CA.Sign's rejectUnsafeIPs filter still applies, so a
// pathological enumeration (multicast, unspecified) is caught at the
// signing boundary.
func (c Config) advertiseIPs() []net.IP {
	if len(c.AdvertiseIPs) > 0 {
		return c.AdvertiseIPs
	}
	ips := detectRoutableIPs(c.BindHost)
	return append(ips, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
}

// Node is the cluster-side state machine. One instance per process.
// Fields are unexported; the API is through the methods below.
//
// Node is single-use: Start may be called at most once per instance, and
// Stop terminates the instance permanently. Restarting after Stop is
// not supported — callers who need a restartable node construct a fresh
// one via New.
type Node struct {
	cfg      Config
	identity *clusterid.Identity
	ca       *clusterid.CA // nil on non-coordinator

	// adminAPI serves /zzrouter/v1/internal/* on the cluster mTLS port —
	// admin orchestration (models, runs, deployments, providers, params).
	// Required on Worker + Unclaimed (the latter rebinds with this handler
	// on pairing completion).
	adminAPI http.Handler

	// workerCompat serves the OpenAI/Ollama compat surface
	// (/v1/*, /api/*, /mcp + dynamic NativeWire mounts) on the cluster
	// mTLS port in Worker mode. Coord-proxied inference traffic flows
	// through here over mTLS — workers never expose compat externally.
	// Nil on Coordinator (the coord serves compat on its admin port).
	workerCompat http.Handler

	// Coordinator-only state. deny is non-nil on Coordinator nodes
	// and backs the /cluster/renew deny-list check and the Revoke API.
	deny *denyList

	// pairingStore is non-nil on Coordinator nodes and backs the
	// worker-initiated pairing flow (POST /cluster/pairing-request
	// plus the admin /cluster/pairing/accept). nil elsewhere; the
	// route-registration path only mounts the handlers on coord.
	pairingStore *PairingStore

	// pairingRateLimiter guards /cluster/pairing-request from floods.
	// Paired with pairingStore — created and torn down together.
	pairingRateLimiter *pairingRateLimiter

	// Worker-side pairing state. activePairingWindow holds the running
	// pairing attempt started by BeginPairing; nil when no window is
	// active. pairingMu serializes Begin/Cancel and the async loop's
	// teardown so two operators typing `cluster pair` at once don't
	// race for the on-disk pairing.txt.
	pairingMu           sync.Mutex
	activePairingWindow *pairingWindow
	// lastPairingError holds the most recent terminal error returned
	// by runPairingLoop — primarily strict-mode 403 rejections, but
	// also approve-side failures. Consumed once via
	// ConsumeLastPairingError so the admin handler can surface the
	// root cause to the operator on their next poll. Guarded by
	// pairingMu.
	lastPairingError error

	// Worker-only renewal state. renewalCancel terminates the loop
	// on Stop or revert-to-unclaimed. renewalClock is the test hook
	// for the ticker — production uses the real clock. selfHealStarted
	// gates a single in-flight revert; reset on a fresh pairing cycle.
	// Protected by n.mu.
	renewalCancel   context.CancelFunc
	renewalClock    renewalClock
	selfHealStarted bool

	// mode is read on every request via ClusterModeGate — atomic so
	// reads are lock-free. Writes happen only in completePairing
	// (Unclaimed→Worker) and revertToUnclaimed (Worker→Unclaimed).
	mode atomic.Int32

	// listenFn is the injectable version of net.Listen used by
	// completePairing. Production nodes set it to net.Listen in New;
	// tests override to simulate bind failures.
	listenFn func(network, addr string) (net.Listener, error)

	// shutdownCtx is captured from the ctx passed to Start so any
	// late-starting background work (e.g. the renewal ticker spawned
	// by completePairing after pairing success) can bind its lifetime
	// to the node's overall Stop path. Covered by n.mu.
	shutdownCtx context.Context

	// Lifecycle state. start/stop are serialized under mu; listener
	// swap during completePairing also takes mu. httpServer is set to
	// nil once shutdownServer succeeds so a concurrent second caller
	// short-circuits rather than relying on http.Server.Shutdown's
	// documented re-entrancy.
	mu           sync.Mutex
	started      bool
	stopped      bool
	shutdownFn   context.CancelFunc
	httpServer   *http.Server
	listener     net.Listener
	listenerAddr net.Addr
	wg           sync.WaitGroup

	// servingDone closes when the current serve goroutine returns (HTTP
	// listener torn down). Tests wait on this to observe ctx-triggered
	// shutdown without polling the listener. Nil on Disabled/Unclaimed
	// nodes that never open a listener; set to a fresh channel by every
	// caller that spawns serve (Start on Coordinator/Worker,
	// completePairing on Unclaimed→Worker) so a re-pair after revert
	// gets a new channel to close.
	servingDone chan struct{}
}

// New constructs a Node from a Config. It loads the identity keypair
// (and generates a self-signed cert if the cert file is missing) and —
// for coordinators — loads or creates the CA. No goroutines are started;
// call Start(ctx) to open the listener.
//
// For Worker configs, the caller MUST install the admin-API handler
// via SetAdminAPIHandler AND the worker-compat handler via
// SetWorkerCompatHandler before Start — workers serve /internal/* +
// /v1/* + /api/* over mTLS and a nil handler would silently 404. For
// Unclaimed configs both handlers must also be installed because a
// successful pairing completion rebinds the listener with the same
// references (see completePairing).
func New(cfg Config) (*Node, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	n := &Node{
		cfg:      cfg,
		listenFn: net.Listen,
	}
	n.mode.Store(int32(cfg.Mode))

	if cfg.Mode != Disabled {
		id, err := clusterid.LoadOrCreateIdentity(cfg.IdentityDir)
		if err != nil {
			return nil, fmt.Errorf("load identity: %w", err)
		}
		n.identity = id
	}
	if cfg.Mode == Coordinator {
		ca, err := clusterid.LoadOrCreateCA(cfg.CADir)
		if err != nil {
			return nil, fmt.Errorf("load CA: %w", err)
		}
		n.ca = ca
		// Ensure our own identity cert is CA-signed; workers that
		// later dial this coordinator over mTLS trust the CA but not
		// a self-signed cert. Idempotent across boots — only signs
		// if the on-disk cert isn't already issued by our CA.
		if err := n.ensureCoordinatorIdentitySigned(); err != nil {
			return nil, fmt.Errorf("sign coordinator identity: %w", err)
		}
		deny, err := loadDenyList(denyListPath(cfg.CADir))
		if err != nil {
			return nil, fmt.Errorf("load deny list: %w", err)
		}
		n.deny = deny
		n.pairingStore = NewPairingStore(defaultPairingTTL)
		n.pairingRateLimiter = newPairingRateLimiter()
	}
	// Renewal clock is wired for any mode that can reach Worker at
	// runtime: Worker directly on a warm restart, Unclaimed via
	// completePairing after a successful pairing window. Coordinators
	// and Disabled nodes never run the renewal ticker, but wiring the
	// clock for them is a harmless default.
	if cfg.Mode == Worker || cfg.Mode == Unclaimed {
		n.renewalClock = realClock{}
	}
	return n, nil
}

// CoordinatorURL returns the paired coordinator URL for Worker mode,
// or "" for other modes. Exposed so the worker-side notify path can
// target the coord directly — the Endpoints list only carries
// coord→worker URLs, not the other direction.
func (n *Node) CoordinatorURL() string {
	return n.cfg.CoordinatorURL
}

// SetAdminAPIHandler installs the handler that serves /zzrouter/v1/internal/*
// (models, runs, deployments, providers — admin orchestration) on this
// node's cluster mTLS listener. Must be called before Start.
//
// Required for Unclaimed and Worker configs; optional on Coordinator and
// Disabled. Calling after Start returns ErrAlreadyStarted; passing a nil
// handler returns ErrAdminAPIHandlerMissing.
func (n *Node) SetAdminAPIHandler(h http.Handler) error {
	if h == nil {
		return ErrAdminAPIHandlerMissing
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return ErrAlreadyStarted
	}
	n.adminAPI = h
	return nil
}

// SetWorkerCompatHandler installs the handler that serves the OpenAI
// (/v1/*) and Ollama (/api/*) compat surface, plus any NativeWire
// mounts, on the cluster mTLS listener in Worker mode. Coord-proxied
// inference traffic flows through here. Must be called before Start.
//
// Required for Unclaimed and Worker configs; nil on Coordinator (the
// coord serves compat on its admin port for external clients).
func (n *Node) SetWorkerCompatHandler(h http.Handler) error {
	if h == nil {
		return ErrWorkerCompatHandlerMissing
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return ErrAlreadyStarted
	}
	n.workerCompat = h
	return nil
}

// ensureCoordinatorIdentitySigned re-signs the coordinator's own
// identity cert with the CA if it isn't already. Idempotent — a second
// boot observes the already-CA-signed cert on disk and skips the work.
// Without this, the coordinator's cluster listener would present a
// self-signed cert that workers couldn't mTLS-verify via the CA they
// trust.
func (n *Node) ensureCoordinatorIdentitySigned() error {
	if n.identity == nil || n.ca == nil {
		return nil
	}
	cert := n.identity.Certificate()
	// Cert is CA-signed when its Issuer matches the CA's Subject.
	if cert.Issuer.String() == n.ca.Certificate().Subject.String() {
		return nil
	}
	csrPEM, err := n.identity.CSR(n.cfg.NodeName, n.cfg.advertiseDNSNames(), n.cfg.advertiseIPs())
	if err != nil {
		return fmt.Errorf("coordinator CSR: %w", err)
	}
	signedPEM, err := n.ca.Sign(csrPEM, clusterid.RoleCoordinator)
	if err != nil {
		return fmt.Errorf("coordinator sign: %w", err)
	}
	return n.identity.InstallSignedCert(signedPEM)
}

// Start binds the cluster-port listener and begins serving in a
// background goroutine. It returns after the listener is bound so the
// caller can read Addr immediately. ctx controls the node's lifetime —
// cancelling it triggers a graceful shutdown equivalent to Stop().
//
// Listener binding by mode:
//
//   - Disabled:    no-op, no listener runs.
//   - Coordinator: binds the cluster listener with mTLS; serves /renew,
//     /leave (deny-list check), /internal/*, plus the
//     worker-initiated pairing routes (/pairing-request,
//     /ca-fingerprint).
//   - Worker:      binds the cluster listener with mTLS; starts the
//     renewal ticker.
//   - Unclaimed:   binds NO cluster listener. The node is dormant on
//     the cluster network until an operator runs
//     `zzrouter cluster pair`; completePairing rebinds on
//     success.
//
// Unclaimed requires the inference handler be pre-wired because
// completePairing reuses n.adminAPI + n.workerCompat on listener rebuild — a nil
// handler would silently 404 post-pairing.
func (n *Node) Start(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	// Single-use contract: once started (still running or already
	// stopped), refuse. Construct a fresh Node to restart.
	if n.started {
		return ErrAlreadyStarted
	}
	n.started = true
	shutdownCtx, shutdownFn := context.WithCancel(ctx)
	n.shutdownFn = shutdownFn
	n.shutdownCtx = shutdownCtx

	if n.Mode() == Disabled {
		return nil
	}

	// Worker-track modes (Unclaimed, Worker) must have BOTH the admin-API
	// handler AND the worker-compat handler wired. Unclaimed counts because
	// a successful pairing flips mode to Worker at runtime via completePairing,
	// which rebuilds the listener with the same handler references. Fail-
	// closed at Start so a deployment error doesn't hide behind a silent
	// 404 post-pairing.
	if (n.Mode() == Worker || n.Mode() == Unclaimed) && (n.adminAPI == nil || n.workerCompat == nil) {
		n.started = false
		shutdownFn()
		return fmt.Errorf("%w: mode %s", ErrAdminAPIHandlerMissing, n.Mode())
	}

	// Unclaimed is dormant on the cluster network. Admin API is
	// served elsewhere in the process; that's what `zzrouter cluster
	// pair` reaches through. The ctx-watcher is still installed so
	// Stop semantics are uniform across modes.
	if n.Mode() == Unclaimed {
		n.wg.Add(1)
		go n.watchCtx(shutdownCtx) //nolint:gosec // shutdownCtx is the node-lifecycle ctx, not request-scoped
		return nil
	}

	addr := net.JoinHostPort(n.cfg.BindHost, strconv.Itoa(n.cfg.Port))
	ln, err := n.listenFn("tcp", addr)
	if err != nil {
		n.started = false
		shutdownFn()
		return fmt.Errorf("%w: listen %s: %w", ErrListenerFailed, addr, err)
	}
	n.listener = ln
	n.listenerAddr = ln.Addr()

	n.httpServer = &http.Server{
		Handler:           n.buildHandler(),
		TLSConfig:         n.buildTLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	n.servingDone = make(chan struct{})
	servingDone := n.servingDone

	n.wg.Add(2)
	go n.serve(n.httpServer, n.listener, servingDone)
	go n.watchCtx(shutdownCtx) //nolint:gosec // shutdownCtx is the node-lifecycle ctx, not request-scoped

	// Worker-mode side-effect: start the renewal ticker. Tracked in
	// wg inside startRenewalTickerLocked. Must run AFTER serve is
	// spawned so a renewal arriving immediately after start has a
	// listener to reach through.
	if n.Mode() == Worker {
		n.startRenewalTickerLocked(shutdownCtx)
	}

	return nil
}

// watchCtx translates ctx cancellation into a graceful shutdown. Tracked
// by wg so Stop waits for it to exit before returning — without this,
// Stop could return while this goroutine is still running shutdownServer
// and observers would see the node as "stopped" with a live goroutine.
func (n *Node) watchCtx(ctx context.Context) {
	defer n.wg.Done()
	<-ctx.Done()
	_ = n.shutdownServer(context.Background())
}

// Addr returns the listener's bound address. Zero value until Start
// succeeds; nil for Disabled nodes.
func (n *Node) Addr() net.Addr {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.listenerAddr
}

// Stop drains in-flight requests, closes the listener, and returns
// when every goroutine spawned by Start has exited. The caller's ctx
// bounds the HTTP server drain deadline: a short ctx forces an early
// listener close, a cancelled ctx makes Shutdown return immediately
// with context.Canceled (swallowed — the caller already knows). Safe
// to call multiple times; calls after the first are no-ops. Safe on a
// Disabled node that never started a listener.
//
// Passing context.Background retains the internal 5s drain cap;
// shorter deadlines win (Shutdown honors the earlier of the ctx
// deadline and its own 5s cap).
func (n *Node) Stop(ctx context.Context) error {
	n.mu.Lock()
	if !n.started || n.stopped {
		n.mu.Unlock()
		return nil
	}
	n.stopped = true
	n.mu.Unlock()

	if err := n.shutdownServer(ctx); err != nil {
		return err
	}
	n.shutdownFn()
	n.wg.Wait()

	// Release coord-side pairing state: cancels any parked long-poll
	// waiters with ErrPairingStoreStopped and joins the GC goroutine.
	if n.pairingStore != nil {
		n.pairingStore.Stop()
	}
	// Release worker-side pairing state: cancel + JOIN. Capture done
	// under pairingMu (cancelWindowLocked niles the window), wait
	// outside the lock — the goroutine takes pairingMu on exit.
	var pairingDone chan struct{}
	n.pairingMu.Lock()
	if n.activePairingWindow != nil {
		pairingDone = n.activePairingWindow.done
		n.cancelWindowLocked()
	}
	n.pairingMu.Unlock()
	if pairingDone != nil {
		<-pairingDone
	}
	return nil
}

// shutdownServer gracefully closes the HTTP server and its listener.
// Idempotent: concurrent callers race on the httpServer claim; the
// winner runs Shutdown and niles the field, the loser sees nil and
// returns immediately. This does not rely on http.Server.Shutdown being
// safe to call twice — we just never call it twice.
func (n *Node) shutdownServer(ctx context.Context) error {
	n.mu.Lock()
	srv := n.httpServer
	n.httpServer = nil
	n.mu.Unlock()
	if srv == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("server shutdown: %w", err)
	}
	return nil
}

// serve runs the HTTP server. Takes the server, listener, AND
// servingDone as explicit parameters rather than reading from n.*
// because shutdownServer niles n.httpServer on first shutdown and
// completePairing rebinds n.servingDone on Unclaimed→Worker — both
// would race a concurrently-starting serve goroutine that re-read
// the fields at defer-execution time. Capturing at call-entry
// guarantees this invocation always signals the channel the caller
// created for it. Returns when Shutdown is called or the listener
// is closed; http.ErrServerClosed is the expected termination.
func (n *Node) serve(srv *http.Server, ln net.Listener, servingDone chan<- struct{}) {
	defer n.wg.Done()
	defer close(servingDone)
	err := srv.ServeTLS(ln, "", "") // certs come from TLSConfig.GetCertificate
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		// TODO(slog): route through operator logger once pkg/clusternode
		// threads a *slog.Logger through Config. Until then any hard
		// Serve error (listener externally closed, TLS config load
		// failure at ServeTLS setup) is silently dropped because
		// shutdownServer is the only expected terminator and observes
		// ErrServerClosed.
		_ = err
	}
}

// Mode returns the current cluster mode. Lock-free.
func (n *Node) Mode() Mode { return Mode(n.mode.Load()) }

// notifyModeChange invokes the operator-supplied OnModeChange callback
// if one is configured. Designed to be called OUTSIDE n.mu so the
// callback can take its own locks (e.g. role.Manager.Set serializes on
// its own mutex). Safe to call with a nil callback — no-op.
func (n *Node) notifyModeChange(to Mode, reason string) {
	if n.cfg.OnModeChange != nil {
		n.cfg.OnModeChange(to, reason)
	}
}

// Fingerprint returns the node's canonical SPKI fingerprint. Empty on
// a Disabled node that has no identity.
func (n *Node) Fingerprint() string {
	if n.identity == nil {
		return ""
	}
	return n.identity.Fingerprint()
}

// ShortForm returns the human-comparable truncation of Fingerprint.
func (n *Node) ShortForm() string {
	if n.identity == nil {
		return ""
	}
	return n.identity.ShortForm()
}

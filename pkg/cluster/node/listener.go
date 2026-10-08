package clusternode

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

// buildTLSConfig returns the *tls.Config for the cluster-port
// listener. Only called for Coordinator and Worker — Unclaimed binds
// no cluster listener (see Node.Start). ClientAuth is mode-dependent
// via clientAuthForMode: Coordinator accepts clientless handshakes
// (so unpaired workers can reach the pairing endpoints), Worker
// requires client certs.
//
// GetCertificate reads the identity cert lazily on every handshake so
// a renewal that atomically rewrites node.pem is picked up without a
// listener rebind. Identity serializes access internally.
//
// ClientCAs:
//   - Coordinator: trusts its own CA (worker client certs chain to it).
//   - Worker: trusts the CA it stored on pairing completion
//     (coordinator client certs chain to it).
func (n *Node) buildTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert := n.identity.Certificate()
			// Present [leaf, CA] where possible so pin-pinned clients
			// (worker pairing client) can locate the CA in the
			// presented chain and run stdlib x509.Verify against a
			// synthesized single-root pool. Worker-mode callers don't
			// have the CA in-memory (only on disk) and their peer
			// already trusts the CA via the client-side ClientCAs
			// pool, so they ship leaf-only.
			chain := [][]byte{cert.Raw}
			if n.ca != nil {
				chain = append(chain, n.ca.Certificate().Raw)
			}
			return &tls.Certificate{
				Certificate: chain,
				PrivateKey:  n.identity.Signer(),
				Leaf:        cert,
			}, nil
		},
		ClientAuth: n.clientAuthForMode(),
		ClientCAs:  n.clientCAPool(),
	}
}

// clientCAPool returns the CA pool used to validate client certificates.
// Nil for Unclaimed / Disabled (ClientAuth is NoClientCert there, and a
// nil pool with that auth mode is fine). For Coordinator, trusts the
// process-local CA. For Worker, loads the trusted CA from ClusterDir.
func (n *Node) clientCAPool() *x509.CertPool {
	switch n.Mode() {
	case Coordinator:
		if n.ca == nil {
			return nil
		}
		pool := x509.NewCertPool()
		pool.AddCert(n.ca.Certificate())
		return pool
	case Worker:
		caPEM, err := os.ReadFile(filepath.Join(n.cfg.ClusterDir, "ca.pem"))
		if err != nil {
			return nil
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil
		}
		return pool
	default:
		return nil
	}
}

// clientAuthForMode returns the ClientAuth to bake into the tls.Config
// for the current mode. This value is read ONCE at listener build time
// and captured by the running *http.Server — Go's TLS stack does not
// consult ClientAuth per handshake.
//
//   - Coordinator → VerifyClientCertIfGiven. The pairing endpoints
//     (/cluster/pairing-request, /cluster/ca-fingerprint) MUST accept
//     clientless handshakes because unpaired workers have no client
//     cert to present yet. Every protected handler behind this
//     listener (/cluster/renew, /internal/*) carries mTLSOUCheck or
//     wrapInternal, both of which reject len(VerifiedChains)==0 with
//     401 — those middlewares enforce the mTLS requirement at the
//     app layer, not the transport layer.
//
//   - Worker → RequireAndVerifyClientCert. Workers only accept
//     coordinator connections, and the coordinator always has a
//     client cert to present. No unauthenticated entry points.
//
//   - Unclaimed / Disabled → never reach here (Start returns early
//     for Unclaimed, no listener for Disabled).
//
// If you add a new route to the coordinator's cluster listener, check
// it: if the route requires authentication, it MUST apply
// mTLSOUCheck (or equivalent wrapInternal-style gate). If the route
// is intentionally unauthenticated (like the pairing endpoints), it
// must pair with a rate limiter.
func (n *Node) clientAuthForMode() tls.ClientAuthType {
	switch n.Mode() {
	case Coordinator:
		return tls.VerifyClientCertIfGiven
	default:
		return tls.RequireAndVerifyClientCert
	}
}

// buildHandler composes the top-level handler for the cluster listener.
// Returns a http.ServeMux that splits traffic between sub-handlers:
//
//   - /zzrouter/v1/internal/ → caller-supplied admin-API handler
//     (models, runs, deployments, providers — admin orchestration).
//     Registered in Worker mode (coord→worker dispatch, OU=coordinator)
//     and Coordinator mode (worker→coord notify, OU=worker).
//   - /v1/, /api/, /mcp        → caller-supplied worker-compat handler
//     (Worker mode only). Coord-proxied inference flows here over mTLS;
//     workers never expose this surface on the admin port.
//   - everything else          → the gin engine built by buildClusterEngine
//     (serves /health and /zzrouter/v1/cluster/* per-mode routes).
//
// The split keeps clusternode ignorant of the engines' middleware stacks
// — operator concerns (RequestID, logger, etc.) are owned by the caller.
// clusternode only contributes the transport-level mTLS + OU boundary.
//
// Called fresh on every listener rebind (pairing completion, revert to
// unclaimed), so mode-conditional registrations pick up the current
// n.Mode() automatically.
func (n *Node) buildHandler() http.Handler {
	mux := http.NewServeMux()
	clusterEngine := n.buildClusterEngine()
	if n.adminAPI != nil {
		switch n.Mode() {
		case Worker:
			mux.Handle("/zzrouter/v1/internal/", n.wrapInternal(n.adminAPI, clusterid.RoleCoordinator.OU()))
		case Coordinator:
			mux.Handle("/zzrouter/v1/internal/", n.wrapInternal(n.adminAPI, clusterid.RoleWorker.OU()))
		}
	}
	if n.workerCompat != nil && n.Mode() == Worker {
		// Compat surface: gin handles its own internal routing, so we
		// hand the whole path tree to it via two registrations per
		// prefix (with and without trailing slash — ServeMux treats
		// "/v1" and "/v1/" as separate registrations and both must
		// route to the compat engine).
		compat := n.wrapInternal(n.workerCompat, clusterid.RoleCoordinator.OU())
		for _, p := range workerCompatPathPrefixes {
			mux.Handle(p, compat)
			mux.Handle(p+"/", compat)
		}
	}
	// Catch-all: /health, /zzrouter/v1/cluster/*, and any other path
	// land on the gin engine (which returns 404 for unknowns).
	mux.Handle("/", clusterEngine)
	return mux
}

// workerCompatPathPrefixes is the static prefix list of compat-class
// surfaces served on the worker's cluster mTLS port. Caller (the gin
// compat engine) decides whether a given full path under one of these
// prefixes is registered; ServeMux just routes the prefix here.
//
// Static rather than config-driven because the listener is rebuilt only
// on pairing events, not on config mutations — a runtime-added
// NativeWire mount would not become reachable on the cluster port until
// next rebind. NativeWire today reads config at startup only, so static
// prefixes match reality.
var workerCompatPathPrefixes = []string{"/v1", "/api", "/mcp", "/metrics"}

// buildClusterEngine constructs the gin engine for the non-inference
// cluster-port routes — health plus the per-mode /cluster/* groups.
// Handlers live in pairing_handlers.go (coord pairing surface) and
// lifecycle.go (renew/leave); this function only wires the router.
//
// gin.New (not gin.Default) because Default's Logger middleware writes
// to stderr, which pollutes test output; observability will be layered
// in via middleware in a later PR.
func (n *Node) buildClusterEngine() *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())

	engine.GET("/health", n.handleHealth)
	engine.GET("/health/ready", n.handleHealth)

	cluster := engine.Group("/zzrouter/v1/cluster")
	n.registerCoordinatorRoutes(cluster)
	n.registerWorkerRoutes(cluster)

	return engine
}

// wrapInternal applies the same mTLS + OU + deny-list gate that
// mTLSOUCheck enforces on /cluster/renew and /cluster/leave, but at
// the http.Handler layer so the inference engine can be a plain
// http.Handler with no knowledge of the cluster trust model.
//
// requiredOU is direction-dependent: coord→worker dispatch requires
// OU=coordinator on the peer cert; worker→coord notify requires
// OU=worker. Picked by buildHandler from the current mode.
//
// Writes a minimal JSON error body on rejection. No logger wired yet —
// the request was either truly unauthorized (flood-worthy detail) or a
// misconfigured peer (caller-side debuggable). Matches the existing
// gin-based mTLSOUCheck's quiet posture.
func (n *Node) wrapInternal(h http.Handler, requiredOU string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			writeJSONError(w, http.StatusUnauthorized, "client cert required")
			return
		}
		peer := r.TLS.PeerCertificates[0]
		if !hasOU(peer, requiredOU) {
			writeJSONError(w, http.StatusForbidden, "client cert OU mismatch")
			return
		}
		if n.deny != nil && n.deny.Contains(clusterid.Fingerprint(peer)) {
			writeJSONErrorRevoked(w)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// writeJSONError writes a tiny {"error": msg} body. Kept minimal so the
// handler has no gin dependency — mTLSOUCheck does the gin equivalent.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}

// writeJSONErrorRevoked mirrors the gin handler's revoked-client body,
// including the structured "revoked":true marker that worker-side
// renewOnce branches on.
func writeJSONErrorRevoked(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"client cert revoked","revoked":true}`))
}

// registerCoordinatorRoutes wires the coordinator-only routes:
//
//   - /cluster/renew           — worker submits CSR for re-signing (mTLS + OU=worker + deny list).
//   - /cluster/pairing-request — unpaired worker's long-poll entry point. Unauth at the
//     app layer (CA-pinned TLS on the worker's side is the trust root); per-IP + global rate-limited.
//   - /cluster/ca-fingerprint  — returns SPKI hash of the coord's CA cert. Non-secret.
//
// The pairing-flow routes are split into their own group so the mTLS
// middleware only attaches to the renew route — an unpaired worker has
// no client cert to present.
func (n *Node) registerCoordinatorRoutes(cluster *gin.RouterGroup) {
	if n.Mode() != Coordinator {
		return
	}

	public := cluster.Group("")
	public.Use(ClusterModeGate(n, Coordinator))
	if n.pairingStore != nil && n.pairingRateLimiter != nil {
		public.POST("/pairing-request",
			pairingRateLimitMiddleware(n.pairingRateLimiter),
			n.handlePairingRequest)
		// Rate-limit the fingerprint endpoint too. It's cheap to
		// serve but unauthenticated and scanner-fingerprintable —
		// no reason to hand an attacker a free, unthrottled
		// enumeration primitive.
		public.GET("/ca-fingerprint",
			pairingRateLimitMiddleware(n.pairingRateLimiter),
			n.handleCAFingerprint)
	} else {
		// Test/no-limiter path: mount unthrottled so unit tests
		// that don't stand up a limiter still see the endpoint.
		public.GET("/ca-fingerprint", n.handleCAFingerprint)
	}

	mtls := cluster.Group("")
	mtls.Use(ClusterModeGate(n, Coordinator))
	mtls.Use(n.mTLSOUCheck(clusterid.RoleWorker.OU()))
	mtls.POST("/renew", n.handleRenew)
}

// registerWorkerRoutes wires the worker-only routes: /cluster/leave
// (coordinator decommissions this worker). Behind mTLS with
// OU=zzrouter-coordinator.
func (n *Node) registerWorkerRoutes(cluster *gin.RouterGroup) {
	if n.Mode() != Worker {
		return
	}
	worker := cluster.Group("")
	worker.Use(ClusterModeGate(n, Worker))
	worker.Use(n.mTLSOUCheck(clusterid.RoleCoordinator.OU()))
	worker.POST("/leave", n.handleLeave)
}

// handleHealth serves a minimal liveness response. No fingerprint, no
// version — that's an operator-reconnaissance concern discussed in the
// plan. `mode` is included because it's what ops needs to verify the
// node is in the expected role.
func (n *Node) handleHealth(c *gin.Context) {
	c.JSON(200, gin.H{
		"status": "ok",
		"mode":   n.Mode().String(),
	})
}

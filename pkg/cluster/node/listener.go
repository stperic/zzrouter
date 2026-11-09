package clusternode

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

// buildTLSConfig authenticates coordinator clients and picks up renewed identities per handshake.
func (n *Node) buildTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || !hasOU(cs.PeerCertificates[0], clusterid.RoleCoordinator.OU()) {
				return errors.New("only coordinator-initiated cluster connections are accepted")
			}
			return nil
		},
		GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert := n.identity.Certificate()
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

// clientAuthForMode requires mTLS; the explicitly armed enrollment listener overrides it.
func (n *Node) clientAuthForMode() tls.ClientAuthType { return tls.RequireAndVerifyClientCert }

// buildHandler composes the top-level handler for the cluster listener.
// Returns a http.ServeMux that splits traffic between sub-handlers:
//
//   - /zzrouter/v1/internal/ → caller-supplied admin-API handler
//     (models, runs, deployments, providers — admin orchestration).
//     Registered in Worker mode for coordinator dispatches.
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
	n.registerWorkerRoutes(cluster)

	return engine
}

// wrapInternal enforces the cluster trust boundary before delegating to worker engines.
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

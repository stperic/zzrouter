package clusternode

import (
	"crypto/x509"
	"net/http"

	"github.com/gin-gonic/gin"
)

// hasOU reports whether the cert's Subject Organizational Unit is
// exactly the single value want. Tighter than "contains want" —
// rejects certs with extra OU entries that a buggy or compromised CA
// might emit ("zzrouter-worker" alongside "attacker-payload" would
// otherwise pass a loose check). Used by mTLSOUCheck (gin middleware
// in lifecycle.go), wrapInternal (http.Handler middleware in
// listener.go), and DialClient's VerifyConnection (dial.go) to gate
// every mTLS-protected path in the cluster subsystem.
func hasOU(cert *x509.Certificate, want string) bool {
	ous := cert.Subject.OrganizationalUnit
	return len(ous) == 1 && ous[0] == want
}

// ClusterModeGate is the single place where "is this route allowed in
// the current cluster mode?" is answered. Applied to every route group
// that isn't universally available (like /health).
//
// Returns 501 Not Implemented rather than 404 when the mode doesn't
// match. 404 would be indistinguishable from a wrong URL and hide
// half-deployed-binary bugs — 501 with the current mode in the body
// tells the operator exactly why the call was refused.
//
// Reads Mode via atomic.Int32.Load (see Node.Mode), so this middleware
// is safe to run on every request with no contention against the
// completePairing goroutine that flips the mode.
func ClusterModeGate(n *Node, allowed ...Mode) gin.HandlerFunc {
	set := make(map[Mode]struct{}, len(allowed))
	for _, m := range allowed {
		set[m] = struct{}{}
	}
	return func(c *gin.Context) {
		m := n.Mode()
		if _, ok := set[m]; !ok {
			c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
				"error": "route not available in this cluster mode",
				"mode":  m.String(),
			})
			return
		}
		c.Next()
	}
}

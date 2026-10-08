package clusternode

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClusterUpdateCommandsRequireCoordinatorCertificate(t *testing.T) {
	for _, ou := range []string{"", "zzrouter-worker", "zzrouter-coordinator"} {
		t.Run(ou, func(t *testing.T) {
			called := false
			n := &Node{}
			h := n.wrapInternal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusAccepted) }), "zzrouter-coordinator")
			r := httptest.NewRequest(http.MethodPost, "/zzrouter/v1/internal/update/apply", nil)
			if ou != "" {
				cert := &x509.Certificate{Subject: pkix.Name{OrganizationalUnit: []string{ou}}}
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			switch ou {
			case "":
				require.Equal(t, 401, out.Code)
			case "zzrouter-worker":
				require.Equal(t, 403, out.Code)
			default:
				require.Equal(t, 202, out.Code)
			}
			require.Equal(t, ou == "zzrouter-coordinator", called)
		})
	}
}

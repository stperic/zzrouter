package clusternode

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCA is a minimal self-signed CA for client-pin tests. Not
// reusing pkg/cluster/id because those helpers bring 0600-dir +
// filesystem persistence into the mix — noisy for table tests.
type testCA struct {
	key     *ecdsa.PrivateKey
	cert    *x509.Certificate
	certPEM []byte
	certDER []byte
	spkiHex string
}

func newTestCA(t *testing.T, subject string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: subject},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return &testCA{
		key:     key,
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		certDER: der,
		spkiHex: hex.EncodeToString(sum[:]),
	}
}

// issueServerCert creates a leaf cert signed by the CA, suitable for
// the httptest.Server's TLS config.
func (ca *testCA) issueServerCert(t *testing.T, host string) tls.Certificate {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, leafKey.Public(), ca.key)
	require.NoError(t, err)
	return tls.Certificate{
		Certificate: [][]byte{leafDER, ca.certDER},
		PrivateKey:  leafKey,
	}
}

func TestNewCAPinnedClient_RejectsMalformedPin(t *testing.T) {
	cases := []string{
		"",
		"not-hex",
		"abc",                   // too short
		strings.Repeat("z", 64), // hex-decode fails
	}
	for _, pin := range cases {
		_, err := newCAPinnedClient(pin)
		assert.ErrorIs(t, err, ErrPairingCAFingerprintMalformed, "input %q", pin)
	}
}

func TestNewCAPinnedClient_AcceptsCanonicalAndPrefixed(t *testing.T) {
	ca := newTestCA(t, "test-ca")
	for _, pin := range []string{ca.spkiHex, "sha256:" + ca.spkiHex, strings.ToUpper(ca.spkiHex)} {
		_, err := newCAPinnedClient(pin)
		assert.NoError(t, err, "input %q", pin)
	}
}

// testTLSServer starts an httptest.Server serving TLS with a cert
// issued by ca. Handler is the provided http.HandlerFunc.
func testTLSServer(t *testing.T, ca *testCA, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issueServerCert(t, "127.0.0.1")}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestCAPinnedClient_AcceptsRightCA(t *testing.T) {
	ca := newTestCA(t, "right-ca")
	srv := testTLSServer(t, ca, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	client, err := newCAPinnedClient(ca.spkiHex)
	require.NoError(t, err)

	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestCAPinnedClient_RejectsWrongCA(t *testing.T) {
	caReal := newTestCA(t, "real-ca")
	caAttacker := newTestCA(t, "attacker-ca")
	srv := testTLSServer(t, caAttacker, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	client, err := newCAPinnedClient(caReal.spkiHex)
	require.NoError(t, err)

	_, err = client.Get(srv.URL) //nolint:bodyclose // TLS failure returns nil resp
	require.Error(t, err, "expected TLS failure when server uses a non-pinned CA")
	// The underlying error wraps ErrPairingBadCA from the
	// VerifyPeerCertificate callback. Go's TLS stack propagates the
	// callback err up the client's Get error chain.
	assert.ErrorIs(t, err, ErrPairingBadCA)
}

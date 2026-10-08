package clusternode

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// fakeCoord runs an httptest TLS server with a pairing-request handler
// that cycles through the provided response sequence. Each incoming
// POST returns the next response in responses; when exhausted the
// server returns the last one repeatedly.
type fakeCoord struct {
	srv       *httptest.Server
	url       string
	ca        *testCA
	calls     atomic.Int32
	responses []pairingPollResponse
}

func newFakeCoord(t *testing.T, ca *testCA, responses []pairingPollResponse) *fakeCoord {
	t.Helper()
	fc := &fakeCoord{ca: ca, responses: responses}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(fc.calls.Add(1) - 1)
		if idx >= len(fc.responses) {
			idx = len(fc.responses) - 1
		}
		body, _ := json.Marshal(fc.responses[idx])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issueServerCert(t, "127.0.0.1")}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	fc.srv = srv
	fc.url = srv.URL
	return fc
}

func TestRunPairingLoop_ApprovedImmediately(t *testing.T) {
	ca := newTestCA(t, "approver-ca")
	fc := newFakeCoord(t, ca, []pairingPollResponse{{
		Status:         "approved",
		SignedCertPEM:  "signed-cert-pem",
		CACertPEM:      string(ca.certPEM),
		CoordinatorURL: "https://coord:9091",
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := runPairingLoop(ctx, pairingClientOpts{
		coordURL:      fc.url,
		caFingerprint: ca.spkiHex,
		code:          "CODE1CODE1CODE1A",
		nodeName:      "worker",
		fingerprint:   "sha256:aa",
		csrPEM:        "csr",
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("signed-cert-pem"), result.signedCertPEM)
	assert.Equal(t, string(ca.certPEM), string(result.caCertPEM))
	assert.Equal(t, "https://coord:9091", result.coordinatorURL)
	assert.Equal(t, int32(1), fc.calls.Load())
}

func TestRunPairingLoop_RejectsCAMismatch(t *testing.T) {
	caReal := newTestCA(t, "real-ca")
	caOther := newTestCA(t, "other-ca")
	// Transport is pinned to caReal but the returned ca_cert_pem is
	// caOther — defense-in-depth check fires.
	fc := newFakeCoord(t, caReal, []pairingPollResponse{{
		Status:        "approved",
		SignedCertPEM: "signed",
		CACertPEM:     string(caOther.certPEM),
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := runPairingLoop(ctx, pairingClientOpts{
		coordURL:      fc.url,
		caFingerprint: caReal.spkiHex,
		code:          "CODE1CODE1CODE1B",
		nodeName:      "worker",
		fingerprint:   "sha256:aa",
		csrPEM:        "csr",
	})
	assert.ErrorIs(t, err, ErrPairingBadCA)
}

func TestRunPairingLoop_Expired(t *testing.T) {
	ca := newTestCA(t, "exp-ca")
	fc := newFakeCoord(t, ca, []pairingPollResponse{{Status: "expired"}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := runPairingLoop(ctx, pairingClientOpts{
		coordURL:      fc.url,
		caFingerprint: ca.spkiHex,
		code:          "CODE1CODE1CODE1C",
		nodeName:      "w",
		fingerprint:   "f",
		csrPEM:        "c",
	})
	assert.ErrorIs(t, err, ErrPairingWindowExpired)
}

func TestRunPairingLoop_WaitingThenApproved(t *testing.T) {
	ca := newTestCA(t, "wait-ca")
	fc := newFakeCoord(t, ca, []pairingPollResponse{
		{Status: "waiting_for_operator", PollIntervalSeconds: 0},
		{Status: "waiting_for_operator", PollIntervalSeconds: 0},
		{
			Status:        "approved",
			SignedCertPEM: "signed",
			CACertPEM:     string(ca.certPEM),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := runPairingLoop(ctx, pairingClientOpts{
		coordURL:      fc.url,
		caFingerprint: ca.spkiHex,
		code:          "CODE1CODE1CODE1D",
		nodeName:      "w",
		fingerprint:   "f",
		csrPEM:        "c",
		pollInterval:  5 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("signed"), result.signedCertPEM)
	assert.Equal(t, int32(3), fc.calls.Load(), "expected three calls total")
}

func TestRunPairingLoop_CtxCancel(t *testing.T) {
	ca := newTestCA(t, "cancel-ca")
	fc := newFakeCoord(t, ca, []pairingPollResponse{
		{Status: "waiting_for_operator"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := runPairingLoop(ctx, pairingClientOpts{
		coordURL:      fc.url,
		caFingerprint: ca.spkiHex,
		code:          "CODE1CODE1CODE1E",
		nodeName:      "w",
		fingerprint:   "f",
		csrPEM:        "c",
		pollInterval:  time.Second, // long — ensures we're parked in sleep when cancel fires
	})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRunPairingLoop_TransportErrorBackoff(t *testing.T) {
	// Point at a closed port. The loop will error repeatedly and
	// back off. Cancel the ctx quickly to bound the test.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	ca := newTestCA(t, "unreachable-ca")

	start := time.Now()
	_, err := runPairingLoop(ctx, pairingClientOpts{
		// 127.0.0.1:1 is "discard protocol"; no TCP listener there
		// on any sane box, so connect fails fast.
		coordURL:       "https://127.0.0.1:1",
		caFingerprint:  ca.spkiHex,
		code:           "CODE1CODE1CODE1F",
		nodeName:       "w",
		fingerprint:    "f",
		csrPEM:         "c",
		backoffInitial: 5 * time.Millisecond,
	})
	elapsed := time.Since(start)
	// Ctx deadline should win; we're asserting the loop does not
	// loop-without-sleep and busy-burn CPU (elapsed should cover at
	// least one backoff interval) and that it respects ctx cancel.
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"expected ctx.DeadlineExceeded, got %v", err)
	assert.True(t, elapsed >= 5*time.Millisecond, "loop exited too fast (%s)", elapsed)
}

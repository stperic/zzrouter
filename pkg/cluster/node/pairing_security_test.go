package clusternode

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- verifyCAPEMSignsLeaf --------------------------------------------------

// TestVerifyCAPEMSignsLeaf_HappyPath pins the positive case: a real
// CA PEM paired with a leaf it signed verifies cleanly.
func TestVerifyCAPEMSignsLeaf_HappyPath(t *testing.T) {
	ca := newTestCA(t, "happy-ca")
	leafCert := ca.issueServerCert(t, "127.0.0.1")
	leaf, err := x509.ParseCertificate(leafCert.Certificate[0])
	require.NoError(t, err)

	if err := verifyCAPEMSignsLeaf(ca.certPEM, leaf); err != nil {
		t.Fatalf("verify should pass for matched CA+leaf: %v", err)
	}
}

// TestVerifyCAPEMSignsLeaf_MismatchedCARejected pins the negative
// case that catches a passive on-path attacker who swaps CACertPEM:
// a CA that did not sign the observed leaf must fail verification.
func TestVerifyCAPEMSignsLeaf_MismatchedCARejected(t *testing.T) {
	realCA := newTestCA(t, "real-ca")
	attackerCA := newTestCA(t, "attacker-ca")

	leafCert := realCA.issueServerCert(t, "127.0.0.1")
	leaf, err := x509.ParseCertificate(leafCert.Certificate[0])
	require.NoError(t, err)

	err = verifyCAPEMSignsLeaf(attackerCA.certPEM, leaf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not chain")
}

// TestVerifyCAPEMSignsLeaf_NonCARejected pins the CA-sanity gate: a
// leaf-shaped cert (IsCA=false) with a signing keypair that happens
// to have signed the TLS leaf MUST NOT be accepted as a trust root.
// Without this check, an attacker delivering any cert with the
// right signature would get a non-CA persisted into clusterDir.
func TestVerifyCAPEMSignsLeaf_NonCARejected(t *testing.T) {
	// Build a "fake CA" cert with IsCA=false but otherwise usable
	// as a signing key. Then sign a leaf with it.
	fakeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	fakeTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "not-a-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  false, // <- the footgun
		BasicConstraintsValid: true,
	}
	fakeDER, err := x509.CreateCertificate(rand.Reader, fakeTmpl, fakeTmpl, fakeKey.Public(), fakeKey)
	require.NoError(t, err)
	fakeCert, err := x509.ParseCertificate(fakeDER)
	require.NoError(t, err)
	fakePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fakeDER})

	// Sign a leaf with the fake CA. Note: we use fakeCert as both
	// parent template and actual parent — mirrors the testCA
	// issuing flow.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	leafTmpl := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"127.0.0.1"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, fakeCert, leafKey.Public(), fakeKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(leafDER)
	require.NoError(t, err)

	err = verifyCAPEMSignsLeaf(fakePEM, leaf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a CA")
}

// TestVerifyCAPEMSignsLeaf_MissingCertSignRejected pins the KeyUsage
// gate: a CA-shaped cert that doesn't assert CertSign must not be
// installed as the cluster trust root — it can't legitimately sign
// future worker certs, so persisting it would break future mTLS.
func TestVerifyCAPEMSignsLeaf_MissingCertSignRejected(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	caTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "no-certsign-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature, // no CertSign
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	// Issue a leaf signed by this CA.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafSerial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	leafTmpl := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"127.0.0.1"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, leafKey.Public(), caKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(leafDER)
	require.NoError(t, err)

	err = verifyCAPEMSignsLeaf(caPEM, leaf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CertSign")
}

// TestVerifyCAPEMSignsLeaf_NilLeafRejected pins the guard against a
// missing TLS handshake (e.g. plain HTTP): without a leaf to bind
// against, TOFU has no anchor and must fail closed.
func TestVerifyCAPEMSignsLeaf_NilLeafRejected(t *testing.T) {
	ca := newTestCA(t, "noleaf-ca")
	err := verifyCAPEMSignsLeaf(ca.certPEM, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no TLS leaf")
}

// TestVerifyCAPEMSignsLeaf_BadPEMRejected pins that garbage input
// doesn't trip the chain-check (we reject early).
func TestVerifyCAPEMSignsLeaf_BadPEMRejected(t *testing.T) {
	ca := newTestCA(t, "bad-ca")
	leafCert := ca.issueServerCert(t, "127.0.0.1")
	leaf, _ := x509.ParseCertificate(leafCert.Certificate[0])

	err := verifyCAPEMSignsLeaf([]byte("not pem"), leaf)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not valid PEM")
}

// --- verifyWorkerPinnedOurCA ----------------------------------------------

// TestVerifyWorkerPinnedOurCA_Match pins the happy case.
func TestVerifyWorkerPinnedOurCA_Match(t *testing.T) {
	fp := "sha256:51aae8bdd0a31839e5389d59bdd0711f70feba0a504c86879c71d42c4d0018f7"
	if err := verifyWorkerPinnedOurCA(fp, fp); err != nil {
		t.Fatalf("matching fingerprints should verify: %v", err)
	}
}

// TestVerifyWorkerPinnedOurCA_CaseInsensitive pins normalization.
func TestVerifyWorkerPinnedOurCA_CaseInsensitive(t *testing.T) {
	lower := "sha256:51aae8bdd0a31839e5389d59bdd0711f70feba0a504c86879c71d42c4d0018f7"
	upper := strings.ToUpper("51aae8bdd0a31839e5389d59bdd0711f70feba0a504c86879c71d42c4d0018f7")
	if err := verifyWorkerPinnedOurCA(upper, lower); err != nil {
		t.Fatalf("case-folded + prefixed fingerprints should match: %v", err)
	}
}

// TestVerifyWorkerPinnedOurCA_EmptyEcho pins the strict-mode rejection
// path: a worker that didn't echo a fingerprint (TOFU) must be told.
func TestVerifyWorkerPinnedOurCA_EmptyEcho(t *testing.T) {
	err := verifyWorkerPinnedOurCA("", "sha256:deadbeef")
	require.Error(t, err)
	require.Contains(t, err.Error(), "rejects TOFU")
}

// TestVerifyWorkerPinnedOurCA_Mismatch pins that a different
// fingerprint (attacker, typo, wrong coord) is rejected.
func TestVerifyWorkerPinnedOurCA_Mismatch(t *testing.T) {
	err := verifyWorkerPinnedOurCA(
		"sha256:51aae8bdd0a31839e5389d59bdd0711f70feba0a504c86879c71d42c4d0018f7",
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match")
}

// TestVerifyWorkerPinnedOurCA_OversizedRejected pins the defensive
// length cap: arbitrary-length input doesn't burn normalize cycles.
func TestVerifyWorkerPinnedOurCA_OversizedRejected(t *testing.T) {
	err := verifyWorkerPinnedOurCA(strings.Repeat("a", 1024), "sha256:deadbeef")
	require.Error(t, err)
	require.Contains(t, err.Error(), "too long")
}

// TestVerifyWorkerPinnedOurCA_EmptyCoordFingerprint pins the
// defensive closed failure when the coord can't load its own CA —
// we refuse to pair rather than silently admit everyone.
func TestVerifyWorkerPinnedOurCA_EmptyCoordFingerprint(t *testing.T) {
	err := verifyWorkerPinnedOurCA("sha256:deadbeef", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "coordinator CA not loaded")
}

// --- FetchPairingPolicy ---------------------------------------------------

// TestFetchPairingPolicy_ReturnsPolicy pins the happy path. Uses the
// existing testTLSServer helper so the TLS handshake is real (same
// InsecureSkipVerify path production uses).
func TestFetchPairingPolicy_ReturnsPolicy(t *testing.T) {
	ca := newTestCA(t, "policy-ca")
	srv := testTLSServer(t, ca, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fingerprint":"sha256:abc","short_form":"sha256:ab…cd","require_secure_pairing":true}`))
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	policy, err := FetchPairingPolicy(ctx, srv.URL)
	require.NoError(t, err)
	require.True(t, policy.RequireSecurePairing, "must propagate require_secure_pairing=true")
	require.Equal(t, "sha256:abc", policy.Fingerprint)
}

// TestFetchPairingPolicy_UnreachableErrors pins that the helper
// returns an error (rather than a zero-value policy) when the coord
// is unreachable — so callers can fail-closed.
func TestFetchPairingPolicy_UnreachableErrors(t *testing.T) {
	// Unused port on loopback — connection refused.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := FetchPairingPolicy(ctx, "https://127.0.0.1:1")
	require.Error(t, err)
}

// TestFetchPairingPolicy_EmptyURLRejected pins the fast-fail on an
// empty URL input (a class of programming error worth catching
// rather than returning a mysterious DNS/dial failure).
func TestFetchPairingPolicy_EmptyURLRejected(t *testing.T) {
	_, err := FetchPairingPolicy(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty coordinator url")
}

// TestFetchPairingPolicy_Non200Errors pins that an unexpected
// non-200 response surfaces as an error, not a silent zero policy.
func TestFetchPairingPolicy_Non200Errors(t *testing.T) {
	ca := newTestCA(t, "policy-ca-500")
	srv := testTLSServer(t, ca, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := FetchPairingPolicy(ctx, srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "500")
}

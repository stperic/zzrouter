package clusterid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ShortForm is deterministic and elides the middle of the hex suffix. The
// test doubles as documentation of the exact truncation rule — the short
// form is what operators compare visually between two terminals.
func TestShortForm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "32-byte sha256",
			// 64 hex chars = 32 bytes, a valid SHA256.
			in: "sha256:ab12cd34ef56789a0b1c2d3e4f506172839405a6b7c8d9e0f1a2b3c4d5e6f7a8",
		},
		{
			name: "malformed prefix",
			in:   "md5:deadbeefdeadbeef",
			want: "",
		},
		{
			name: "too short",
			in:   "sha256:ab12",
			want: "",
		},
		{
			name: "non-hex suffix",
			in:   "sha256:zzzzzzzzzzzzzzzz",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ShortForm(tc.in)
			if tc.name == "32-byte sha256" {
				h := tc.in[len("sha256:"):]
				assert.Equal(t, h[:8]+"…"+h[len(h)-8:], got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// Fingerprint is stable across cert regeneration as long as the keypair
// is unchanged. This is the core identity invariant — renewals must not
// rotate the fingerprint, because it's what the coordinator pins on.
func TestFingerprintStableAcrossCertRegeneration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	id1, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	fp1 := id1.Fingerprint()
	require.NotEmpty(t, fp1)

	// Delete the cert file, force a self-sign on reload. Key is
	// preserved so the SPKI stays identical.
	require.NoError(t, os.Remove(filepath.Join(dir, "node.pem")))

	id2, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	assert.Equal(t, fp1, id2.Fingerprint(), "fingerprint must survive cert-only regeneration")
}

// A wiped identity dir produces a different fingerprint — this is the
// "wipe identity dir == new node identity" contract from the plan.
func TestFingerprintChangesOnNewKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	id1, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	fp1 := id1.Fingerprint()

	require.NoError(t, os.RemoveAll(dir))
	id2, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	assert.NotEqual(t, fp1, id2.Fingerprint())
}

func TestLoadOrCreateIdentityPersistsAcrossReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	id1, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)

	id2, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	assert.Equal(t, id1.Fingerprint(), id2.Fingerprint())
	assert.Equal(t, id1.Certificate().Raw, id2.Certificate().Raw,
		"existing cert should be reused when key matches")
}

func TestIdentityFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX perms test")
	}
	t.Parallel()
	// Use a subdir that doesn't yet exist so MkdirAll actually creates
	// it — t.TempDir() pre-creates at OS default (0755).
	dir := filepath.Join(t.TempDir(), "identity")

	_, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)

	assertMode := func(path string, want fs.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, want, info.Mode().Perm(), path)
	}
	assertMode(filepath.Join(dir, "node.key"), 0o600)
	assertMode(filepath.Join(dir, "node.pem"), 0o600)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o700), info.Mode().Perm())
}

func TestRoleOUAndTTL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "zzrouter-worker", RoleWorker.OU())
	assert.Equal(t, "zzrouter-coordinator", RoleCoordinator.OU())
	assert.Equal(t, "", Role(99).OU())

	assert.Equal(t, 30*24*time.Hour, RoleWorker.TTL())
	assert.Equal(t, 20*365*24*time.Hour, RoleCoordinator.TTL())
	assert.Equal(t, time.Duration(0), Role(99).TTL())

	assert.True(t, RoleWorker.Valid())
	assert.True(t, RoleCoordinator.Valid())
	assert.False(t, Role(0).Valid())
	assert.False(t, Role(99).Valid())
}

// CA sign round-trip: generate a CA, generate an Identity, build a CSR,
// sign it, verify the result chains to the CA and carries the right OU +
// TTL + SANs.
func TestCASignRoundTrip(t *testing.T) {
	t.Parallel()
	caDir := t.TempDir()
	idDir := t.TempDir()

	ca, err := LoadOrCreateCA(caDir)
	require.NoError(t, err)

	id, err := LoadOrCreateIdentity(idDir)
	require.NoError(t, err)

	sans := []string{"worker1.lan", "worker1"}
	ips := []net.IP{net.IPv4(10, 1, 5, 10)}
	csrPEM, err := id.CSR("worker1", sans, ips)
	require.NoError(t, err)

	signedPEM, err := ca.Sign(csrPEM, RoleWorker)
	require.NoError(t, err)

	cert := mustParseCert(t, signedPEM)

	// OU is role-specific.
	require.Len(t, cert.Subject.OrganizationalUnit, 1)
	assert.Equal(t, "zzrouter-worker", cert.Subject.OrganizationalUnit[0])

	// TTL is 30 days ± a few seconds.
	ttl := cert.NotAfter.Sub(cert.NotBefore)
	assert.InDelta(t, RoleWorker.TTL().Seconds(), ttl.Seconds(), 5)

	// SANs are copied verbatim.
	assert.ElementsMatch(t, sans, cert.DNSNames)
	require.Len(t, cert.IPAddresses, 1)
	assert.True(t, cert.IPAddresses[0].Equal(ips[0]))

	// Chains to the CA.
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	assert.NoError(t, err, "signed cert must chain to CA")

	// Cert's public key matches the identity's key (fingerprint parity).
	assert.True(t, samePublicKey(cert.PublicKey, id.Signer().Public()))
}

func TestCASignRejectsWildcardSAN(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	cases := []string{"*.lan", "*", "*.sub.example.com"}
	for _, san := range cases {
		t.Run(san, func(t *testing.T) {
			t.Parallel()
			csr, err := id.CSR("x", []string{san}, nil)
			require.NoError(t, err)
			_, err = ca.Sign(csr, RoleWorker)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrWildcardSAN)
		})
	}
}

func TestCASignRejectsInvalidRole(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)
	csr, err := id.CSR("x", nil, nil)
	require.NoError(t, err)

	_, err = ca.Sign(csr, Role(99))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRole)

	_, err = ca.Sign(csr, Role(0))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRole)
}

func TestCASignRejectsMalformedCSR(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)

	cases := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"not PEM", []byte("hello")},
		{"wrong PEM type", encodePEM("CERTIFICATE", []byte("garbage"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ca.Sign(tc.input, RoleWorker)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidCSR)
		})
	}
}

// A CSR with a valid signature but corrupted DER should fail CSR parsing,
// not slip through. Unlikely in practice but the fail-closed path matters.
func TestCASignRejectsBadCSRSignature(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)

	// Manually build a CSR and corrupt one signature byte.
	key, err := generateECDSAKey()
	require.NoError(t, err)
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	require.NoError(t, err)
	// Flip the final byte (signature bits).
	der[len(der)-1] ^= 0xff
	bad := encodePEM("CERTIFICATE REQUEST", der)

	_, err = ca.Sign(bad, RoleWorker)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidCSR)
}

func TestInstallSignedCertRejectsKeyMismatch(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	// Build a CSR for a DIFFERENT key, sign it with the CA, then try to
	// install on id. Must be rejected with ErrKeyMismatch.
	otherKey, err := generateECDSAKey()
	require.NoError(t, err)
	otherCSR := mustMakeCSR(t, otherKey, "other", nil, nil)
	signed, err := ca.Sign(otherCSR, RoleWorker)
	require.NoError(t, err)

	err = id.InstallSignedCert(signed)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyMismatch)
}

func TestInstallSignedCertSucceedsForMatchingKey(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	csr, err := id.CSR("worker1", []string{"worker1"}, nil)
	require.NoError(t, err)
	signed, err := ca.Sign(csr, RoleWorker)
	require.NoError(t, err)

	fpBefore := id.Fingerprint()
	require.NoError(t, id.InstallSignedCert(signed))
	assert.Equal(t, fpBefore, id.Fingerprint(), "installing a signed cert must preserve the fingerprint")

	// Cert on disk now chains to the CA.
	reloaded, err := LoadOrCreateIdentity(id.dir)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	_, err = reloaded.Certificate().Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	assert.NoError(t, err)
}

func TestInstallSignedCertRejectsMalformedPEM(t *testing.T) {
	t.Parallel()
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	cases := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"wrong block type", encodePEM("CERTIFICATE REQUEST", []byte("hi"))},
		{"trailing bytes", append(id.CertPEM(), 'x', 'y', 'z')},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := id.InstallSignedCert(tc.input)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrBadCertPEM)
		})
	}
}

func TestLoadIdentityRegeneratesCertOnKeyMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	id1, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	fpOrig := id1.Fingerprint()

	// Swap out the cert for an unrelated self-signed cert. Simulates
	// corruption or a half-written rename.
	bogus := buildBogusCertPEM(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.pem"), bogus, 0o600))

	id2, err := LoadOrCreateIdentity(dir)
	require.NoError(t, err)
	// Key is still the original; cert was regenerated to match.
	assert.Equal(t, fpOrig, id2.Fingerprint())
	assert.True(t, samePublicKey(id2.Certificate().PublicKey, id2.Signer().Public()))
}

func TestLoadIdentityFailsOnMalformedKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"),
		[]byte("not a key"), 0o600))

	_, err := LoadOrCreateIdentity(dir)
	require.Error(t, err)
	// Must NOT silently overwrite — the plan is explicit about this.
	_, statErr := os.Stat(filepath.Join(dir, "node.key"))
	assert.NoError(t, statErr, "bad key file must be preserved, not overwritten")
}

func TestWriteAtomicPreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX perms test")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, writeAtomic(path, []byte("hello"), 0o600))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

// --- helpers ---

func mustParseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	require.NotNil(t, block)
	require.Equal(t, "CERTIFICATE", block.Type)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func mustMakeCSR(t *testing.T, key *ecdsa.PrivateKey, cn string, dnsNames []string, ips []net.IP) []byte {
	t.Helper()
	tmpl := &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: cn},
		DNSNames:    dnsNames,
		IPAddresses: ips,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	require.NoError(t, err)
	return encodePEM("CERTIFICATE REQUEST", der)
}

func buildBogusCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := generateECDSAKey()
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "bogus"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	return encodePEM("CERTIFICATE", der)
}

// Package-local error aliasing sanity: ensure the sentinels exported here
// match what callers use via errors.Is. Guards against accidental
// `errors.New(...)` redefinition on refactor.
func TestSentinelErrorsAreSentinels(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		ErrInvalidRole, ErrInvalidCSR, ErrWildcardSAN,
		ErrKeyMismatch, ErrBadCertPEM, ErrBadKeyPEM,
		ErrUnsafeSAN, ErrUnsupportedKey,
	}
	for _, e := range sentinels {
		require.Error(t, e)
		assert.True(t, errors.Is(e, e))
		assert.True(t, strings.HasPrefix(e.Error(), "clusterid: "), e.Error())
	}
}

func TestCASignRejectsUnsafeIPSANs(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	cases := []struct {
		name string
		ip   net.IP
	}{
		{"v4 unspecified", net.IPv4(0, 0, 0, 0)},
		{"v6 unspecified", net.IPv6unspecified},
		{"v4 multicast", net.IPv4(224, 0, 0, 1)},
		{"v6 multicast", net.ParseIP("ff02::1")},
		{"v4 broadcast", net.IPv4bcast},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			csr, err := id.CSR("x", nil, []net.IP{tc.ip})
			require.NoError(t, err)
			_, err = ca.Sign(csr, RoleWorker)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnsafeSAN)
		})
	}
}

func TestCASignAllowsLoopbackAndLinkLocal(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	ips := []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
		net.ParseIP("fe80::1"),
	}
	csr, err := id.CSR("x", nil, ips)
	require.NoError(t, err)
	_, err = ca.Sign(csr, RoleWorker)
	require.NoError(t, err, "loopback + link-local unicast must be allowed")
}

func TestCASignRejectsNonP256Key(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	csr := mustMakeCSR(t, p384, "x", nil, nil)
	_, err = ca.Sign(csr, RoleWorker)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedKey)
}

// Concurrent reads from Certificate()/CertPEM() alongside an
// InstallSignedCert must be race-free. Without the RWMutex this would
// trip `go test -race`.
func TestIdentityConcurrentAccess(t *testing.T) {
	t.Parallel()
	ca, err := LoadOrCreateCA(t.TempDir())
	require.NoError(t, err)
	id, err := LoadOrCreateIdentity(t.TempDir())
	require.NoError(t, err)

	csr, err := id.CSR("x", []string{"x"}, nil)
	require.NoError(t, err)
	signed, err := ca.Sign(csr, RoleWorker)
	require.NoError(t, err)

	done := make(chan struct{})
	stop := make(chan struct{})

	// Readers: hammer the cert accessors.
	const readers = 4
	for i := 0; i < readers; i++ {
		go func() {
			for {
				select {
				case <-stop:
					done <- struct{}{}
					return
				default:
					_ = id.Fingerprint()
					_ = id.Certificate()
					_ = id.CertPEM()
				}
			}
		}()
	}

	// Writer: rotate the cert repeatedly. Re-sign the same CSR so each
	// InstallSignedCert gets a fresh cert with a new serial.
	const rotations = 100
	for i := 0; i < rotations; i++ {
		fresh, err := ca.Sign(csr, RoleWorker)
		require.NoError(t, err)
		require.NoError(t, id.InstallSignedCert(fresh))
		_ = signed // silence unused in the single-shot path
	}

	close(stop)
	for i := 0; i < readers; i++ {
		<-done
	}
}

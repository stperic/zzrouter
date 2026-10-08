package clusternode

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathsFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	p := PathsFromConfigDir(dir)
	assert.Equal(t, filepath.Join(dir, "identity"), p.IdentityDir)
	assert.Equal(t, filepath.Join(dir, "ca"), p.CADir)
	assert.Equal(t, filepath.Join(dir, "cluster"), p.ClusterDir)
	assert.Equal(t, filepath.Join(dir, "pairing.txt"), p.PairingPath)
}

// pathsTestCA bundles generated CA material + leaf helper. Tests here run a
// stdlib-only mini-CA rather than going through pkg/clusterid — that
// keeps this package test-independent from clusterid internals and
// exercises the exact stdlib path that IsPaired uses.
type pathsTestCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	der  []byte
}

func newPathsTestCA(t *testing.T) *pathsTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &pathsTestCA{key: key, cert: cert, der: der}
}

func newPathsLeafSignedBy(t *testing.T, ca *pathsTestCA) []byte {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-worker"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &leafKey.PublicKey, ca.key)
	require.NoError(t, err)
	return der
}

func writePathsPEMCert(t *testing.T, path string, source any) {
	t.Helper()
	var der []byte
	switch v := source.(type) {
	case *pathsTestCA:
		der = v.der
	case []byte:
		der = v
	default:
		t.Fatalf("writePathsPEMCert: unsupported source type %T", source)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// IsPaired is where the Worker-vs-Unclaimed runtime decision lives.
// Invariants pinned here: missing files → Unclaimed, invalid PEM →
// Unclaimed, identity cert not chaining to stored CA → Unclaimed
// (stale pairing state from a prior install or migrated coordinator),
// valid chain → paired.
func TestIsPaired(t *testing.T) {
	paths := func(id, cl string) Paths {
		return Paths{IdentityDir: id, ClusterDir: cl}
	}

	t.Run("empty dirs are unclaimed", func(t *testing.T) {
		identityDir, clusterDir := t.TempDir(), t.TempDir()
		assert.False(t, IsPaired(paths(identityDir, clusterDir)))
	})

	t.Run("only cluster ca.pem is unclaimed (corrupted state)", func(t *testing.T) {
		identityDir, clusterDir := t.TempDir(), t.TempDir()
		writePathsPEMCert(t, filepath.Join(clusterDir, "ca.pem"), newPathsTestCA(t))
		assert.False(t, IsPaired(paths(identityDir, clusterDir)))
	})

	t.Run("only coordinator_url is unclaimed (corrupted state)", func(t *testing.T) {
		identityDir, clusterDir := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "coordinator_url"),
			[]byte("https://c"), 0o600))
		assert.False(t, IsPaired(paths(identityDir, clusterDir)))
	})

	t.Run("invalid PEM in ca.pem is unclaimed", func(t *testing.T) {
		identityDir, clusterDir := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "ca.pem"),
			[]byte("not-a-pem-block"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "coordinator_url"),
			[]byte("https://c"), 0o600))
		assert.False(t, IsPaired(paths(identityDir, clusterDir)))
	})

	t.Run("identity not chaining to stored CA is unclaimed", func(t *testing.T) {
		identityDir, clusterDir := t.TempDir(), t.TempDir()
		stored := newPathsTestCA(t)
		rogue := newPathsTestCA(t)
		writePathsPEMCert(t, filepath.Join(clusterDir, "ca.pem"), stored)
		writePathsPEMCert(t, filepath.Join(identityDir, "node.pem"), newPathsLeafSignedBy(t, rogue))
		require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "coordinator_url"),
			[]byte("https://c"), 0o600))
		assert.False(t, IsPaired(paths(identityDir, clusterDir)))
	})

	t.Run("valid chain is paired", func(t *testing.T) {
		identityDir, clusterDir := t.TempDir(), t.TempDir()
		ca := newPathsTestCA(t)
		writePathsPEMCert(t, filepath.Join(clusterDir, "ca.pem"), ca)
		writePathsPEMCert(t, filepath.Join(identityDir, "node.pem"), newPathsLeafSignedBy(t, ca))
		require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "coordinator_url"),
			[]byte("https://c"), 0o600))
		assert.True(t, IsPaired(paths(identityDir, clusterDir)))
	})
}

func TestReadCoordinatorURL(t *testing.T) {
	t.Run("trims whitespace", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "coordinator_url"),
			[]byte("  https://c.example:9091\n"), 0o600))

		url, err := ReadCoordinatorURL(dir)
		require.NoError(t, err)
		assert.Equal(t, "https://c.example:9091", url)
	})

	t.Run("rejects empty file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "coordinator_url"),
			[]byte("   \n"), 0o600))

		_, err := ReadCoordinatorURL(dir)
		require.Error(t, err)
		assert.ErrorIs(t, err, errCoordinatorURLEmpty)
	})

	t.Run("propagates missing file", func(t *testing.T) {
		_, err := ReadCoordinatorURL(t.TempDir())
		require.Error(t, err)
	})
}

func TestParseAdvertiseIPs(t *testing.T) {
	t.Run("empty input returns nil", func(t *testing.T) {
		assert.Nil(t, ParseAdvertiseIPs(nil))
		assert.Nil(t, ParseAdvertiseIPs([]string{}))
	})

	t.Run("drops invalid entries", func(t *testing.T) {
		ips := ParseAdvertiseIPs([]string{"10.0.0.1", "not-an-ip", " 192.168.1.1 "})
		require.Len(t, ips, 2)
		assert.True(t, ips[0].Equal(net.ParseIP("10.0.0.1")))
		assert.True(t, ips[1].Equal(net.ParseIP("192.168.1.1")))
	})

	t.Run("all invalid returns nil", func(t *testing.T) {
		assert.Nil(t, ParseAdvertiseIPs([]string{"nope", "also-nope"}))
	})
}

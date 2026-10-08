package clusternode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io/fs"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// denyList unit tests
// ---------------------------------------------------------------------

func TestDenyListAddAndContains(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deny.json")
	d, err := loadDenyList(path)
	require.NoError(t, err)

	assert.False(t, d.Contains("sha256:xx"))
	require.NoError(t, d.Add(denyEntry{
		Fingerprint: "sha256:xx",
		Serial:      "abc",
		Reason:      "compromised",
		RevokedAt:   time.Unix(1_700_000_000, 0).UTC(),
	}))
	assert.True(t, d.Contains("sha256:xx"))
}

func TestDenyListPersistsAcrossReload(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deny.json")
	d, err := loadDenyList(path)
	require.NoError(t, err)
	require.NoError(t, d.Add(denyEntry{Fingerprint: "sha256:a", RevokedAt: time.Now().UTC()}))
	require.NoError(t, d.Add(denyEntry{Fingerprint: "sha256:b", RevokedAt: time.Now().UTC()}))

	d2, err := loadDenyList(path)
	require.NoError(t, err)
	assert.True(t, d2.Contains("sha256:a"))
	assert.True(t, d2.Contains("sha256:b"))
	assert.False(t, d2.Contains("sha256:c"))
}

func TestDenyListRefusesMalformedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deny.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))
	_, err := loadDenyList(path)
	require.Error(t, err)
}

func TestDenyListLoadSkipsRowsWithoutFingerprint(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deny.json")
	// Hand-crafted JSON mixing a valid row with two empty-fingerprint
	// rows; the valid one should load, the other two should be
	// dropped with a WARN (observationally: no panic, len == 1).
	const raw = `[
	  {"fingerprint":"sha256:aa","revoked_at":"2026-01-01T00:00:00Z"},
	  {"serial":"00","revoked_at":"2026-01-02T00:00:00Z"},
	  {"fingerprint":"","serial":"01","revoked_at":"2026-01-03T00:00:00Z"}
	]`
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
	d, err := loadDenyList(path)
	require.NoError(t, err)
	assert.True(t, d.Contains("sha256:aa"))
	assert.Len(t, d.entries, 1)
}

func TestDenyListAddRollsBackOnFlushError(t *testing.T) {
	t.Parallel()
	// Point filePath at a directory (not a file) so writeClusterFile's
	// rename step fails deterministically. Either the initial Add
	// fails and leaves entries empty, or tempfile creation succeeds
	// and rename-over-dir fails — either way, the map must not
	// retain the entry.
	dir := t.TempDir()
	d := &denyList{
		entries:  map[string]denyEntry{},
		filePath: dir, // not a file — rename will fail
	}
	err := d.Add(denyEntry{Fingerprint: "sha256:rollback", RevokedAt: time.Now().UTC()})
	require.Error(t, err)
	assert.False(t, d.Contains("sha256:rollback"),
		"failed flush must not leave the map with a phantom entry")
}

func TestDenyListFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX perms")
	}
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deny.json")
	d, err := loadDenyList(path)
	require.NoError(t, err)
	require.NoError(t, d.Add(denyEntry{Fingerprint: "sha256:x", RevokedAt: time.Now().UTC()}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
}

// ---------------------------------------------------------------------
// mTLS middleware unit test — gin.TestMode, no real TLS.
// ---------------------------------------------------------------------

func TestMTLSOUCheckRejectsWrongOU(t *testing.T) {
	t.Parallel()
	n := &Node{}
	mw := n.mTLSOUCheck("zzrouter-worker")

	cert := &x509.Certificate{Subject: pkix.Name{OrganizationalUnit: []string{"zzrouter-coordinator"}}}
	c, w := newGinContextWithClientCert(t, cert)
	mw(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.True(t, c.IsAborted())
}

func TestMTLSOUCheckRejectsMissingTLSState(t *testing.T) {
	t.Parallel()
	n := &Node{}
	mw := n.mTLSOUCheck("zzrouter-worker")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/renew", nil)
	// no TLS on request — middleware must 401.
	mw(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMTLSOUCheckAcceptsMatchingOU(t *testing.T) {
	t.Parallel()
	n := &Node{} // deny list is nil → skipped
	mw := n.mTLSOUCheck("zzrouter-worker")

	cert := &x509.Certificate{
		Subject:      pkix.Name{OrganizationalUnit: []string{"zzrouter-worker"}},
		SerialNumber: big.NewInt(42),
	}
	c, _ := newGinContextWithClientCert(t, cert)
	mw(c)
	assert.False(t, c.IsAborted(), "matching-OU client cert must pass through")
}

func TestMTLSOUCheckRejectsDenyListedFingerprint(t *testing.T) {
	t.Parallel()
	cert := &x509.Certificate{
		Subject:      pkix.Name{OrganizationalUnit: []string{"zzrouter-worker"}},
		SerialNumber: big.NewInt(42),
	}
	fp := clusterid.Fingerprint(cert)
	n := &Node{deny: &denyList{entries: map[string]denyEntry{
		fp: {Fingerprint: fp, RevokedAt: time.Now().UTC()},
	}}}
	mw := n.mTLSOUCheck("zzrouter-worker")

	c, w := newGinContextWithClientCert(t, cert)
	mw(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// ---------------------------------------------------------------------
// Revoke API
// ---------------------------------------------------------------------

func TestRevokeRequiresCoordinator(t *testing.T) {
	t.Parallel()
	worker, _ := startNode(t, Unclaimed)
	err := worker.Revoke("sha256:x", "abc", "test")
	assert.ErrorIs(t, err, ErrNotCoordinator)
}

func TestRevokeAddsAndPersists(t *testing.T) {
	t.Parallel()
	coord, _ := startCoordinator(t)
	require.NoError(t, coord.Revoke("sha256:x", "abc", "test"))
	assert.True(t, coord.deny.Contains("sha256:x"))

	// Reload the deny list from disk to confirm the persisted row.
	reloaded, err := loadDenyList(denyListPath(coord.cfg.CADir))
	require.NoError(t, err)
	assert.True(t, reloaded.Contains("sha256:x"))
}

func TestRevokeRejectsEmptyFingerprint(t *testing.T) {
	t.Parallel()
	coord, _ := startCoordinator(t)
	err := coord.Revoke("", "abc", "test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
}

// Two-node end-to-end coverage (leave → revert, renewal issues new
// cert, renewal 403 → revert) previously exercised the deleted
// coordinator-initiated claim driver. Equivalent coverage for the new
// worker-initiated pairing flow requires a multi-node harness and is
// tracked as Tier 2 of handoff item #3. Single-node coverage lives in
// pairing_client_test.go + pairing_lifecycle_test.go + the admin
// handler tests under internal/server.

// ---------------------------------------------------------------------
// Helpers — gin context with TLS, minimal Coordinator bring-up.
// ---------------------------------------------------------------------

// startCoordinator boots a Coordinator Node on an OS-assigned port for
// the Revoke-API tests below. Paired with startNode(Unclaimed) to
// cover the mode-split assertions in the Revoke flow without dragging
// the deleted claim-driver harness along with it.
func startCoordinator(t *testing.T) (*Node, string) {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Mode:        Coordinator,
		Port:        0,
		BindHost:    "127.0.0.1",
		IdentityDir: filepath.Join(root, "coord-identity"),
		CADir:       filepath.Join(root, "coord-ca"),
		NodeName:    "coordinator",
	}
	n, err := New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = n.Stop(context.Background()) })
	require.NoError(t, n.Start(ctx))
	return n, "https://" + n.Addr().String()
}

func newGinContextWithClientCert(t *testing.T, cert *x509.Certificate) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/renew", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		VerifiedChains:   [][]*x509.Certificate{{cert}}, // non-empty
	}
	c.Request = req
	return c, w
}

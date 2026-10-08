package servercli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestRotateKeyIn_FullFlow exercises the core file-manipulation path
// that rotate-key performs: back up + remove CA + identity, rewrite
// node.yaml with empty endpoints, leave a lockfile-cleanup trail.
//
// The outer runRotateKey wrapper calls os.Exit on every error branch
// (confirm missing / server running / not-coordinator / concurrent)
// and is tested by direct invocation of the gates rather than here.
func TestRotateKeyIn_FullFlow(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()

	// Seed the on-disk layout a coordinator would have.
	mustWrite(t, filepath.Join(configDir, "ca", "ca.key"), "old-ca-key")
	mustWrite(t, filepath.Join(configDir, "ca", "ca.pem"), "old-ca-cert")
	mustWrite(t, filepath.Join(configDir, "identity", "node.key"), "old-node-key")
	mustWrite(t, filepath.Join(configDir, "identity", "node.pem"), "old-node-cert")
	mustWrite(t, filepath.Join(configDir, "node.yaml"), `
node:
  name: coord
  port: 9090
cluster:
  mode: coordinator
  endpoints:
    - 10.0.0.2:9090
    - 10.0.0.3:9090
auth:
  admin_api_key: test-admin-key-000000000000000000
`)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	now := time.Date(2026, 4, 19, 15, 30, 0, 0, time.UTC)
	require.NoError(t, rotateKeyIn(cmd, configDir, now))

	ts := "20260419T153000Z"

	// Originals gone.
	for _, rel := range rotateKeyTargets {
		_, err := os.Stat(filepath.Join(configDir, rel))
		require.True(t, os.IsNotExist(err), "%s must be removed", rel)
	}
	// Backups present with deterministic suffix.
	for _, rel := range rotateKeyTargets {
		bak := filepath.Join(configDir, rel+".bak-"+ts)
		_, err := os.Stat(bak)
		require.NoError(t, err, "backup %s must exist", bak)
	}

	// node.yaml backup + cleared endpoints.
	_, err := os.Stat(filepath.Join(configDir, "node.yaml.bak-"+ts))
	require.NoError(t, err, "node.yaml backup must exist")

	updated, err := os.ReadFile(filepath.Join(configDir, "node.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(updated), "10.0.0.2", "stale endpoints must be cleared")
	require.NotContains(t, string(updated), "10.0.0.3", "stale endpoints must be cleared")

	// Lockfile removed on success.
	_, err = os.Stat(filepath.Join(configDir, rotateKeyLockFile))
	require.True(t, os.IsNotExist(err), "lockfile must be cleaned up")

	// Worklist printed with both old endpoints.
	require.Contains(t, out.String(), "10.0.0.2:9090")
	require.Contains(t, out.String(), "10.0.0.3:9090")
}

// TestRotateKeyIn_RefusesConcurrentRotation drives rotateKeyIn with a
// pre-seeded lockfile to confirm the O_EXCL guard returns the sentinel
// error (which runRotateKey maps to exit code 5). A regression that
// drops O_EXCL would fail this test via the returned error shape.
func TestRotateKeyIn_RefusesConcurrentRotation(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	mustWrite(t, filepath.Join(configDir, "ca", "ca.key"), "x")
	mustWrite(t, filepath.Join(configDir, rotateKeyLockFile), "")

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := rotateKeyIn(cmd, configDir, time.Now().UTC()) // lint:allow time.Now — test-only fixed wall time
	require.ErrorIs(t, err, errRotateConcurrent,
		"a pre-existing lockfile must cause rotateKeyIn to return errRotateConcurrent")

	// Pre-existing ca.key must survive — a concurrent-failed rotation
	// must not have moved anything.
	_, statErr := os.Stat(filepath.Join(configDir, "ca", "ca.key"))
	require.NoError(t, statErr, "ca/ca.key must remain on disk after a refused rotation")
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

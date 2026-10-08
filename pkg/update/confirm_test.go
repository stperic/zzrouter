package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestConfirmer builds a Confirmer over a temp tree instead of the
// node's real config directory.
func newTestConfirmer(t *testing.T) (*Confirmer, string) {
	t.Helper()
	dir := t.TempDir()
	backupDir := filepath.Join(dir, "backups")
	require.NoError(t, os.MkdirAll(backupDir, 0750))
	return &Confirmer{
		path:      filepath.Join(dir, pendingConfirmFileName),
		installer: NewInstaller(backupDir, 2),
	}, dir
}

func TestConfirmer_NoRecordMeansNothingPending(t *testing.T) {
	c, _ := newTestConfirmer(t)

	pending, err := c.ClaimBoot()
	require.NoError(t, err)
	assert.Nil(t, pending, "a node with no pending update must not think it is on trial")
}

func TestConfirmer_RecordThenClaimCountsAttempts(t *testing.T) {
	c, _ := newTestConfirmer(t)

	from := &version.Version{Major: 1}
	to := &version.Version{Major: 2}
	require.NoError(t, c.Record(from, to, &InstallResult{BackupPath: "/tmp/backup"}))

	for want := 1; want <= 3; want++ {
		pending, err := c.ClaimBoot()
		require.NoError(t, err)
		require.NotNil(t, pending)
		assert.Equal(t, want, pending.Attempts)
		assert.Equal(t, "1.0.0", pending.FromVersion)
		assert.Equal(t, "2.0.0", pending.ToVersion)
	}
}

// TestConfirmer_AttemptSurvivesACrash is the property the whole scheme
// rests on: the increment is on disk before startup continues, so a
// binary that dies later still burned an attempt.
func TestConfirmer_AttemptSurvivesACrash(t *testing.T) {
	c, _ := newTestConfirmer(t)
	require.NoError(t, c.Record(&version.Version{Major: 1}, &version.Version{Major: 2}, nil))

	_, err := c.ClaimBoot()
	require.NoError(t, err)

	// A completely separate Confirmer, as the next process would build.
	next := &Confirmer{path: c.path, installer: c.installer}
	pending, err := next.ClaimBoot()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, 2, pending.Attempts)
}

func TestConfirmer_CommitClearsTheRecord(t *testing.T) {
	c, _ := newTestConfirmer(t)
	require.NoError(t, c.Record(&version.Version{Major: 1}, &version.Version{Major: 2}, nil))

	require.NoError(t, c.Commit())
	assert.NoFileExists(t, c.Path())

	// Commit is idempotent: a second one must not fail the boot.
	require.NoError(t, c.Commit())

	pending, err := c.ClaimBoot()
	require.NoError(t, err)
	assert.Nil(t, pending, "a committed update must not be counted against later restarts")
}

func TestConfirmer_RollbackRestoresAndClears(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping Unix-specific test on Windows")
	}

	c, dir := newTestConfirmer(t)

	// Stand up an installed pair and a backup of the previous one, as an
	// install would have left them.
	nodePath := filepath.Join(dir, "zzrouter-node")
	require.NoError(t, os.WriteFile(nodePath, fakeBinary("zzrouter-node", "2.0.0"), 0750))
	c.installer.SetCurrentExecutable(nodePath)

	backup := filepath.Join(dir, "backups", "zzrouter-node-20260101-000000.000000000")
	require.NoError(t, os.WriteFile(backup, fakeBinary("zzrouter-node", "1.0.0"), 0750))

	require.NoError(t, c.Record(&version.Version{Major: 1}, &version.Version{Major: 2},
		&InstallResult{BackupPath: backup}))
	pending, err := c.ClaimBoot()
	require.NoError(t, err)

	require.NoError(t, c.Rollback(pending, "test"))

	restored, err := os.ReadFile(nodePath)
	require.NoError(t, err)
	assert.Equal(t, fakeBinary("zzrouter-node", "1.0.0"), restored)
	assert.NoFileExists(t, c.Path(), "a rolled-back update must not be reconsidered on the next boot")
}

// TestConfirmer_RollbackWithNoBackupStillClears: a node that cannot roll
// back must not re-decide the same thing on every boot from then on.
func TestConfirmer_RollbackWithNoBackupStillClears(t *testing.T) {
	c, dir := newTestConfirmer(t)

	nodePath := filepath.Join(dir, "zzrouter-node")
	require.NoError(t, os.WriteFile(nodePath, []byte("node"), 0750))
	c.installer.SetCurrentExecutable(nodePath)

	require.NoError(t, c.Record(&version.Version{Major: 1}, &version.Version{Major: 2}, nil))
	pending, err := c.ClaimBoot()
	require.NoError(t, err)

	err = c.Rollback(pending, "test")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoBackupsAvailable)
	assert.NoFileExists(t, c.Path())
}

func TestConfirmer_UnreadableRecordCannotImplyConfirmation(t *testing.T) {
	c, _ := newTestConfirmer(t)
	require.NoError(t, os.WriteFile(c.Path(), []byte("{not json"), 0600))

	pending, err := c.ClaimBoot()
	require.Error(t, err)
	assert.Nil(t, pending)
	assert.FileExists(t, c.Path())
}

func TestConfirmer_RecordCapturesBothBackups(t *testing.T) {
	c, _ := newTestConfirmer(t)

	require.NoError(t, c.Record(
		&version.Version{Major: 1}, &version.Version{Major: 2},
		&InstallResult{BackupPath: "/b/node", LauncherBackupPath: "/b/launcher"},
	))

	data, err := os.ReadFile(c.Path())
	require.NoError(t, err)

	var pending PendingConfirm
	require.NoError(t, json.Unmarshal(data, &pending))
	assert.Equal(t, "/b/node", pending.BackupPath)
	assert.Equal(t, "/b/launcher", pending.LauncherBackupPath)
	assert.False(t, pending.RecordedAt.IsZero())
}

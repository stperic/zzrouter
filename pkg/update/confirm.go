package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// MaxConfirmAttempts is how many times the node may boot on a freshly
// installed binary without ever reaching a healthy state before the
// update is given up on and rolled back.
//
// One attempt is not enough: a node can lose a boot to something that
// has nothing to do with the update, such as a port still held by the
// process it replaced. Three bounds the damage of a genuinely broken
// release to well under a minute of restarts.
const MaxConfirmAttempts = 3

// pendingConfirmFileName holds the update that has been installed but
// not yet proven to run, next to the update history.
const pendingConfirmFileName = "update-pending-confirm.json"

// PendingConfirm is an update whose binary is on disk and whose restart
// has been requested, but which has not yet come back up healthy.
//
// It exists because the two halves of a restart happen in different
// processes: the one that installs the update exits, and only the one
// that starts afterwards can say whether the new binary works. Without
// a record on disk, a release that cannot serve would restart forever
// with nothing left that remembers what to go back to.
type PendingConfirm struct {
	// FromVersion is what was running before the update.
	FromVersion string `json:"from_version"`

	// ToVersion is the version the node is trying to move to.
	ToVersion string `json:"to_version"`

	// BackupPath and LauncherBackupPath are what a rollback restores.
	BackupPath         string `json:"backup_path,omitempty"`
	LauncherBackupPath string `json:"launcher_backup_path,omitempty"`

	// Attempts counts boots on the new binary that have not yet
	// confirmed. Incremented by the booting process, cleared on success.
	Attempts int `json:"attempts"`

	// RecordedAt is when the update was installed.
	RecordedAt time.Time `json:"recorded_at"`
}

// Confirmer owns the pending-confirmation record and the rollback that
// follows when the new binary never proves itself.
type Confirmer struct {
	path      string
	installer *Installer
}

// NewConfirmer builds a Confirmer over the node's data directory, the
// same one NewScheduler uses for backups and history.
func NewConfirmer(keepPreviousVersions int) *Confirmer {
	return NewConfirmerIn(config.NewConfigManager("zzrouter").GetNodeConfigDir(), keepPreviousVersions)
}

// NewConfirmerIn builds a Confirmer over a caller-chosen directory. The
// privileged updater passes a root-owned one -- see NewSchedulerIn for
// why that matters.
func NewConfirmerIn(dataDir string, keepPreviousVersions int) *Confirmer {
	return &Confirmer{
		path:      filepath.Join(dataDir, pendingConfirmFileName),
		installer: NewInstaller(filepath.Join(dataDir, "backups"), keepPreviousVersions),
	}
}

// Record notes an installed-but-unproven update. Called just before the
// process exits for the restart, so the next boot knows what it is
// looking at and what it can go back to.
func (c *Confirmer) Record(from, to *version.Version, install *InstallResult) error {
	pending := &PendingConfirm{
		ToVersion:  versionString(to),
		RecordedAt: utils.NowUTC(),
	}
	if from != nil {
		pending.FromVersion = from.String()
	}
	if install != nil {
		pending.BackupPath = install.BackupPath
		pending.LauncherBackupPath = install.LauncherBackupPath
	}
	return c.save(pending)
}

// ClaimBoot registers this boot against a pending update and reports
// what is outstanding. Returns nil when no update is awaiting proof.
//
// The increment is persisted before the caller does anything else: a
// binary that crashes later in startup must still have its attempt
// counted, or the loop never terminates.
func (c *Confirmer) ClaimBoot() (*PendingConfirm, error) {
	pending, err := c.load()
	if err != nil || pending == nil {
		return nil, err
	}

	pending.Attempts++
	if err := c.save(pending); err != nil {
		return pending, fmt.Errorf("record update boot attempt: %w", err)
	}
	return pending, nil
}

// Commit accepts the update: the node came up and answered. Removing
// the record is what stops a later, unrelated restart from being
// counted against this update.
func (c *Confirmer) Commit() error {
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear pending update confirmation: %w", err)
	}
	return nil
}

// Rollback gives up on the update and puts the previous binary back.
// The record is cleared either way: a rollback that cannot find its
// backup must not leave the node re-deciding the same thing on every
// boot from then on.
func (c *Confirmer) Rollback(pending *PendingConfirm, reason string) error {
	slog.Error("rolling back an update that did not come up",
		"from", pending.FromVersion,
		"to", pending.ToVersion,
		"attempts", pending.Attempts,
		"reason", reason,
	)

	_, err := c.installer.Rollback()
	if clearErr := c.Commit(); clearErr != nil {
		slog.Warn("failed to clear the pending update record", "err", clearErr)
	}

	switch {
	case errors.Is(err, ErrNoBackupsAvailable):
		return fmt.Errorf("%w: nothing to roll back to", err)
	case err != nil:
		return fmt.Errorf("roll back to %s: %w", pending.FromVersion, err)
	}

	slog.Info("rolled back to the previous version", "version", pending.FromVersion)
	return nil
}

// Path is where the record lives, for status reporting and tests.
func (c *Confirmer) Path() string { return c.path }

func (c *Confirmer) load() (*PendingConfirm, error) {
	data, err := os.ReadFile(c.path) //nolint:gosec // path is derived from the node's own config dir
	if os.IsNotExist(err) {
		return nil, nil //nolint:nilnil // "no update awaiting proof" is the common case, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("read pending update confirmation: %w", err)
	}

	var pending PendingConfirm
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("decode pending confirmation: %w", err)
	}
	return &pending, nil
}

func (c *Confirmer) save(pending *PendingConfirm) error {
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pending update confirmation: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(c.path, data, 0600); err != nil {
		return fmt.Errorf("write pending update confirmation: %w", err)
	}
	return nil
}

func versionString(v *version.Version) string {
	if v == nil {
		return ""
	}
	return v.String()
}

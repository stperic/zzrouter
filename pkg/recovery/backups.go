package recovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stperic/zzrouter/pkg/utils"
)

// BackupCap bounds the bytes the retained online backups hold: 1 GiB.
const BackupCap int64 = 1 << 30

const (
	backupDir     = "access/backups" // under the configuration directory
	archiveSuffix = ".tar.gz"        // an online backup's file is <id>.tar.gz
	// Past the replay window, the three newest archives younger than seven days are kept.
	backupKeep          = 3
	backupMaxAge        = 7 * 24 * time.Hour
	backupPruneInterval = time.Hour
)

var (
	// ErrBackupNotFound is an ID no retained online backup has.
	ErrBackupNotFound = errors.New("no retained online backup has this ID")
	// ErrImageCheck refuses a backup whose copy of the database failed its check.
	ErrImageCheck = errors.New("the backup's copy of the access database failed its check")
)

// LiveStore is the access store an online backup copies: *sqlstore.Store.
type LiveStore interface {
	Capture(ctx context.Context, dst string) (sqlstore.Meta, error)
	Record(ctx context.Context, ev audit.Event) error
	Status() (sqlstore.Status, error)
}

// OnlineBackup is a retained online backup: its archive, the SHA-256 of the
// archive's bytes, and when retention may remove it.
type OnlineBackup struct {
	Archive
	SHA256 string `json:"sha256"`
	// ProtectedUntil ends the replay window, before which it is never removed.
	ProtectedUntil time.Time `json:"protected_until"`
	// ExpiresAt is when retention removes it; a newer backup can only bring it forward.
	ExpiresAt time.Time `json:"expires_at"`
}

// CapacityError refuses a backup that the byte cap, counting what retention
// removes once it exists, or the disk cannot hold. A full disk has no figures.
type CapacityError struct {
	LimitBytes, RetainedBytes, RequiredBytes int64
	FreesAt                                  time.Time // when a retained backup next expires; zero if none does
	disk                                     error
}

func (e *CapacityError) Error() string {
	if e.disk != nil {
		return "no disk space for an online backup: " + e.disk.Error()
	}
	return fmt.Sprintf("online backups of %d bytes and a new one of %d exceed their %d-byte cap", e.RetainedBytes, e.RequiredBytes, e.LimitBytes)
}

func (e *CapacityError) Unwrap() error { return e.disk }

// Backups owns an installation's online backup directory: the archives in it,
// their index, retention, byte cap and pruning. No caller names a file there.
type Backups struct {
	src     LiveStore
	dir     string
	limit   int64
	slot    chan struct{} // held by a create; Close takes it for good
	closing chan struct{}
	done    chan struct{} // closed when pruning stops
	once    sync.Once

	mu    sync.Mutex
	index []OnlineBackup // as retain orders and dates it
}

// OpenBackups opens the online backups of the installation whose
// configuration directory is dir, in an owner-only directory it creates, and
// prunes them hourly until Close. It deletes what a create never keeps:
// temporary files of one that failed, files not named after a backup and
// archives that are not database bundles. A link on the way is an error.
func OpenBackups(ctx context.Context, dir string, src LiveStore) (*Backups, error) {
	b := &Backups{src: src, dir: filepath.Join(dir, filepath.FromSlash(backupDir)), limit: BackupCap,
		slot: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.FromSlash(backupDir), 0o700); err != nil {
		return nil, err
	}
	archives, others, err := readBackupDir(root)
	if err == nil {
		err = security.RestrictDirectoryPermissions(b.dir)
	}
	if err != nil {
		return nil, err
	}
	for _, archive := range archives {
		if archive.StoreMeta == nil {
			others = append(others, archive.ID+archiveSuffix)
			continue
		}
		_, sum, err := hashFile(ctx, b.dir, archive.ID+archiveSuffix)
		if err != nil {
			return nil, err
		}
		b.index = append(b.index, OnlineBackup{Archive: archive, SHA256: sum})
	}
	for _, name := range others {
		b.remove(name)
	}
	retain(b.index)
	b.prune()
	go b.pruneEvery(backupPruneInterval)
	return b, nil
}

// readBackupDir describes the backup directory under the configuration root
// without following a link: a link on the way is an error, and only regular
// files are read. A file named after a backup ID is described from its
// manifest, newest first, then by ID; others names the rest. An absent
// directory holds nothing.
func readBackupDir(root *os.Root) (archives []Archive, others []string, err error) {
	var entries []fs.DirEntry
	if err = rejectLinks(root, filepath.FromSlash(backupDir)); err == nil {
		entries, err = fs.ReadDir(root.FS(), backupDir)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	archives = []Archive{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		id, archive := strings.CutSuffix(entry.Name(), archiveSuffix)
		if parsed, err := uuid.Parse(id); !archive || err != nil || parsed.String() != id {
			others = append(others, entry.Name())
			continue
		}
		described, err := describeArchive(root, filepath.Join(filepath.FromSlash(backupDir), entry.Name()), entry)
		if err != nil {
			return nil, nil, err
		}
		archives = append(archives, described)
	}
	slices.SortFunc(archives, newestFirst)
	return archives, others, nil
}

func newestFirst(a, b Archive) int {
	return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(a.ID, b.ID))
}

// retain orders backups newest first, then by ID, and dates each one. A
// backup is kept inside the replay window, then while it is among the three
// newest past it and younger than seven days: it expires seven days after its
// creation, or once the third newer backup leaves the window, if sooner.
func retain(backups []OnlineBackup) {
	slices.SortFunc(backups, func(a, b OnlineBackup) int { return newestFirst(a.Archive, b.Archive) })
	for i := range backups {
		backup := &backups[i]
		backup.ProtectedUntil = backup.CreatedAt.Add(constants.IdempotencyReplayWindow)
		backup.ExpiresAt = backup.CreatedAt.Add(backupMaxAge)
		if i >= backupKeep && backups[i-backupKeep].ProtectedUntil.Before(backup.ExpiresAt) {
			backup.ExpiresAt = backups[i-backupKeep].ProtectedUntil
		}
	}
}

// List returns the retained backups, newest first.
func (b *Backups) List() []OnlineBackup {
	now := utils.NowUTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	listed := []OnlineBackup{}
	for _, backup := range b.index {
		if now.Before(backup.ExpiresAt) { // an expired one pruning could not remove yet is gone
			listed = append(listed, backup)
		}
	}
	return listed
}

// Get returns the retained backup with this ID, or ErrBackupNotFound.
func (b *Backups) Get(id string) (OnlineBackup, error) {
	for _, backup := range b.List() {
		if backup.ID == id {
			return backup, nil
		}
	}
	return OnlineBackup{}, ErrBackupNotFound
}

// Create copies the live store into a checked archive, commits the "backup
// created" audit row, then lists the archive. A failure or cancellation lists
// nothing and leaves no temporary file; after the audit row, cancellation no
// longer stops it. Creates run one at a time.
func (b *Backups) Create(ctx context.Context) (OnlineBackup, error) {
	select {
	case b.slot <- struct{}{}:
	case <-b.closing:
		return OnlineBackup{}, access.ErrClosed
	case <-ctx.Done():
		return OnlineBackup{}, ctx.Err()
	}
	defer func() { <-b.slot }()
	backup, err := b.create(ctx)
	if sqlitedb.IsFull(err) || osDiskFull(err) {
		err = &CapacityError{disk: err}
	}
	return backup, err
}

func (b *Backups) create(ctx context.Context) (OnlineBackup, error) {
	st, err := b.src.Status()
	if err != nil {
		return OnlineBackup{}, err
	}
	var estimate int64 // an archive is a compressed compact copy of the database
	if st.DatabaseBytes != nil {
		estimate = *st.DatabaseBytes
	}
	if err := b.fits(estimate); err != nil {
		return OnlineBackup{}, err
	}
	id := uuid.NewString()
	image, temp, name := id+".image.tmp", id+archiveSuffix+".tmp", id+archiveSuffix
	defer func() {
		_ = os.Remove(b.path(image))
		_ = os.Remove(b.path(temp))
	}()
	meta, err := b.src.Capture(ctx, b.path(image))
	if err != nil {
		return OnlineBackup{}, err
	}
	img, err := sqlstore.CheckImage(ctx, b.path(image))
	var refusal *sqlstore.Refusal
	switch {
	case errors.As(err, &refusal):
		return OnlineBackup{}, fmt.Errorf("%w: %w", ErrImageCheck, err)
	case err != nil:
		return OnlineBackup{}, err
	case img.Meta != meta:
		return OnlineBackup{}, fmt.Errorf("%w: it is not the state the store published", ErrImageCheck)
	}
	size, sum, err := hashFile(ctx, b.dir, image)
	if err != nil {
		return OnlineBackup{}, err
	}
	store := StoreMeta(meta)
	manifest := Manifest{Version: 1, Profile: ProfileDatabase, Created: utils.NowUTC(), Store: &store,
		Files: []File{{Name: imageName, Size: size, SHA256: sum, root: b.dir, source: image}}}
	if err := writeBundle(ctx, manifest, b.path(temp)); err != nil {
		return OnlineBackup{}, err
	}
	size, sum, err = hashFile(ctx, b.dir, temp)
	if err == nil {
		err = b.fits(size)
	}
	if err != nil {
		return OnlineBackup{}, err
	}
	if err := b.src.Record(ctx, audit.Event{Action: audit.ActionBackupCreated, Subject: id,
		Metadata: map[string]any{"generation": meta.Generation, "revision": meta.Revision, "sequence": meta.Sequence}}); err != nil {
		return OnlineBackup{}, err
	}
	if err := sqlitedb.Rename(b.path(temp), b.path(name)); err != nil {
		return OnlineBackup{}, err
	}
	// CheckImage refuses every schema but this release's, so the archive is readable.
	b.mu.Lock()
	b.index = append(b.index, OnlineBackup{Archive: Archive{ID: id, CreatedAt: manifest.Created, SizeBytes: size, StoreMeta: &store, Readable: true}, SHA256: sum})
	retain(b.index)
	b.mu.Unlock()
	b.prune()
	return b.Get(id)
}

// fits refuses, with a *CapacityError, a new archive of size bytes that the
// cap cannot hold beside the archives retention keeps. A new archive ends no
// other's retention before it leaves the replay window, so this is retention
// projected over the archive.
func (b *Backups) fits(size int64) error {
	full := CapacityError{LimitBytes: b.limit, RequiredBytes: size}
	for _, kept := range b.List() {
		full.RetainedBytes += kept.SizeBytes
		full.FreesAt = kept.ExpiresAt // expiry follows age, so the oldest frees first
	}
	if full.RetainedBytes+size <= b.limit {
		return nil
	}
	return &full
}

// prune removes the archives retention no longer keeps. One it cannot remove,
// such as a file open on Windows, stays unlisted until a later prune.
func (b *Backups) prune() {
	now := utils.NowUTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.index = slices.DeleteFunc(b.index, func(backup OnlineBackup) bool {
		return !now.Before(backup.ExpiresAt) && b.remove(backup.ID+archiveSuffix)
	})
}

func (b *Backups) pruneEvery(interval time.Duration) {
	defer close(b.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.closing:
			return
		case <-ticker.C:
			b.prune()
		}
	}
}

// remove deletes name from the backup directory; a failure is logged and
// left for a later prune or open.
func (b *Backups) remove(name string) bool {
	err := os.Remove(b.path(name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("Could not remove an online backup file; retrying later", "file", name, "error", err)
		return false
	}
	return true
}

func (b *Backups) path(name string) string { return filepath.Join(b.dir, name) }

// Close stops pruning and waits for a create in flight; later creates refuse
// with access.ErrClosed. List and Get keep answering.
func (b *Backups) Close() {
	b.once.Do(func() {
		close(b.closing)
		<-b.done
		b.slot <- struct{}{}
	})
}

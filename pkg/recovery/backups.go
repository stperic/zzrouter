package recovery

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	imageSuffix   = ".image.tmp"     // a create's temporary files: <id>.image.tmp,
	tempSuffix    = ".tar.gz.tmp"    // its journal and <id>.tar.gz.tmp
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
	// ErrArchiveRefused refuses to export a retained archive that fails its
	// check, or is not the archive listed.
	ErrArchiveRefused = errors.New("the backup's archive failed its check")
)

// LiveStore is the access store an online backup copies: *sqlstore.Store.
type LiveStore interface {
	Capture(ctx context.Context, dst string) (sqlstore.Meta, error)
	Record(ctx context.Context, ev audit.Event) error
}

// OnlineBackup is a retained online backup: its archive, the SHA-256 of the
// archive's bytes, and when retention may remove it. Its store identity is
// its manifest's, which names another store for a bundle copied in.
type OnlineBackup struct {
	Archive
	SHA256 string `json:"sha256"`
	// ProtectedUntil ends the replay window, before which it is never removed.
	ProtectedUntil time.Time `json:"protected_until"`
	// ExpiresAt is when retention removes it; a newer backup can only bring it forward.
	ExpiresAt time.Time `json:"expires_at"`
}

// CapacityError refuses a backup that the byte cap, counting what retention
// keeps, or the disk cannot hold. A full disk has no figures.
type CapacityError struct {
	LimitBytes, RetainedBytes, RequiredBytes int64 // RequiredBytes is 0 before the archive is written
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
	rename  func(from, to string) error // publishes an archive under its final name
	slot    chan struct{}               // held by a create; Close takes it for good
	closing chan struct{}
	done    chan struct{} // closed when pruning stops
	once    sync.Once

	mu    sync.Mutex
	index []OnlineBackup // as retain orders and dates it
}

// OpenBackups opens the online backups of the installation whose
// configuration directory is dir, in an owner-only directory it creates, and
// prunes them hourly until Close. It indexes the database bundles there,
// deletes only the temporary files a create left and leaves anything else
// alone. A link on the way is an error.
func OpenBackups(ctx context.Context, dir string, src LiveStore) (*Backups, error) {
	b := &Backups{src: src, dir: filepath.Join(dir, filepath.FromSlash(backupDir)), limit: BackupCap,
		rename: sqlitedb.Rename, slot: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.FromSlash(backupDir), 0o700); err != nil {
		return nil, err
	}
	archives, temporaries, err := readBackupDir(root)
	if err == nil {
		err = security.RestrictDirectoryPermissions(b.dir)
	}
	if err != nil {
		return nil, err
	}
	for _, name := range temporaries {
		b.remove(name, "a temporary file of an interrupted backup")
	}
	for _, archive := range archives {
		if archive.StoreMeta == nil { // not a database bundle this release reads
			continue
		}
		_, sum, err := hashFile(ctx, b.dir, archive.ID+archiveSuffix)
		if err != nil {
			return nil, err
		}
		b.index = append(b.index, OnlineBackup{Archive: archive, SHA256: sum})
	}
	retain(b.index)
	b.prune()
	go b.pruneEvery(backupPruneInterval)
	return b, nil
}

// readBackupDir describes the backup directory under the configuration root
// without following a link: a link on the way is an error, and only regular
// files are read. A file named after a backup is described from its manifest,
// newest first, then by ID, readable or not; temporaries names a create's
// temporary files. Every other file is ignored. An absent directory holds
// nothing.
func readBackupDir(root *os.Root) (archives []Archive, temporaries []string, err error) {
	var entries []fs.DirEntry
	if err = rejectLinks(root, filepath.FromSlash(backupDir)); err == nil {
		entries, err = fs.ReadDir(root.FS(), backupDir)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	archives = []Archive{}
	for _, entry := range entries {
		switch name := entry.Name(); {
		case !entry.Type().IsRegular():
		case named(name, archiveSuffix):
			described, err := describeArchive(root, filepath.Join(filepath.FromSlash(backupDir), name), entry)
			if err != nil {
				return nil, nil, err
			}
			archives = append(archives, described)
		case named(name, imageSuffix), named(name, sqlitedb.Journal(imageSuffix)), named(name, tempSuffix):
			temporaries = append(temporaries, name)
		}
	}
	slices.SortFunc(archives, newestFirst)
	return archives, temporaries, nil
}

// named reports whether name is a canonical backup ID followed by suffix.
func named(name, suffix string) bool {
	id, ok := strings.CutSuffix(name, suffix)
	parsed, err := uuid.Parse(id)
	return ok && err == nil && parsed.String() == id
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

// Export is a retained backup opened for streaming, checked on the file it
// streams from and positioned at its start. The caller closes File.
type Export struct {
	OnlineBackup
	File *os.File
}

// Open opens the retained backup with this ID for export, checked (Check),
// then commits the "backup exported" audit row, so no byte leaves unchecked
// or unrecorded.
func (b *Backups) Open(ctx context.Context, id string) (Export, error) {
	backup, f, err := b.open(ctx, id)
	if err != nil {
		return Export{}, err
	}
	if _, err = f.Seek(0, io.SeekStart); err == nil {
		err = b.src.Record(ctx, audit.Event{Action: audit.ActionBackupExported, Subject: id, Metadata: map[string]any{"sha256": backup.SHA256}})
	}
	if err != nil {
		_ = f.Close()
		return Export{}, err
	}
	return Export{OnlineBackup: backup, File: f}, nil
}

// Check returns the retained backup with this ID once its archive on disk
// passes the check recovery check runs and hashes to the listed SHA-256, so
// nothing relies on an archive that is gone or altered. A failed check is
// ErrArchiveRefused; a cancellation or a file-system failure is returned as
// it is.
func (b *Backups) Check(ctx context.Context, id string) (OnlineBackup, error) {
	backup, f, err := b.open(ctx, id)
	if err == nil {
		err = f.Close()
	}
	return backup, err
}

// open is Check, keeping the checked file open.
func (b *Backups) open(ctx context.Context, id string) (OnlineBackup, *os.File, error) {
	backup, err := b.Get(id)
	if err != nil {
		return OnlineBackup{}, nil, err
	}
	f, err := openRegular(b.path(id + archiveSuffix))
	if errors.Is(err, fs.ErrNotExist) {
		err = ErrBackupNotFound // pruned since it was listed
	}
	if err != nil {
		return OnlineBackup{}, nil, err
	}
	sum := sha256.New()
	r, err := readArchiveManifest(struct { // closes f when it fails
		io.Reader
		io.Closer
	}{io.TeeReader(f, sum), f})
	if err == nil {
		_, err = r.check(ctx, "", extraction{scratch: checkScratch()}) // reads the whole file
	}
	if err == nil && hex.EncodeToString(sum.Sum(nil)) != backup.SHA256 {
		err = errors.New("the archive is not the one listed")
	}
	if err != nil {
		_ = f.Close()
		var path *fs.PathError // a file-system failure: the surroundings failed, not the archive
		if ctx.Err() == nil && !errors.As(err, &path) {
			err = fmt.Errorf("%w: %w", ErrArchiveRefused, err)
		}
		return OnlineBackup{}, nil, err
	}
	return backup, f, nil
}

// Create copies the live store into a checked archive, publishes it, commits
// the "backup created" audit row, then lists the archive. A failure or
// cancellation lists nothing and leaves no file; after the audit row,
// cancellation no longer stops it. Creates run one at a time.
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

func (b *Backups) create(ctx context.Context) (_ OnlineBackup, err error) {
	if err := b.fits(0); err != nil {
		return OnlineBackup{}, err
	}
	id := uuid.NewString()
	image, temp, name := id+imageSuffix, id+tempSuffix, id+archiveSuffix
	defer func() {
		_ = os.Remove(b.path(image))
		_ = os.Remove(b.path(temp))
		if err != nil { // published, then failed: nothing is kept
			_ = os.Remove(b.path(name))
		}
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
	if err := b.rename(b.path(temp), b.path(name)); err != nil {
		return OnlineBackup{}, err
	}
	if err := b.src.Record(ctx, audit.Event{Action: audit.ActionBackupCreated, Subject: id,
		Metadata: map[string]any{"generation": meta.Generation, "revision": meta.Revision, "sequence": meta.Sequence}}); err != nil {
		return OnlineBackup{}, err // the deferred cleanup removes the published file
	}
	// CheckImage refuses every schema but this release's, so the archive is readable.
	b.mu.Lock()
	b.index = append(b.index, OnlineBackup{Archive: Archive{ID: id, CreatedAt: manifest.Created, SizeBytes: size, StoreMeta: &store, Readable: true}, SHA256: sum})
	retain(b.index)
	created := b.index[slices.IndexFunc(b.index, func(backup OnlineBackup) bool { return backup.ID == id })]
	b.mu.Unlock()
	b.prune()
	return created, nil
}

// fits refuses, with a *CapacityError, a new archive of size bytes that the
// cap cannot hold beside the archives retention keeps. Before the archive is
// written its size is unknown, so size is 0 and only a full cap refuses. A
// new archive ends no other's retention before it leaves the replay window,
// so this is retention projected over it.
func (b *Backups) fits(size int64) error {
	full := CapacityError{LimitBytes: b.limit, RequiredBytes: size}
	for _, kept := range b.List() {
		full.RetainedBytes += kept.SizeBytes
	}
	if full.RetainedBytes+max(size, 1) <= b.limit {
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
		return !now.Before(backup.ExpiresAt) && b.remove(backup.ID+archiveSuffix, "an expired backup")
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

// remove deletes name, described by what, from the backup directory and logs
// it; a failure is logged and left for a later prune or open.
func (b *Backups) remove(name, what string) bool {
	err := os.Remove(b.path(name))
	switch {
	case err == nil:
		slog.Info("Removed "+what, "file", name)
	case !errors.Is(err, fs.ErrNotExist):
		slog.Warn("Could not remove "+what+"; retrying later", "file", name, "error", err)
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

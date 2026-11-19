package recovery

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hooked is a live store whose Capture or Record a test replaces.
type hooked struct {
	*sqlstore.Store
	capture func(ctx context.Context, dst string) (sqlstore.Meta, error)
	record  func(ctx context.Context, ev audit.Event) error
}

func (h *hooked) Capture(ctx context.Context, dst string) (sqlstore.Meta, error) {
	if h.capture != nil {
		return h.capture(ctx, dst)
	}
	return h.Store.Capture(ctx, dst)
}

func (h *hooked) Record(ctx context.Context, ev audit.Event) error {
	if h.record != nil {
		return h.record(ctx, ev)
	}
	return h.Store.Record(ctx, ev)
}

// liveStore opens a new installation's access store with a shared team, and
// returns the installation's configuration directory.
func liveStore(t *testing.T) (*sqlstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.CreateTeam(t.Context(), "eng", teams.Team{Name: "Engineering", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	return store, dir
}

func openBackups(t *testing.T, dir string, src LiveStore) *Backups {
	t.Helper()
	b, err := OpenBackups(t.Context(), dir, src)
	require.NoError(t, err)
	t.Cleanup(b.Close)
	return b
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "access", "backups"))
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func archiveOf(dir, id string) string { return filepath.Join(dir, "access", "backups", id+".tar.gz") }

// recorded returns the subjects of the store's backup-created events.
func recorded(t *testing.T, store *sqlstore.Store) []string {
	t.Helper()
	events, _, err := store.AuditPage(t.Context(), audit.Filter{Action: audit.ActionBackupCreated}, "", audit.MaxPage)
	require.NoError(t, err)
	subjects := []string{}
	for _, ev := range events {
		subjects = append(subjects, ev.Subject)
	}
	return subjects
}

// stagedKeys restores the archive into a new staging directory, as an
// operator does, and returns the IDs of the keys its image holds.
func stagedKeys(t *testing.T, archive string) []string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "staging")
	_, err := Restore(t.Context(), archive, "", destination)
	require.NoError(t, err)
	ids := []string{}
	for id := range activate(t, destination).Snapshot().Keys() {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// TestOnlineBackupsHoldTheirRevision: while one writer creates keys, three
// backups run at once; each archive passes the check and holds exactly the
// keys created up to the revision its manifest names.
func TestOnlineBackupsHoldTheirRevision(t *testing.T) {
	store, dir := liveStore(t)
	base := store.Meta().Revision
	b := openBackups(t, dir, store)
	const count = 30
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range count {
			_, _, err := store.CreateKey(t.Context(), fmt.Sprintf("k%03d", i+1), keys.VirtualKey{Name: "Key", Role: "user", TeamID: "eng", TeamRole: "member"}, audit.Event{})
			assert.NoError(t, err)
		}
	})
	created := make([]OnlineBackup, 3)
	errs := make([]error, 3)
	for i := range created {
		wg.Go(func() { created[i], errs[i] = b.Create(t.Context()) })
	}
	wg.Wait()
	for i, backup := range created {
		require.NoError(t, errs[i])
		want := []string{}
		for k := range backup.Revision - base {
			want = append(want, fmt.Sprintf("k%03d", k+1))
		}
		require.Equal(t, want, stagedKeys(t, archiveOf(dir, backup.ID)), "revision %d", backup.Revision)
	}
}

// sqliteFull returns SQLite's own "database or disk is full", from a scratch
// database at its page limit.
func sqliteFull(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "full.sqlite")))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(t.Context(), "CREATE TABLE t (v TEXT); PRAGMA max_page_count = 2")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES (?)", strings.Repeat("x", 1<<16))
	require.Error(t, err)
	return err
}

// platformDiskFull is the operating system's error for a full disk.
func platformDiskFull() error {
	errno := syscall.ENOSPC
	if runtime.GOOS == "windows" {
		errno = syscall.Errno(112) // ERROR_DISK_FULL
	}
	return &fs.PathError{Op: "write", Path: "image", Err: errno}
}

// TestOnlineBackupFailuresListNothing: a corrupt image, an image that is not
// the state the capture reported, a failing audit row, a full disk (the
// platform's error or SQLite's, wrapped) and an archive larger than the byte
// cap each fail the create with their own error, list nothing, record nothing
// and leave no file in the backup directory.
func TestOnlineBackupFailuresListNothing(t *testing.T) {
	errInjected := errors.New("injected audit failure")
	for name, tc := range map[string]struct {
		hook  func(t *testing.T, h *hooked)
		limit int64
		check func(t *testing.T, err error)
	}{
		"a corrupt image": {
			hook: func(t *testing.T, h *hooked) {
				h.capture = func(ctx context.Context, dst string) (sqlstore.Meta, error) {
					meta, err := h.Store.Capture(ctx, dst)
					storeDamages()[sqlstore.IntegrityFailed](t, dst)
					return meta, err
				}
			},
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrImageCheck)
				var refusal *sqlstore.Refusal
				require.ErrorAs(t, err, &refusal)
			},
		},
		"an image that is not the capture's": {
			hook: func(_ *testing.T, h *hooked) {
				h.capture = func(ctx context.Context, dst string) (sqlstore.Meta, error) {
					meta, err := h.Store.Capture(ctx, dst)
					meta.Sequence++
					return meta, err
				}
			},
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, ErrImageCheck)
				var refusal *sqlstore.Refusal
				require.False(t, errors.As(err, &refusal), "zzRouter's fault, not a refused image")
			},
		},
		"a failing audit row": {
			hook: func(_ *testing.T, h *hooked) {
				h.record = func(context.Context, audit.Event) error { return errInjected }
			},
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, errInjected) },
		},
		"a full disk": {
			hook: func(_ *testing.T, h *hooked) {
				h.capture = func(context.Context, string) (sqlstore.Meta, error) {
					return sqlstore.Meta{}, fmt.Errorf("capture: %w", platformDiskFull())
				}
			},
			check: requireDiskFull,
		},
		"SQLite's full database or disk": {
			hook: func(t *testing.T, h *hooked) {
				full := sqliteFull(t)
				h.capture = func(context.Context, string) (sqlstore.Meta, error) {
					return sqlstore.Meta{}, fmt.Errorf("capture: %w", full)
				}
			},
			check: requireDiskFull,
		},
		"an archive larger than the byte cap": {
			limit: 1,
			check: func(t *testing.T, err error) {
				var full *CapacityError
				require.ErrorAs(t, err, &full)
				require.Equal(t, int64(1), full.LimitBytes)
				require.Positive(t, full.RequiredBytes, "refused once written, at its size")
				require.Zero(t, full.RetainedBytes)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, dir := liveStore(t)
			h := &hooked{Store: store}
			if tc.hook != nil {
				tc.hook(t, h)
			}
			b := openBackups(t, dir, h)
			if tc.limit != 0 {
				b.limit = tc.limit
			}
			_, err := b.Create(t.Context())
			tc.check(t, err)
			require.Empty(t, b.List())
			require.Empty(t, backupFiles(t, dir), "no archive and no temporary")
			require.Empty(t, recorded(t, store))
		})
	}
}

func requireDiskFull(t *testing.T, err error) {
	t.Helper()
	var full *CapacityError
	require.ErrorAs(t, err, &full)
	require.Equal(t, CapacityError{disk: full.disk}, *full, "a full disk has no figures")
}

// TestOnlineBackupCancellation: a create cancelled during the copy or after
// it fails with the context's error, lists nothing and leaves nothing; one
// cancelled after its audit row is committed completes and is listed.
func TestOnlineBackupCancellation(t *testing.T) {
	for name, tc := range map[string]struct {
		hook   func(h *hooked, cancel context.CancelFunc)
		listed bool
	}{
		"during the copy": {hook: func(h *hooked, cancel context.CancelFunc) {
			h.capture = func(ctx context.Context, dst string) (sqlstore.Meta, error) {
				cancel()
				return h.Store.Capture(ctx, dst)
			}
		}},
		"after the copy": {hook: func(h *hooked, cancel context.CancelFunc) {
			h.capture = func(ctx context.Context, dst string) (sqlstore.Meta, error) {
				defer cancel()
				return h.Store.Capture(ctx, dst)
			}
		}},
		"after the audit row": {listed: true, hook: func(h *hooked, cancel context.CancelFunc) {
			h.record = func(ctx context.Context, ev audit.Event) error { defer cancel(); return h.Store.Record(ctx, ev) }
		}},
	} {
		t.Run(name, func(t *testing.T) {
			store, dir := liveStore(t)
			h := &hooked{Store: store}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tc.hook(h, cancel)
			b := openBackups(t, dir, h)
			backup, err := b.Create(ctx)
			if !tc.listed {
				require.ErrorIs(t, err, context.Canceled)
				require.Empty(t, b.List())
				require.Empty(t, backupFiles(t, dir))
				require.Empty(t, recorded(t, store))
				return
			}
			require.NoError(t, err)
			require.Equal(t, []OnlineBackup{backup}, b.List())
			require.Equal(t, []string{backup.ID + ".tar.gz"}, backupFiles(t, dir))
			require.Equal(t, []string{backup.ID}, recorded(t, store))
		})
	}
}

// TestOnlineBackupAuditRowComesFirst: when the audit row is committed, the
// archive is neither listed nor in place; once the create returns, it is.
func TestOnlineBackupAuditRowComesFirst(t *testing.T) {
	store, dir := liveStore(t)
	h := &hooked{Store: store}
	var b *Backups
	hooked := 0
	h.record = func(ctx context.Context, ev audit.Event) error {
		err := store.Record(ctx, ev)
		hooked++
		events, _, auditErr := store.AuditPage(ctx, audit.Filter{Action: audit.ActionBackupCreated}, "", 1)
		require.NoError(t, auditErr)
		require.Equal(t, ev.Subject, events[0].Subject, "the row is committed")
		require.Empty(t, b.List(), "and the archive is not listed")
		require.NoFileExists(t, archiveOf(dir, ev.Subject))
		return err
	}
	b = openBackups(t, dir, h)
	backup, err := b.Create(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, hooked)
	require.Equal(t, []OnlineBackup{backup}, b.List())
	require.FileExists(t, archiveOf(dir, backup.ID))
}

// TestOnlineBackupsOpenDeletesOnlyItsTemporaries: opening deletes a create's
// temporary files, the copy's journal included, and nothing else. It indexes
// the database bundles under a backup's name, a newer release's listed as
// unreadable, and prunes an expired one; a file under a backup's name that is
// not a database bundle (garbage, an installation bundle), other names, a
// directory and a link are neither listed nor touched. The inventory lists
// every archive under a backup's name beforehand and deletes nothing.
func TestOnlineBackupsOpenDeletesOnlyItsTemporaries(t *testing.T) {
	store, dir := liveStore(t)
	first := openBackups(t, dir, store)
	kept, err := first.Create(t.Context())
	require.NoError(t, err)
	first.Close()
	backups := filepath.Join(dir, "access", "backups")
	place := func(name string, data []byte) {
		require.NoError(t, os.WriteFile(filepath.Join(backups, name), data, 0o600))
	}
	valid := readBytes(t, archiveOf(dir, kept.ID))
	id := uuid.NewString()
	temporaries := []string{id + ".image.tmp", id + ".image.tmp-journal", id + ".tar.gz.tmp"}
	for _, name := range temporaries {
		place(name, valid)
	}
	for _, name := range []string{"notes.txt", strings.ToUpper(uuid.NewString()) + ".tar.gz", uuid.NewString() + ".tmp"} {
		place(name, valid)
	}
	garbage, installation, newer, expired := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	place(garbage+".tar.gz", []byte("not an archive"))
	place(installation+".tar.gz", readBytes(t, writeTestBundle(t, map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}")})))
	image, meta := capturedImage(t)
	newerMeta := meta
	newerMeta.SchemaVersion = 99
	place(newer+".tar.gz", readBytes(t, databaseBundle(t, image, newerMeta)))
	place(expired+".tar.gz", readBytes(t, databaseBundle(t, image, meta, func(m *Manifest) { m.Created = utils.NowUTC().Add(-8 * 24 * time.Hour) })))
	require.NoError(t, os.Mkdir(filepath.Join(backups, uuid.NewString()+".tar.gz"), 0o700))
	_ = os.Symlink(archiveOf(dir, kept.ID), filepath.Join(backups, uuid.NewString()+".tar.gz"))

	listed, err := readArchives(dir)
	require.NoError(t, err)
	readable := map[string]bool{}
	for _, archive := range listed {
		readable[archive.ID] = archive.Readable
	}
	require.Equal(t, map[string]bool{kept.ID: true, garbage: false, installation: false, newer: false, expired: true}, readable)
	before := backupFiles(t, dir)

	b := openBackups(t, dir, store)
	retained := map[string]bool{}
	for _, backup := range b.List() {
		retained[backup.ID] = backup.Readable
		require.Len(t, backup.SHA256, 64)
	}
	require.Equal(t, map[string]bool{kept.ID: true, newer: false}, retained, "database bundles only; the expired one pruned")
	want := []string{}
	for _, name := range before {
		if !slices.Contains(temporaries, name) && name != expired+".tar.gz" {
			want = append(want, name)
		}
	}
	require.Equal(t, want, backupFiles(t, dir), "only temporaries and the expired backup go")
}

func ids(backups []OnlineBackup) []string {
	out := []string{}
	for _, backup := range backups {
		out = append(out, backup.ID)
	}
	return out
}

// TestOnlineBackupRetention: every backup inside the replay window is kept;
// past it, the three newest are kept and none from seven days on. Ages are on
// both sides of each boundary.
func TestOnlineBackupRetention(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	window, day, hour := constants.IdempotencyReplayWindow, 24*time.Hour, time.Hour
	for name, tc := range map[string]struct {
		ages []time.Duration
		kept []bool
	}{
		"inside the window, at any count":                {[]time.Duration{0, hour, 2 * hour, 3 * hour, window - time.Second}, []bool{true, true, true, true, true}},
		"just past the window":                           {[]time.Duration{window}, []bool{true}},
		"a second short of seven days":                   {[]time.Duration{7*day - time.Second}, []bool{true}},
		"seven days":                                     {[]time.Duration{7 * day}, []bool{false}},
		"the three newest past the window":               {[]time.Duration{window, window + hour, window + 2*hour, window + 3*hour}, []bool{true, true, true, false}},
		"a fourth whose third newer is in":               {[]time.Duration{window - time.Second, window + hour, window + 2*hour, window + 3*hour}, []bool{true, true, true, true}},
		"backups inside the window are not of the three": {[]time.Duration{0, hour, 2 * hour, window + hour, window + 2*hour, window + 3*hour, window + 4*hour}, []bool{true, true, true, true, true, true, false}},
		"seven days ends one of the three":               {[]time.Duration{window, 7*day - time.Second, 7 * day}, []bool{true, true, false}},
	} {
		t.Run(name, func(t *testing.T) {
			backups := make([]OnlineBackup, len(tc.ages))
			for i, age := range tc.ages {
				backups[i] = OnlineBackup{Archive: Archive{ID: fmt.Sprint(i), CreatedAt: now.Add(-age)}}
			}
			slices.Reverse(backups) // retain orders them
			retain(backups)
			kept := []bool{}
			for i, backup := range backups {
				require.Equal(t, fmt.Sprint(i), backup.ID, "newest first")
				require.Equal(t, backup.CreatedAt.Add(window), backup.ProtectedUntil)
				require.False(t, backup.ExpiresAt.Before(backup.ProtectedUntil), "never before the window ends")
				kept = append(kept, now.Before(backup.ExpiresAt))
			}
			require.Equal(t, tc.kept, kept)
		})
	}
}

// backupsAt creates n backups with the clock at at, a minute apart.
func backupsAt(t *testing.T, b *Backups, at time.Time, n int) []OnlineBackup {
	t.Helper()
	created := []OnlineBackup{}
	for i := range n {
		restore := utils.SetClock(utils.FixedClock(at.Add(time.Duration(i) * time.Minute)))
		backup, err := b.Create(t.Context())
		restore()
		require.NoError(t, err)
		created = append(created, backup)
	}
	return created
}

// TestOnlineBackupCapCountsWhatRetentionKeeps: three expired backups pruning
// has not removed yet, though they fill the cap, never block a create, and
// the create prunes them. Three retained ones that fill it refuse a create
// before anything is copied; with room for less than the new archive, the
// written archive is refused at its own size. Not parallel: it sets the clock.
func TestOnlineBackupCapCountsWhatRetentionKeeps(t *testing.T) {
	sum := func(backups []OnlineBackup) (bytes int64) {
		for _, backup := range backups {
			bytes += backup.SizeBytes
		}
		return bytes
	}
	store, dir := liveStore(t)
	b := openBackups(t, dir, store)
	old := backupsAt(t, b, utils.NowUTC().Add(-8*24*time.Hour), 3)
	require.Empty(t, b.List(), "expired")
	require.Len(t, backupFiles(t, dir), 3, "but not pruned yet")
	b.limit = sum(old)
	backup, err := b.Create(t.Context())
	require.NoError(t, err, "the expired backups do not count")
	require.Equal(t, []string{backup.ID + ".tar.gz"}, backupFiles(t, dir), "the create pruned them")

	store, dir = liveStore(t)
	b = openBackups(t, dir, store)
	retained := backupsAt(t, b, utils.NowUTC().Add(-2*24*time.Hour), 3)
	copies := 0
	b.src = &hooked{Store: store, capture: func(ctx context.Context, dst string) (sqlstore.Meta, error) {
		copies++
		return store.Capture(ctx, dst)
	}}
	var full *CapacityError
	b.limit = sum(retained)
	_, err = b.Create(t.Context())
	require.ErrorAs(t, err, &full)
	require.Equal(t, CapacityError{LimitBytes: b.limit, RetainedBytes: sum(retained)}, *full, "refused before the copy, the size unknown")
	b.limit = sum(retained) + 1
	_, err = b.Create(t.Context())
	require.ErrorAs(t, err, &full)
	require.Equal(t, sum(retained), full.RetainedBytes)
	require.Positive(t, full.RequiredBytes, "the written archive is refused at its size")
	require.Equal(t, 1, copies)
	require.Len(t, b.List(), 3)
	require.Len(t, backupFiles(t, dir), 3)
}

// TestOnlineBackupsCreateOneAtATime: two creates at once with room for one:
// the slot makes exactly one succeed and the other meet the cap, even when
// the first is held after its own checks passed.
func TestOnlineBackupsCreateOneAtATime(t *testing.T) {
	store, dir := liveStore(t)
	var calls atomic.Int32
	second := make(chan struct{})
	h := &hooked{Store: store}
	h.record = func(ctx context.Context, ev audit.Event) error {
		if calls.Add(1) == 2 {
			close(second)
		} else {
			select { // the first waits for a second create past its checks, which the slot keeps out
			case <-second:
			case <-time.After(time.Second):
			}
		}
		return store.Record(ctx, ev)
	}
	b := openBackups(t, dir, h)
	probe, err := b.Create(t.Context())
	require.NoError(t, err)
	calls.Store(0)
	b.limit = probe.SizeBytes * 5 / 2 // room for the probe and one more
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { _, errs[i] = b.Create(t.Context()) })
	}
	wg.Wait()
	slices.SortFunc(errs, func(a, b error) int { return cmp.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	require.NoError(t, errs[0])
	var full *CapacityError
	require.ErrorAs(t, errs[1], &full)
	require.Len(t, b.List(), 2)
	require.Len(t, backupFiles(t, dir), 2)
}

// TestOnlineBackupFailedPublishLeavesNothing: when the archive is under its
// final name and its directory sync then fails, the create fails and the
// archive is removed; only the audit row, already committed, remains.
func TestOnlineBackupFailedPublishLeavesNothing(t *testing.T) {
	store, dir := liveStore(t)
	b := openBackups(t, dir, store)
	b.rename = func(from, to string) error {
		require.NoError(t, os.Rename(from, to))
		return errors.New("injected: directory sync failed")
	}
	_, err := b.Create(t.Context())
	require.ErrorContains(t, err, "directory sync failed")
	require.Empty(t, b.List())
	require.Empty(t, backupFiles(t, dir))
	require.Len(t, recorded(t, store), 1, "an event without a backup")
}

// TestOnlineBackupsSurviveARestart: after the backups and the store close and
// open again, a backup created before is listed as it was and its archive
// passes recovery check with the identity it lists.
func TestOnlineBackupsSurviveARestart(t *testing.T) {
	store, dir := liveStore(t)
	b, err := OpenBackups(t.Context(), dir, store)
	require.NoError(t, err)
	backup, err := b.Create(t.Context())
	require.NoError(t, err)
	before, err := json.Marshal(b.List())
	require.NoError(t, err)
	b.Close()
	require.NoError(t, store.Close())

	store, err = sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	reopened := openBackups(t, dir, store)
	after, err := json.Marshal(reopened.List())
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
	manifest, err := Check(t.Context(), archiveOf(dir, backup.ID), "")
	require.NoError(t, err)
	require.Equal(t, ProfileDatabase, manifest.Profile)
	require.Equal(t, *backup.StoreMeta, *manifest.Store)
	got, err := reopened.Get(backup.ID)
	require.NoError(t, err)
	require.Equal(t, backup.SHA256, got.SHA256)
	_, err = reopened.Get(uuid.NewString())
	require.ErrorIs(t, err, ErrBackupNotFound)
}

// TestOnlineBackupConcurrentCreates: creates at once each succeed with an
// archive, a listing and an audit row of their own.
func TestOnlineBackupConcurrentCreates(t *testing.T) {
	store, dir := liveStore(t)
	b := openBackups(t, dir, store)
	created := make([]OnlineBackup, 4)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range created {
		wg.Go(func() { created[i], errs[i] = b.Create(t.Context()) })
	}
	wg.Wait()
	want := []string{}
	for i, backup := range created {
		require.NoError(t, errs[i])
		want = append(want, backup.ID)
		_, err := Check(t.Context(), archiveOf(dir, backup.ID), "")
		require.NoError(t, err)
	}
	slices.Sort(want)
	for _, got := range [][]string{ids(b.List()), recorded(t, store)} {
		slices.Sort(got)
		require.Equal(t, want, got)
	}
	require.Len(t, backupFiles(t, dir), len(want))
}

// TestOnlineBackupCreateRacesClose: Close called while a create copies waits
// for it, and the backup is listed with its archive in place; a create after
// Close refuses with access.ErrClosed and leaves nothing.
func TestOnlineBackupCreateRacesClose(t *testing.T) {
	store, dir := liveStore(t)
	h := &hooked{Store: store}
	b, err := OpenBackups(t.Context(), dir, h)
	require.NoError(t, err)
	copying := make(chan struct{})
	h.capture = func(ctx context.Context, dst string) (sqlstore.Meta, error) {
		close(copying)
		<-b.closing // Close has begun
		return store.Capture(ctx, dst)
	}
	type result struct {
		backup OnlineBackup
		err    error
	}
	created := make(chan result, 1)
	go func() {
		backup, err := b.Create(t.Context())
		created <- result{backup, err}
	}()
	<-copying
	b.Close()
	files, listed := backupFiles(t, dir), b.List() // as Close left them
	done := <-created
	require.NoError(t, done.err)
	require.Equal(t, []string{done.backup.ID + ".tar.gz"}, files, "Close waited for the create")
	require.Equal(t, []OnlineBackup{done.backup}, listed)
	_, err = b.Create(t.Context())
	require.ErrorIs(t, err, access.ErrClosed)
	require.Len(t, backupFiles(t, dir), 1)
	b.Close()
}

// damageImage returns archive with one byte of the image entry's data flipped
// and every digest left as it was; only the gzip layer is written again.
func damageImage(t *testing.T, archive []byte) []byte {
	t.Helper()
	unzipped, err := gzip.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	tarred, err := io.ReadAll(unzipped)
	require.NoError(t, err)
	entries := tar.NewReader(bytes.NewReader(tarred))
	manifest, err := entries.Next()
	require.NoError(t, err)
	image, err := entries.Next()
	require.NoError(t, err)
	require.Equal(t, imageName, image.Name)
	const block = 512 // a header, then data padded to whole blocks
	tarred[block+(manifest.Size+block-1)/block*block+block+image.Size/2] ^= 0xff
	var damaged bytes.Buffer
	zipped := gzip.NewWriter(&damaged)
	_, err = zipped.Write(tarred)
	require.NoError(t, errors.Join(err, zipped.Close()))
	return damaged.Bytes()
}

// TestOnlineBackupOpen: an export reads the archive as it is on disk, checked
// and with one "backup exported" row recorded first. A failing row fails the
// open and an unknown backup is ErrBackupNotFound. An archive damaged while
// the node was down, so the index hashed it at open (an image byte changed,
// or the end cut off), fails the check; another valid archive swapped in
// after listing fails the digest; all are ErrArchiveRefused and record
// nothing. A cancellation and a missing temporary directory are not.
func TestOnlineBackupOpen(t *testing.T) {
	store, dir := liveStore(t)
	h := &hooked{Store: store}
	b := openBackups(t, dir, h)
	backup, err := b.Create(t.Context())
	require.NoError(t, err)
	exported := func() []audit.Event {
		events, _, err := store.AuditPage(t.Context(), audit.Filter{Action: audit.ActionBackupExported}, "", audit.MaxPage)
		require.NoError(t, err)
		return events
	}
	export, err := b.Open(t.Context(), backup.ID)
	require.NoError(t, err)
	content, err := io.ReadAll(export.File)
	require.NoError(t, errors.Join(err, export.File.Close()))
	require.Equal(t, readBytes(t, archiveOf(dir, backup.ID)), content, "positioned at the start, the whole archive")
	require.Equal(t, backup, export.OnlineBackup)
	events := exported()
	require.Len(t, events, 1)
	require.Equal(t, backup.ID, events[0].Subject)
	require.Equal(t, map[string]any{"sha256": backup.SHA256}, events[0].Metadata)

	_, err = b.Open(t.Context(), uuid.NewString())
	require.ErrorIs(t, err, ErrBackupNotFound)
	h.record = func(context.Context, audit.Event) error { return sqlstore.ErrBusy }
	_, err = b.Open(t.Context(), backup.ID)
	require.ErrorIs(t, err, sqlstore.ErrBusy)
	h.record = nil
	other, err := b.Create(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(archiveOf(dir, backup.ID), readBytes(t, archiveOf(dir, other.ID)), 0o600))
	_, err = b.Open(t.Context(), backup.ID)
	require.ErrorIs(t, err, ErrArchiveRefused, "a valid archive, but not the one listed")
	for name, damaged := range map[string][]byte{"an image byte": damageImage(t, content), "a cut end": content[:len(content)-64]} {
		require.NoError(t, os.WriteFile(archiveOf(dir, backup.ID), damaged, 0o600))
		b.Close()
		b = openBackups(t, dir, h) // hashes the damaged file into its index
		_, err = b.Open(t.Context(), backup.ID)
		require.ErrorIs(t, err, ErrArchiveRefused, "%s: listed as it is, but damaged", name)
	}
	require.Len(t, exported(), 1, "a refused export records nothing")

	require.NoError(t, os.WriteFile(archiveOf(dir, backup.ID), content, 0o600))
	b.Close()
	b = openBackups(t, dir, h)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = b.Open(canceled, backup.ID)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrArchiveRefused, "the caller left; the archive is fine")
	missing := filepath.Join(t.TempDir(), "missing")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, missing)
	}
	_, err = b.Open(t.Context(), backup.ID)
	var path *fs.PathError
	require.ErrorAs(t, err, &path)
	require.NotErrorIs(t, err, ErrArchiveRefused, "the temporary directory failed, not the archive")
	require.Len(t, exported(), 1)
}

// TestOnlineBackupExportOutlivesPruning: pruning an archive being exported
// does not cut the stream short, since the open file keeps its bytes (Unix;
// Windows refuses to remove an open file and pruning retries).
func TestOnlineBackupExportOutlivesPruning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps an open file in place")
	}
	store, dir := liveStore(t)
	b := openBackups(t, dir, store)
	backup, err := b.Create(t.Context())
	require.NoError(t, err)
	want := readBytes(t, archiveOf(dir, backup.ID))
	export, err := b.Open(t.Context(), backup.ID)
	require.NoError(t, err)
	defer func() { _ = export.File.Close() }()
	restore := utils.SetClock(utils.FixedClock(utils.NowUTC().Add(8 * 24 * time.Hour)))
	b.prune()
	restore()
	require.NoFileExists(t, archiveOf(dir, backup.ID), "pruned")
	got, err := io.ReadAll(export.File)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

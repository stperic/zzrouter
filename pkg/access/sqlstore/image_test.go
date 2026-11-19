package sqlstore

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/access"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stperic/zzrouter/pkg/utils"
)

// readImage checks the image at path and returns its snapshot and history.
func readImage(t *testing.T, path string) (Image, *access.Snapshot, []audit.Event) {
	t.Helper()
	img, err := CheckImage(t.Context(), path)
	require.NoError(t, err)
	s := &Store{}
	require.NoError(t, s.attach(t.Context(), path, true))
	snap := s.Snapshot()
	require.NoError(t, s.Close())
	return img, snap, fileEvents(t, path)
}

// copyFile copies the file at from to a new temporary path.
func copyFile(t *testing.T, from string) string {
	t.Helper()
	data, err := os.ReadFile(from)
	require.NoError(t, err)
	to := filepath.Join(t.TempDir(), filepath.Base(from))
	require.NoError(t, os.WriteFile(to, data, 0o600))
	return to
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// TestCaptureUnderWriters captures while writers commit: each image holds
// exactly the commits up to the sequence Capture returned, and its meta is
// what Capture returned.
func TestCaptureUnderWriters(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), Options{})
	start := s.Meta().Sequence
	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				_, err := s.CreateTeam(context.Background(), fmt.Sprintf("writer-%d-%d", w, i), engineering(), audit.Event{})
				if !assert.NoError(t, err) {
					return
				}
			}
		})
	}
	var last Meta
	for i := range 8 {
		dst := filepath.Join(t.TempDir(), fmt.Sprint("image-", i))
		m, err := s.Capture(t.Context(), dst)
		require.NoError(t, err)
		img, snap, _ := readImage(t, dst)
		assert.Equal(t, Image{Meta: m}, img, "capture %d returns the image's meta", i)
		assert.Len(t, maps.Collect(snap.Teams()), int(m.Sequence-start), "capture %d holds exactly the commits up to its sequence", i)
		last = m
	}
	close(done)
	wg.Wait()
	assert.Greater(t, last.Sequence, start, "the writers committed between captures")
}

// TestCaptureReleasesTheGate: a write that waits while Capture copies resumes
// once the copy ends, and the image does not hold it.
func TestCaptureReleasesTheGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, Options{})
	other := rawDB(t, dir)
	_, err := other.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	dst := filepath.Join(t.TempDir(), "image.sqlite")
	type result struct {
		meta Meta
		err  error
	}
	captured := make(chan result, 1)
	go func() {
		m, err := s.Capture(context.Background(), dst)
		captured <- result{m, err}
	}()
	// Until Capture holds the gate, its copy waiting for the other program.
	for s.gate.TryLock() {
		s.gate.Unlock()
		time.Sleep(time.Millisecond)
	}
	written := make(chan error, 1)
	go func() {
		_, err := s.CreateTeam(context.Background(), "during", engineering(), audit.Event{})
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("a write committed during the copy: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_, err = other.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(t, err)
	got := <-captured
	require.NoError(t, got.err)
	select {
	case err := <-written:
		require.NoError(t, err, "the waiting write resumes once the copy ends")
	case <-time.After(10 * time.Second):
		t.Fatal("the write never resumed: Capture kept the gate")
	}
	assert.Zero(t, got.meta.Sequence)
	_, snap, _ := readImage(t, dst)
	assert.Nil(t, snap.Team("during"), "the image holds the state before the write")
	assert.EqualValues(t, 1, s.Meta().Sequence)
}

// TestCaptureRefusals: a closed, failed or busy store, or a cancelled capture,
// refuses and leaves no file.
func TestCaptureRefusals(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, ctx context.Context, s *Store, want error) {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "image.sqlite")
		_, err := s.Capture(ctx, dst)
		require.ErrorIs(t, err, want)
		assert.NoFileExists(t, dst, "a failed capture leaves no file")
	}
	t.Run("closed", func(t *testing.T) {
		t.Parallel()
		s := openStore(t, t.TempDir(), Options{})
		require.NoError(t, s.Close())
		refused(t, t.Context(), s, access.ErrClosed)
	})
	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		s := openStore(t, t.TempDir(), Options{})
		s.beforeCommit = func(tx *sql.Tx) error {
			if err := tx.Rollback(); err != nil {
				return err
			}
			_, err := s.db.SQL().ExecContext(context.Background(), "ALTER TABLE grants RENAME TO lost")
			return err
		}
		_, err := s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
		require.ErrorIs(t, err, ErrFailed)
		refused(t, t.Context(), s, ErrFailed)
	})
	t.Run("busy", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := openStore(t, dir, Options{})
		_, err := s.db.SQL().ExecContext(t.Context(), "PRAGMA busy_timeout = 50")
		require.NoError(t, err)
		other := rawDB(t, dir)
		_, err = other.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
		require.NoError(t, err)
		refused(t, t.Context(), s, ErrBusy)
		_, err = other.ExecContext(t.Context(), "ROLLBACK")
		require.NoError(t, err)
		_, err = s.Capture(t.Context(), filepath.Join(t.TempDir(), "image.sqlite"))
		require.NoError(t, err, "a capture succeeds once the program lets go")
	})
	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		s := openStore(t, t.TempDir(), Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		refused(t, ctx, s, context.Canceled)
	})
}

// alter runs query on the image at path through a connection of its own.
func alter(t *testing.T, path, query string) {
	t.Helper()
	db, err := sqlitedb.Open(t.Context(), path, schema)
	require.NoError(t, err)
	_, err = db.SQL().ExecContext(t.Context(), query)
	require.NoError(t, errors.Join(err, db.Close()))
}

// corruptPage overwrites the first byte of table's root page in the file at path.
func corruptPage(t *testing.T, path, table string) {
	t.Helper()
	var page, size int64
	db := rawFile(t, path)
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT rootpage FROM sqlite_schema WHERE name = ?", table).Scan(&page))
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&size))
	require.NoError(t, db.Close())
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xff}, (page-1)*size)
	require.NoError(t, errors.Join(err, f.Close()))
}

// TestCheckImage passes a captured image without writing to it, and refuses a
// damaged or foreign one with a start's reason, also without writing to it.
func TestCheckImage(t *testing.T) {
	t.Parallel()
	s := openStore(t, t.TempDir(), contractOptions)
	claim(t, s)
	_, _, err := s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{Action: audit.ActionKeyCreated, Subject: "reader"})
	require.NoError(t, err)
	image := filepath.Join(t.TempDir(), "image.sqlite")
	m, err := s.Capture(t.Context(), image)
	require.NoError(t, err)
	before := readFile(t, image)
	img, err := CheckImage(t.Context(), image)
	require.NoError(t, err)
	assert.Equal(t, Image{Meta: m, Initialized: true}, img)
	assert.Equal(t, before, readFile(t, image), "a check never writes")
	assert.NoFileExists(t, image+"-journal")

	for name, tc := range map[string]struct {
		damage func(t *testing.T, path string)
		reason Reason
	}{
		"a corrupt page": {func(t *testing.T, path string) { corruptPage(t, path, "keys") }, IntegrityFailed},
		// Only integrity_check compares an index with its table.
		"an index that disagrees with its table": {func(t *testing.T, path string) {
			alter(t, path, `PRAGMA writable_schema = ON;
				UPDATE sqlite_schema SET sql = 'CREATE INDEX audit_subject ON audit_events(action, seq)' WHERE name = 'audit_subject'`)
		}, IntegrityFailed},
		"a foreign key violation": {func(t *testing.T, path string) {
			alter(t, path, "PRAGMA foreign_keys = OFF; UPDATE keys SET team_id = 'missing'")
		}, IntegrityFailed},
		"a record its rules refuse": {func(t *testing.T, path string) {
			alter(t, path, "UPDATE keys SET expires_at = 253402300800000000")
		}, IntegrityFailed},
		"a newer schema": {func(t *testing.T, path string) { alter(t, path, "PRAGMA user_version = 2") }, SchemaNewer},
		// Images are never migrated, so a lower version can only be a damaged header.
		"an older schema version": {func(t *testing.T, path string) { alter(t, path, "PRAGMA user_version = 0") }, Unreadable},
		"another application":     {func(t *testing.T, path string) { alter(t, path, "PRAGMA application_id = 1") }, Unreadable},
		"junk": {func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte("not a database, just text long enough to have a header"), 0o600))
		}, Unreadable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := copyFile(t, image)
			tc.damage(t, path)
			damaged := readFile(t, path)
			for op, run := range map[string]func(context.Context, string) (Image, error){"CheckImage": CheckImage, "PrepareImage": PrepareImage} {
				_, err := run(t.Context(), path)
				var refusal *Refusal
				require.ErrorAs(t, err, &refusal, op)
				assert.Equal(t, tc.reason, refusal.Reason, op)
			}
			assert.Equal(t, damaged, readFile(t, path), "a refused image is never written")
		})
	}
}

// TestImageOperationsRefuseTheLiveFile: an image operation pointed at the
// live database's file is refused before it opens the file, whatever its case.
func TestImageOperationsRefuseTheLiveFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	claim(t, s)
	live := readFile(t, dbPath(dir))
	upper := filepath.Join(t.TempDir(), "ACCESS.SQLITE")
	require.NoError(t, os.WriteFile(upper, live, 0o644))
	for _, path := range []string{dbPath(dir), upper} {
		for op, run := range map[string]func(context.Context, string) (Image, error){"CheckImage": CheckImage, "PrepareImage": PrepareImage} {
			_, err := run(t.Context(), path)
			require.ErrorIs(t, err, errLiveImage, "%s %s", op, path)
		}
	}
	assert.Equal(t, live, readFile(t, dbPath(dir)), "the live file is untouched")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(upper)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "not even opened")
	}
	_, err := s.CreateTeam(t.Context(), "eng", engineering(), audit.Event{})
	require.NoError(t, err, "the store keeps writing")
}

// restoreFixture is a store with people, grants in every state and a key,
// references issued against it, and an image captured from it.
type restoreFixture struct {
	image             string
	before            Meta
	owner, member     *people.Person
	raw               string
	personRef, keyRef control.CredentialReference
	records           people.Records
	keys              map[string]*keys.VirtualKey
	events            []audit.Event
}

func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	s := openStore(t, t.TempDir(), contractOptions)
	f := restoreFixture{image: filepath.Join(t.TempDir(), "image.sqlite"), owner: claim(t, s)}
	invitation := bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "workspace", Role: "user"})
	var err error
	f.member, err = s.People().AcceptInvitation(t.Context(), invitation.Token, testProof, people.CreatePerson{Username: "member", Name: "Member", Password: testPassword})
	require.NoError(t, err)
	_, err = s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSession, Issuer: installationIssuer})
	require.NoError(t, err)
	bound(t, s, people.GrantRequest{Purpose: people.PurposeInvitation, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{PersonID: f.owner.ID, Revision: f.owner.Revision, Reference: "owner"}})
	recovery, err := s.People().Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeRecovery, TeamID: "workspace", PersonID: f.member.ID, Issuer: installationIssuer})
	require.NoError(t, err)
	require.NoError(t, s.People().RevokeGrant(t.Context(), recovery.ID))
	_, f.raw, err = s.CreateKey(t.Context(), "reader", readerKey(), audit.Event{Action: audit.ActionKeyCreated, Subject: "reader"})
	require.NoError(t, err)
	core := control.NewCore(control.StaticKeySet{}, s, nil, nil, nil)
	f.personRef, err = core.PersonReference(f.owner)
	require.NoError(t, err)
	f.keyRef, err = core.Reference(f.raw)
	require.NoError(t, err)
	f.before, err = s.Capture(t.Context(), f.image)
	require.NoError(t, err)
	f.records, f.keys = s.Snapshot().PeopleState().Records(), maps.Collect(s.Snapshot().Keys())
	f.events = fileEvents(t, f.image)
	return f
}

// changes counts the people with a revision other than before's and the
// grants still pending.
func (f restoreFixture) changes(snap *access.Snapshot) (revised, pending int) {
	now := utils.NowUTC()
	records := snap.PeopleState().Records()
	for id, p := range records.People {
		if p.Revision != f.records.People[id].Revision {
			revised++
		}
	}
	for _, g := range records.Grants {
		if g.UsedAt == nil && g.RevokedAt == nil && now.Before(g.ExpiresAt) {
			pending++
		}
	}
	return revised, pending
}

func TestPrepareImage(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t)
	revised, pending := f.changes(readImageSnapshot(t, f.image))
	require.Equal(t, [2]int{0, 2}, [2]int{revised, pending}, "the fixture has a pending session and invitation")

	prepared, err := PrepareImage(t.Context(), f.image)
	require.NoError(t, err)
	assert.Equal(t, f.before.StoreID, prepared.StoreID, "the store ID is kept")
	assert.NotEqual(t, f.before.Generation, prepared.Generation)
	assert.NotEqual(t, prepared.StoreID, prepared.Generation)
	assert.Equal(t, [2]uint64{f.before.Revision + 1, f.before.Sequence + 1}, [2]uint64{prepared.Revision, prepared.Sequence}, "one commit")
	assert.True(t, prepared.Initialized)
	img, snap, events := readImage(t, f.image)
	assert.Equal(t, prepared, img, "the prepared image passes its check")

	records := snap.PeopleState().Records()
	require.Len(t, records.People, 2)
	for id, p := range records.People {
		before := f.records.People[id]
		assert.NotEqual(t, before.Revision, p.Revision, "%s has a new credential revision", id)
		p.Revision = before.Revision
		assert.Equal(t, before, p, "%s: nothing else changes, the password hash included", id)
	}
	require.Len(t, records.Grants, len(f.records.Grants))
	expired := 0
	for hash, g := range records.Grants {
		before := f.records.Grants[hash]
		if before.UsedAt == nil && before.RevokedAt == nil {
			expired++
			assert.False(t, g.ExpiresAt.After(utils.NowUTC()), "a pending grant expired")
			g.ExpiresAt = before.ExpiresAt
		}
		assert.Equal(t, before, g, "only a pending grant's expiry changes; used and revoked grants are kept")
	}
	assert.Equal(t, 2, expired)
	assert.Equal(t, f.keys, maps.Collect(snap.Keys()), "keys and their hashes are kept")
	require.Len(t, events, len(f.events)+1, "one audit row")
	last := events[len(events)-1]
	assert.Equal(t, audit.ActionRestorePrepared, last.Action)
	assert.Equal(t, audit.ActorLocalOperator, last.Actor)
	assert.Equal(t, prepared.StoreID, last.Subject)
	assert.Equal(t, map[string]any{"generation": prepared.Generation, "restored_generation": f.before.Generation}, last.Metadata)

	// References issued before the restore no longer resolve; the credentials do.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "access"), 0o700))
	require.NoError(t, os.WriteFile(dbPath(dir), readFile(t, f.image), 0o600))
	live := openStore(t, dir, contractOptions)
	core := control.NewCore(control.StaticKeySet{}, live, nil, nil, nil)
	_, err = core.AuthenticateReference(f.personRef)
	require.ErrorIs(t, err, control.ErrInvalidKey, "a person reference from before")
	_, err = core.AuthenticateReference(f.keyRef)
	require.ErrorIs(t, err, control.ErrInvalidKey, "an app-key reference from before")
	_, err = core.Reference(f.raw)
	require.NoError(t, err, "the key itself still signs in")
	owner := live.Snapshot().Person(f.owner.ID)
	_, err = core.PersonReference(owner)
	require.NoError(t, err)
	_, err = live.People().Authenticate(t.Context(), "owner", testPassword)
	require.NoError(t, err, "the password still verifies")
	changed, err := live.People().ChangePassword(t.Context(), owner.ID, owner.Revision, testPassword, "another long sqlstore password")
	require.NoError(t, err, "a password change works")
	assert.NotEqual(t, owner.Revision, changed.Revision)
	member := live.Snapshot().Person(f.member.ID)
	_, err = live.People().RevokeSessions(t.Context(), member.ID, f.member.Revision)
	require.ErrorIs(t, err, people.ErrInvalidCredentials, "the revision from before is stale")
	_, err = live.People().RevokeSessions(t.Context(), member.ID, member.Revision)
	require.NoError(t, err, "session revocation works")

	again, err := PrepareImage(t.Context(), f.image)
	require.NoError(t, err)
	assert.Equal(t, prepared.StoreID, again.StoreID)
	assert.NotEqual(t, prepared.Generation, again.Generation, "each preparation draws a new generation")
}

func readImageSnapshot(t *testing.T, path string) *access.Snapshot {
	t.Helper()
	_, snap, _ := readImage(t, path)
	return snap
}

// ownerOnly requires the file at path to be readable by its owner alone; Windows
// restricts it with ACLs, not mode bits.
func ownerOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), path)
}

func TestImagesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows restricts these files with ACLs, not mode bits")
	}
	t.Parallel()
	dir := t.TempDir()
	s := openStore(t, dir, contractOptions)
	claim(t, s)
	image := filepath.Join(t.TempDir(), "image.sqlite")
	_, err := s.Capture(t.Context(), image)
	require.NoError(t, err)
	ownerOnly(t, image)
	require.NoError(t, os.Chmod(image, 0o644))
	_, err = PrepareImage(t.Context(), image)
	require.NoError(t, err)
	ownerOnly(t, image)
	require.NoError(t, s.Close())
	stopped := filepath.Join(t.TempDir(), "stopped.sqlite")
	_, err = CaptureStopped(t.Context(), dir, stopped)
	require.NoError(t, err)
	ownerOnly(t, stopped)
}

const prepareImageVar = "SQLSTORE_PREPARE_IMAGE"

// TestPrepareChild is TestPrepareImageIsAtomic's child: it prepares the image
// it is given, announcing the start and the end.
func TestPrepareChild(t *testing.T) {
	path := os.Getenv(prepareImageVar)
	if path == "" {
		return
	}
	fmt.Println("preparing")
	_, err := PrepareImage(context.Background(), path)
	require.NoError(t, err)
	fmt.Println("prepared")
}

// TestPrepareImageIsAtomic kills a process while it prepares an image: the
// image is then either as it was or fully prepared, and passes its check.
func TestPrepareImageIsAtomic(t *testing.T) {
	t.Parallel()
	f := newRestoreFixture(t)
	start := func(t *testing.T) (string, *exec.Cmd, *bufio.Scanner) {
		t.Helper()
		path := copyFile(t, f.image)
		child := host.Command(os.Args[0], "-test.run=^TestPrepareChild$")
		child.Env = append(os.Environ(), prepareImageVar+"="+path)
		stdout, err := child.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, child.Start())
		t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
		lines := bufio.NewScanner(stdout)
		for lines.Scan() && lines.Text() != "preparing" {
		}
		return path, child, lines
	}
	// Time one preparation first, so the kills spread across it on any disk.
	_, _, lines := start(t)
	started := utils.Now()
	for lines.Scan() && lines.Text() != "prepared" {
	}
	window := 2 * time.Since(started)
	const kills = 24
	outcomes := map[bool]int{}
	for i := range kills {
		path, child, _ := start(t)
		time.Sleep(rand.N(window))
		require.NoError(t, child.Process.Kill())
		_ = child.Wait()

		img, snap, events := readImage(t, path)
		revised, pending := f.changes(snap)
		prepared := img.Generation != f.before.Generation
		outcomes[prepared]++
		if prepared {
			assert.Equal(t, [2]uint64{f.before.Revision + 1, f.before.Sequence + 1}, [2]uint64{img.Revision, img.Sequence}, "kill %d", i)
			assert.Equal(t, [2]int{len(f.records.People), 0}, [2]int{revised, pending}, "kill %d: every person revised, no grant pending", i)
			assert.Len(t, events, len(f.events)+1, "kill %d", i)
		} else {
			assert.Equal(t, f.before, img.Meta, "kill %d", i)
			assert.Equal(t, [2]int{0, 2}, [2]int{revised, pending}, "kill %d: the image holds part of the preparation", i)
			assert.Len(t, events, len(f.events), "kill %d", i)
		}
	}
	t.Logf("%d kills within %v of the start: %d left the image prepared, %d as it was", kills, window, outcomes[true], outcomes[false])
}

// TestCaptureStopped captures a stopped store, refuses what a start refuses,
// captures a newer release's database, and creates nothing without a store.
func TestCaptureStopped(t *testing.T) {
	t.Parallel()
	stopped := func(t *testing.T) (string, Meta) {
		t.Helper()
		dir := t.TempDir()
		s := openStore(t, dir, contractOptions)
		claim(t, s)
		m := s.Meta()
		require.NoError(t, s.Close())
		return dir, m
	}
	dst := func(t *testing.T) string { return filepath.Join(t.TempDir(), "image.sqlite") }

	t.Run("a stopped store", func(t *testing.T) {
		t.Parallel()
		dir, m := stopped(t)
		before, image := readFile(t, dbPath(dir)), dst(t)
		img, err := CaptureStopped(t.Context(), dir, image)
		require.NoError(t, err)
		assert.Equal(t, Image{Meta: m, Initialized: true}, img)
		checked, _, _ := readImage(t, image)
		assert.Equal(t, img, checked)
		assert.Equal(t, before, readFile(t, dbPath(dir)), "the store is not written")
	})
	t.Run("a journal a crash left", func(t *testing.T) {
		t.Parallel()
		_, crashed, committed := crashedMidCommit(t)
		image := dst(t)
		img, err := CaptureStopped(t.Context(), crashed, image)
		require.NoError(t, err)
		assert.EqualValues(t, 1, img.Sequence)
		assert.Equal(t, []string{"committed"}, subjects(fileEvents(t, image)), "the image holds exactly the committed transactions")
		assert.NoFileExists(t, dbPath(crashed)+"-journal")
		assert.Equal(t, committed, readFile(t, dbPath(crashed)), "recovering the journal is the only change to the store")
	})
	t.Run("no store", func(t *testing.T) {
		t.Parallel()
		dir, image := t.TempDir(), dst(t)
		_, err := CaptureStopped(t.Context(), dir, image)
		require.ErrorIs(t, err, ErrNoStore)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "nothing is created")
		assert.NoFileExists(t, image)
	})
	t.Run("a marker alone", func(t *testing.T) {
		t.Parallel()
		dir, _ := stopped(t)
		require.NoError(t, os.Remove(dbPath(dir)))
		image := dst(t)
		_, err := CaptureStopped(t.Context(), dir, image)
		var refusal *Refusal
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, Missing, refusal.Reason)
		assert.NoFileExists(t, dbPath(dir), "nothing is created")
		assert.NoFileExists(t, image)
	})
	// A newer release's database is captured byte for byte whether or not this
	// release parses its schema, and no image operation calls it damaged.
	for name, tc := range map[string]struct {
		newer    string
		readable bool // its meta table
	}{
		"a newer release's database": {"PRAGMA user_version = 2", true},
		"a newer schema this release cannot parse": {`PRAGMA writable_schema = ON;
			INSERT INTO sqlite_schema (type, name, tbl_name, rootpage, sql) VALUES ('table', 'future', 'future', 0, 'CREATE TABLE future(x FUTURE SYNTAX !!!)');
			PRAGMA user_version = 2; PRAGMA writable_schema = OFF`, false},
		"a newer release's database without a meta table": {"DROP TABLE meta; PRAGMA user_version = 2", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, m := stopped(t)
			alter(t, dbPath(dir), tc.newer)
			requireRefusal(t, dir, SchemaNewer)
			source, image := readFile(t, dbPath(dir)), dst(t)
			img, err := CaptureStopped(t.Context(), dir, image)
			require.NoError(t, err, "a newer database can still be backed up")
			want := Image{Meta: Meta{SchemaVersion: 2}}
			if tc.readable {
				want.Meta, want.SchemaVersion = m, 2
			}
			assert.Equal(t, want, img, "the header, and the meta table when readable")
			assert.Equal(t, source, readFile(t, dbPath(dir)), "the store is not written")
			assert.Equal(t, source, readFile(t, image), "the image is the store byte for byte")
			ownerOnly(t, image)
			for op, run := range map[string]func(context.Context, string) (Image, error){"CheckImage": CheckImage, "PrepareImage": PrepareImage} {
				_, err := run(t.Context(), image)
				var refusal *Refusal
				require.ErrorAs(t, err, &refusal, op)
				assert.Equal(t, SchemaNewer, refusal.Reason, op)
			}
			assert.Equal(t, source, readFile(t, image), "neither operation writes it")
		})
	}
	for name, tc := range map[string]struct {
		damage func(t *testing.T, path string)
		reason Reason
	}{
		"a record its rules refuse": {func(t *testing.T, path string) {
			alter(t, path, "UPDATE teams SET suspended = 1 WHERE id = 'workspace'")
		}, IntegrityFailed},
		// The copy reads the damage before any check runs.
		"a corrupt page":      {func(t *testing.T, path string) { corruptPage(t, path, "people") }, IntegrityFailed},
		"another application": {func(t *testing.T, path string) { alter(t, path, "PRAGMA application_id = 1") }, Unreadable},
		"junk": {func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, []byte("not a database, just text long enough to have a header"), 0o600))
		}, Unreadable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, _ := stopped(t)
			tc.damage(t, dbPath(dir))
			image := dst(t)
			_, err := CaptureStopped(t.Context(), dir, image)
			var refusal *Refusal
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, tc.reason, refusal.Reason)
			requireRefusal(t, dir, tc.reason)
			assert.NoFileExists(t, image, "a refused capture leaves no file")
		})
	}
}

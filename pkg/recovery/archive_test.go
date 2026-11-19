package recovery

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// peopleOptions accepts every issuer: the fixture tests archives, not authority.
var peopleOptions = sqlstore.Options{
	People:                 true,
	ValidateExternalIssuer: func(context.Context, string) error { return nil },
}

func recoveryFixture(t *testing.T) (dir, output, keyOutput string) {
	t.Helper()
	dir = t.TempDir()
	_, prepareErr := templates.PrepareProviders(filepath.Join(dir, "providers"))
	require.NoError(t, prepareErr)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte("node:\n  name: recovery-test\n  port: 9090\n"), 0o600))
	cfg, err := config.ReadExistingNodeConfig(dir)
	require.NoError(t, err)
	apps, err := config.LoadAppsConfig(filepath.Join(dir, "providers"))
	require.NoError(t, err)
	inventory, err := describe(t.Context(), cfg, apps, dir, dir, filepath.Join(t.TempDir(), "providers"), []string{filepath.Join(dir, "models")})
	require.NoError(t, err)
	lock, err := config.LockInstallation(dir)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	require.NoError(t, Save(t.Context(), inventory))
	key, err := config.PrepareCredentialKey(t.Context(), dir, config.CredentialsConfig{})
	require.NoError(t, err)
	defer clear(key)
	vault, err := secrets.Open(filepath.Join(dir, "credentials.enc"), key)
	require.NoError(t, err)
	_, err = vault.Replace(t.Context(), "cloud", 0, map[string]string{"token": "synthetic-provider-secret"})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	accessStore, err := sqlstore.Open(t.Context(), dir, peopleOptions)
	require.NoError(t, err)
	_, err = accessStore.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, _, err = accessStore.CreateKey(t.Context(), "editor", keys.VirtualKey{Name: "Editor", Role: "user", TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	peopleStore := accessStore.People()
	grant, err := peopleStore.Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: "test"}})
	require.NoError(t, err)
	bound, err := peopleStore.Exchange(t.Context(), grant.Token, "test-proof")
	require.NoError(t, err)
	_, err = peopleStore.ClaimOwner(t.Context(), bound.Token, "test-proof", people.CreatePerson{Username: "owner", Name: "Owner", Password: "a long test password"})
	require.NoError(t, err)
	require.NoError(t, accessStore.Close())
	require.NoError(t, quota.SaveSpend(filepath.Join(dir, "spend.json"), &quota.SpendFile{Version: 1, States: map[string]*quota.SpendState{"team:workspace:": {EntityID: "workspace", Kind: quota.ScopeTeam, SpendMicro: 1750000}}}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pairing.txt"), []byte("transient-pairing-proof"), 0o600))
	output = filepath.Join(t.TempDir(), "backup.tar.gz")
	keyOutput = filepath.Join(t.TempDir(), "credentials.key")
	return
}

// issued is what a running coordinator handed out before a backup: the
// owner's session reference, an app key and its reference, at meta.
type issued struct {
	meta         sqlstore.Meta
	session, app control.CredentialReference
	raw          string
}

// issue opens dir's stopped store as a running node would and issues references.
func issue(t *testing.T, dir string) issued {
	t.Helper()
	store, err := sqlstore.Open(t.Context(), dir, peopleOptions)
	require.NoError(t, err)
	var i issued
	_, i.raw, err = store.CreateKey(t.Context(), "probe", keys.VirtualKey{Name: "Probe", Role: "user", TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	core := control.NewCore(control.StaticKeySet{}, store, nil, nil, nil)
	i.session, err = core.PersonReference(store.Snapshot().Owner())
	require.NoError(t, err)
	i.app, err = core.Reference(i.raw)
	require.NoError(t, err)
	i.meta = store.Meta()
	require.NoError(t, store.Close())
	return i
}

// activate places the staged image as docs/recovery.md tells the operator to,
// at the staged configuration's access/access.sqlite, and opens that store.
func activate(t *testing.T, destination string) *sqlstore.Store {
	t.Helper()
	config := filepath.Join(destination, "config")
	require.NoError(t, os.MkdirAll(filepath.Join(config, "access"), 0o700))
	data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(imageName)))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config, "access", "access.sqlite"), data, 0o600))
	store, err := sqlstore.Open(t.Context(), config, peopleOptions)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// requireStaged requires destination to hold a prepared image of want and a
// staging manifest equal to manifest that records it, and returns the image.
func requireStaged(t *testing.T, destination string, manifest Manifest, want StoreMeta) sqlstore.Image {
	t.Helper()
	require.NoFileExists(t, filepath.Join(destination, "INCOMPLETE"))
	data, err := os.ReadFile(filepath.Join(destination, "manifest.json"))
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var staging Manifest
	require.NoError(t, decoder.Decode(&staging))
	require.NotNil(t, staging.Staged)
	require.Equal(t, *manifest.Staged, *staging.Staged)
	require.Equal(t, want, *staging.Store, "the archived store description is kept")
	image := filepath.Join(destination, filepath.FromSlash(imageName))
	content, err := os.ReadFile(image)
	require.NoError(t, err)
	require.Equal(t, digest(content), staging.Staged.ImageSHA256, "the staged digest is the prepared image's")
	img, err := sqlstore.CheckImage(t.Context(), image)
	require.NoError(t, err)
	require.Equal(t, staging.Staged.Generation, img.Generation)
	require.NotEqual(t, want.Generation, img.Generation, "the image is never published as archived")
	require.Equal(t, [2]any{want.StoreID, want.Sequence + 1}, [2]any{img.StoreID, img.Sequence}, "the store ID is kept; preparation is one commit")
	require.WithinDuration(t, utils.NowUTC(), staging.Staged.PreparedAt, time.Minute)
	return img
}

// TestRecoveryRoundTripPreservesAuthorityAndSpend backs up, checks and stages
// an installation with people, keys and teams. The staged image opens as a
// store where the owner signs in with the same password while every session
// and app-key reference issued before does not resolve, and its history
// records the restore. Every image copy lives in a private temporary
// directory that is gone afterwards.
func TestRecoveryRoundTripPreservesAuthorityAndSpend(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	before := issue(t, dir)
	destination := filepath.Join(t.TempDir(), "restored")
	temp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temp)
	}
	manifest, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	for _, path := range []string{output, keyOutput} {
		entries, err := os.ReadDir(filepath.Dir(path))
		require.NoError(t, err)
		require.Len(t, entries, 1, "the output alone, no temporary beside it")
	}
	require.Equal(t, ProfileInstallation, manifest.Profile)
	require.Equal(t, StoreMeta(before.meta), *manifest.Store)
	require.True(t, slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Name == imageName }))
	require.False(t, slices.ContainsFunc(manifest.Files, func(file File) bool {
		return file.Name == "config/private/credentials.key" || file.Name == "config/pairing.txt" || file.Name == "config/installation.lock" || strings.HasPrefix(file.Name, "config/access/")
	}), "the access directory travels only as the image")
	checked, err := Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
	require.Equal(t, len(manifest.Files), len(checked.Files))
	require.Equal(t, manifest.Store, checked.Store)
	require.NotEmpty(t, checked.MissingOptional, "never-installed providers are explicitly reported, not silently omitted")
	restored, err := Restore(t.Context(), output, keyOutput, destination)
	require.NoError(t, err)
	entries, err := os.ReadDir(temp)
	require.NoError(t, err)
	require.Empty(t, entries, "no image copy is left behind")
	img := requireStaged(t, destination, restored, *manifest.Store)
	// The marker travels raw, and preparation keeps the store ID it holds.
	for _, name := range []string{"node.yaml", "spend.json", "access-store.json", "credentials.enc"} {
		before, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		after, err := os.ReadFile(filepath.Join(destination, "config", name))
		require.NoError(t, err)
		require.Equal(t, before, after, name)
	}
	require.NoDirExists(t, filepath.Join(destination, "config", "access"))

	restoredStore := activate(t, destination)
	require.Equal(t, img.Meta, restoredStore.Meta())
	person, err := restoredStore.People().Authenticate(t.Context(), "owner", "a long test password")
	require.NoError(t, err, "the owner signs in with the same password")
	require.Equal(t, "admin", person.Role)
	require.Equal(t, "workspace", person.TeamID)
	require.Equal(t, "workspace", restoredStore.Snapshot().Key("editor").TeamID)
	core := control.NewCore(control.StaticKeySet{}, restoredStore, nil, nil, nil)
	_, err = core.AuthenticateReference(before.session)
	require.ErrorIs(t, err, control.ErrInvalidKey, "the owner's session from before does not resolve")
	_, err = core.AuthenticateReference(before.app)
	require.ErrorIs(t, err, control.ErrInvalidKey, "an app-key reference from before does not resolve")
	_, err = core.Reference(before.raw)
	require.NoError(t, err, "the app key itself still signs in")
	events, _, err := restoredStore.AuditPage(t.Context(), audit.Filter{Action: audit.ActionRestorePrepared}, "", 10)
	require.NoError(t, err)
	require.Len(t, events, 1, "the history records the restore")
	require.Equal(t, restored.Staged.Generation, events[0].Metadata["generation"])

	spend, err := quota.LoadSpend(filepath.Join(destination, "config", "spend.json"))
	require.NoError(t, err)
	require.EqualValues(t, 1750000, spend.States["team:workspace:"].SpendMicro)
	key, err := secrets.LoadKeyFile(filepath.Join(destination, "protection", "credentials.key"))
	require.NoError(t, err)
	defer clear(key)
	vault, err := secrets.Open(filepath.Join(destination, "config", "credentials.enc"), key)
	require.NoError(t, err)
	record, err := vault.Snapshot("cloud")
	require.NoError(t, err)
	require.Equal(t, "synthetic-provider-secret", record.Values["token"])
	require.NoError(t, vault.Close())
	_, err = Restore(t.Context(), output, keyOutput, destination)
	require.ErrorContains(t, err, "new directory")
}

func TestRecoveryRefusesLiveNodeAndInvalidAuthority(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	lock, err := config.LockInstallation(dir)
	require.NoError(t, err)
	_, err = Backup(t.Context(), dir, output, keyOutput)
	require.ErrorContains(t, err, "installation is in use")
	require.NoError(t, lock.Close())
	_, err = os.Stat(output)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	_, err = Check(t.Context(), output, "")
	require.ErrorContains(t, err, "separate protection-key")
	wrong := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(wrong, bytes.Repeat([]byte{3}, 32), 0o600))
	_, err = Check(t.Context(), output, wrong)
	require.ErrorContains(t, err, "does not match")
	files := map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}"), "config/spend.json": []byte("{broken")}
	bad := writeTestBundle(t, files)
	destination := filepath.Join(t.TempDir(), "not-created")
	_, err = Restore(t.Context(), bad, "", destination)
	require.ErrorContains(t, err, "spend snapshot")
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecoveryArchivePathAndIntegrityRefusals(t *testing.T) {
	for _, name := range []string{"../escape", "config/../../escape", "config/C:drive", "config/CON", "config/name.", "config\\escape", "config/private/key", "config/owner.lock", "config/update-pending-confirm.json", "config/update-operation.json", "config/backups/executable", "config/SPEND.JSON", "config/OAuth.json", "config/access/access.sqlite", "config/access/access.sqlite-journal", "config/access/access.sqlite.staged", "config/Access/Access.sqlite-Journal", "config/access/backups/old.tar.gz", "config/access/people.json", "store/other.sqlite", "Store/image.sqlite", "store/IMAGE.sqlite", "access.sqlite"} {
		t.Run(name, func(t *testing.T) {
			archive := writeTestBundle(t, map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}"), name: []byte("unsafe")})
			_, err := Check(t.Context(), archive, "")
			require.Error(t, err)
		})
	}
	archive := writeTestBundle(t, map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}")})
	data, err := os.ReadFile(archive)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(archive, data[:len(data)-5], 0o600))
	_, err = Check(t.Context(), archive, "")
	require.Error(t, err, "gzip truncation must be detected")
}

// TestRecoveryBackupCapturesExactlyTheCommittedState: a coordinator killed
// mid-commit leaves uncommitted pages in the file and a hot journal beside it;
// the bundle's image holds exactly the committed state, recovering the journal
// is the only change to the live file, and the restored node opens the image.
func TestRecoveryBackupCapturesExactlyTheCommittedState(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	committedMeta := issue(t, dir).meta
	dbFile := filepath.Join(dir, "access", "access.sqlite")
	committed, err := os.ReadFile(dbFile)
	require.NoError(t, err)

	// A transaction larger than the page cache writes into the file before COMMIT.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbFile)+"?_pragma=synchronous(OFF)&_pragma=cache_size(10)")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE meta SET value = '999' WHERE key = 'sequence';
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO audit_events (seq, time, action, actor, subject, request_id) SELECT 100000 + i, 0, 'x', 'a', hex(randomblob(512)), '' FROM n`)
	require.NoError(t, err)
	inFlight, err := os.ReadFile(dbFile)
	require.NoError(t, err)
	journal, err := os.ReadFile(dbFile + "-journal")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.NoError(t, db.Close())
	// Put back what the kill would have left on disk.
	require.NoError(t, os.WriteFile(dbFile, inFlight, 0o600))
	require.NoError(t, os.WriteFile(dbFile+"-journal", journal, 0o600))
	require.NotEqual(t, committed, inFlight, "the crash left uncommitted pages in the file")

	manifest, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	require.Equal(t, StoreMeta(committedMeta), *manifest.Store, "the image is at the committed sequence")
	require.NoFileExists(t, dbFile+"-journal", "backup recovered the journal in place")
	live, err := os.ReadFile(dbFile)
	require.NoError(t, err)
	require.Equal(t, committed, live, "recovering the journal is the only change to the live file")
	_, err = Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(t.Context(), output, keyOutput, destination)
	require.NoError(t, err)
	requireStaged(t, destination, restored, *manifest.Store)
	store := activate(t, destination)
	inflight, _, err := store.AuditPage(t.Context(), audit.Filter{Action: "x"}, "", 10)
	require.NoError(t, err)
	require.Empty(t, inflight, "no uncommitted row survives")
	_, err = store.People().Authenticate(t.Context(), "owner", "a long test password")
	require.NoError(t, err)
}

// TestRecoveryBackupCapturesTheAccessStore never reads the access directory
// as raw files, captures a newer release's database, and backs up a node
// without a store with no image.
func TestRecoveryBackupCapturesTheAccessStore(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	for _, name := range []string{"access/access.sqlite.staged", "access/backups/old.tar.gz", "access/people.json"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("not a raw source"), 0o600))
	}
	manifest, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	require.NotNil(t, manifest.Store)
	require.True(t, slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Name == imageName }))
	require.True(t, slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Name == "config/access-store.json" }))
	require.False(t, slices.ContainsFunc(manifest.Files, func(file File) bool { return strings.HasPrefix(file.Name, "config/access/") }))

	// A rolled-back binary still backs up the newer release's database before
	// restoring over it, byte for byte; only that release can check it.
	dir, output, keyOutput = recoveryFixture(t)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "access", "access.sqlite")))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "PRAGMA user_version = 99")
	require.NoError(t, errors.Join(err, db.Close()))
	manifest, err = Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	require.Equal(t, 99, manifest.Store.SchemaVersion)
	live, err := os.ReadFile(filepath.Join(dir, "access", "access.sqlite"))
	require.NoError(t, err)
	image := slices.IndexFunc(manifest.Files, func(file File) bool { return file.Name == imageName })
	require.Equal(t, digest(live), manifest.Files[image].SHA256)
	_, err = Check(t.Context(), output, keyOutput)
	var refusal *sqlstore.Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, sqlstore.SchemaNewer, refusal.Reason)

	// A worker, or a node that never ran as a coordinator, has no store.
	dir, output, keyOutput = recoveryFixture(t)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "access")))
	require.NoError(t, os.Remove(filepath.Join(dir, "access-store.json")))
	manifest, err = Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	require.Nil(t, manifest.Store)
	require.False(t, slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Name == imageName }))
	checked, err := Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
	require.Nil(t, checked.Store)
	destination := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(t.Context(), output, keyOutput, destination)
	require.NoError(t, err)
	require.Nil(t, restored.Staged)
	require.NoFileExists(t, filepath.Join(destination, "INCOMPLETE"))
	require.NoDirExists(t, filepath.Join(destination, "store"))
}

// emptyDir requires dir to hold nothing.
func emptyDir(t *testing.T, dir, msg string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Empty(t, names, msg)
}

// privateTemp points the system's temporary directory at a new empty one.
func privateTemp(t *testing.T) string {
	t.Helper()
	temp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, temp)
	}
	return temp
}

// TestRecoveryBackupRefusalsWriteNothing: a live store that will not open
// stops the backup with its own reason and next action, and leaves no
// archive, no key and no temporary anywhere.
func TestRecoveryBackupRefusalsWriteNothing(t *testing.T) {
	for reason, damage := range map[sqlstore.Reason]func(t *testing.T, database string){
		sqlstore.Missing: func(t *testing.T, database string) { require.NoError(t, os.Remove(database)) },
		sqlstore.Unreadable: func(t *testing.T, database string) {
			require.NoError(t, os.WriteFile(database, []byte("not a database, just text long enough to have a header"), 0o600))
		},
		sqlstore.IntegrityFailed: func(t *testing.T, database string) {
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(database))
			require.NoError(t, err)
			var page, size int64
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT rootpage FROM sqlite_schema WHERE name = 'keys'").Scan(&page))
			require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&size))
			require.NoError(t, db.Close())
			data, err := os.ReadFile(database)
			require.NoError(t, err)
			data[(page-1)*size] = 0xff
			require.NoError(t, os.WriteFile(database, data, 0o600))
		},
	} {
		t.Run(string(reason), func(t *testing.T) {
			dir, output, keyOutput := recoveryFixture(t)
			temp := privateTemp(t)
			damage(t, filepath.Join(dir, "access", "access.sqlite"))
			_, err := Backup(t.Context(), dir, output, keyOutput)
			var refusal *sqlstore.Refusal
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, reason, refusal.Reason)
			require.Contains(t, err.Error(), refusal.NextAction(), "the live store keeps its own advice")
			emptyDir(t, filepath.Dir(output), "no archive and no temporary beside it")
			emptyDir(t, filepath.Dir(keyOutput), "no key and no temporary beside it")
			emptyDir(t, temp, "nothing in the system's temporary directory")
		})
	}
}

// TestRecoveryFailedBackupLeavesNoOutput: a bundle that fails while it is
// written leaves no file; an existing archive or key is refused and kept, and
// a refused archive takes its already published key with it.
func TestRecoveryFailedBackupLeavesNoOutput(t *testing.T) {
	sources := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sources, "node.yaml"), []byte("node: {port: 9090}"), 0o600))
	output := filepath.Join(t.TempDir(), "backup.tar.gz")
	changed := Manifest{Version: 1, Profile: ProfileInstallation, Inventory: &Inventory{Version: 1}, Files: []File{{Name: "config/node.yaml", Size: 18, SHA256: digest([]byte("other content, same")), root: sources, source: "node.yaml"}}}
	require.ErrorContains(t, writeBundle(t.Context(), changed, output), "source changed")
	require.NoFileExists(t, output, "a failed bundle leaves no partial output")

	for _, existing := range []string{"archive", "key"} {
		t.Run("an existing "+existing, func(t *testing.T) {
			dir, output, keyOutput := recoveryFixture(t)
			kept := map[string]string{"archive": output, "key": keyOutput}[existing]
			require.NoError(t, os.WriteFile(kept, []byte("an earlier backup"), 0o600))
			_, err := Backup(t.Context(), dir, output, keyOutput)
			require.ErrorIs(t, err, fs.ErrExist)
			data, err := os.ReadFile(kept)
			require.NoError(t, err)
			require.Equal(t, "an earlier backup", string(data), "never replaced")
			for _, path := range []string{output, keyOutput} {
				entries, err := os.ReadDir(filepath.Dir(path))
				require.NoError(t, err)
				if path == kept {
					require.Len(t, entries, 1, "only the earlier file, no temporary")
				} else {
					require.Empty(t, entries, "no new file and no temporary")
				}
			}
		})
	}
}

// TestRecoveryRefusesLeftovers: what a killed backup leaves carries a .tmp
// suffix, and check and restore refuse it as an archive or a key.
func TestRecoveryRefusesLeftovers(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	_, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	leftover := func(path string) string {
		copied := filepath.Join(t.TempDir(), "leftover.tmp")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(copied, data, 0o600))
		return copied
	}
	_, err = Check(t.Context(), leftover(output), keyOutput)
	require.ErrorContains(t, err, "leftover of an interrupted backup")
	_, err = Check(t.Context(), output, leftover(keyOutput))
	require.ErrorContains(t, err, "leftover of an interrupted backup")
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(t.Context(), leftover(output), keyOutput, destination)
	require.ErrorContains(t, err, "leftover of an interrupted backup")
	require.NoDirExists(t, destination)
}

func writeTestBundle(t *testing.T, files map[string][]byte, edits ...func(*Manifest)) string {
	t.Helper()
	manifest := Manifest{Version: 1, Profile: ProfileInstallation, Inventory: &Inventory{Version: 1}}
	for name, data := range files {
		manifest.Files = append(manifest.Files, File{Name: name, Size: int64(len(data)), SHA256: digest(data)})
	}
	for _, edit := range edits {
		edit(&manifest)
	}
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	write := func(name string, data []byte) {
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0o600}))
		_, err := writer.Write(data)
		require.NoError(t, err)
	}
	write("manifest.json", encoded)
	for _, file := range manifest.Files {
		write(file.Name, files[file.Name])
	}
	require.NoError(t, writer.Close())
	require.NoError(t, compressed.Close())
	name := filepath.Join(t.TempDir(), "backup.tar.gz")
	require.NoError(t, os.WriteFile(name, buffer.Bytes(), 0o600))
	return name
}

func TestRecoveryRefusedPreflightDoesNotCreateState(t *testing.T) {
	for _, existing := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "installation")
		if existing {
			require.NoError(t, os.Mkdir(dir, 0o700))
		}
		_, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup.tar.gz"), "")
		require.Error(t, err)
		if existing {
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		} else {
			_, err := os.Stat(dir)
			require.ErrorIs(t, err, os.ErrNotExist)
		}
	}
	dir, output, keyOutput := recoveryFixture(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "installation.lock")))
	_, err := Backup(t.Context(), dir, output, keyOutput)
	require.ErrorContains(t, err, "missing installation lock")
	_, err = os.Stat(filepath.Join(dir, "installation.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecoveryRefusesStorageDeclarationDrift(t *testing.T) {
	for _, change := range []string{"provider root", "model path", "provider data"} {
		t.Run(change, func(t *testing.T) {
			dir, output, keyOutput := recoveryFixture(t)
			file := filepath.Join(dir, "node.yaml")
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			switch change {
			case "provider root":
				data = append(data, []byte("providers: {install_dir: /new/provider-root}\n")...)
			case "model path":
				data = append(data, []byte("models: {cache: /new/models}\n")...)
			case "provider data":
				file = filepath.Join(dir, "providers", "on-demand", "comfyui", "config.yaml")
				data, err = os.ReadFile(file)
				require.NoError(t, err)
				require.Contains(t, string(data), "data_dir: data")
				data = []byte(strings.Replace(string(data), "data_dir: data", "data_dir: workflows", 1))
			}
			require.NoError(t, os.WriteFile(file, data, 0o600))
			_, err = Backup(t.Context(), dir, output, keyOutput)
			require.ErrorContains(t, err, "storage configuration changed")
			_, err = os.Stat(output)
			require.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(keyOutput)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRecoveryRejectsLinkedProviderAncestor(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	inventory, err := Read(t.Context(), dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(inventory.ProviderDir, 0o700))
	external := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(external, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(external, "data", "workflow.json"), []byte("external workflow"), 0o600))
	if err := os.Symlink(external, filepath.Join(inventory.ProviderDir, "comfyui")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = Backup(t.Context(), dir, output, keyOutput)
	require.Error(t, err, "a linked provider parent must never be traversed")
	_, err = os.Stat(output)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecoveryExcludesModelStorageAndPendingUpdates(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	inventory, err := Read(t.Context(), dir)
	require.NoError(t, err)
	models := filepath.Join(dir, "custom-weights")
	relativeModels := filepath.Join(dir, "relative-weights")
	nodeData, err := os.ReadFile(filepath.Join(dir, "node.yaml"))
	require.NoError(t, err)
	nodeData = append(nodeData, []byte("models:\n  cache: '"+filepath.ToSlash(models)+"'\n  shared: relative-weights\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), nodeData, 0o600))
	cfg, err := config.ReadExistingNodeConfig(dir)
	require.NoError(t, err)
	apps, err := config.LoadAppsConfig(filepath.Join(dir, "providers"))
	require.NoError(t, err)
	inventory, err = describe(t.Context(), cfg, apps, dir, dir, inventory.ProviderDir, []string{models, relativeModels})
	require.NoError(t, err)
	require.NoError(t, Save(t.Context(), inventory))
	for _, name := range []string{"custom-weights/large.safetensors", "relative-weights/model.gguf", "backups/zzrouter-node", "update-pending-confirm.json", "update-operation.json", "update/request.json"} {
		target := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
		require.NoError(t, os.WriteFile(target, []byte("excluded"), 0o600))
	}
	for _, name := range []string{"comfyui/data/workflow.json", "comfyui/data/models/large.safetensors"} {
		target := filepath.Join(inventory.ProviderDir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
		require.NoError(t, os.WriteFile(target, []byte("provider data"), 0o600))
	}
	manifest, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	for _, file := range manifest.Files {
		require.NotContains(t, file.Name, "weights/")
		require.NotContains(t, file.Name, "/models/")
		require.NotContains(t, file.Name, "backups/")
		require.NotContains(t, file.Name, "update-")
		require.NotContains(t, file.Name, "update/")
	}
	require.True(t, slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Name == "providers/comfyui/data/workflow.json" }))
	_, err = Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
}

func TestRecoveryRejectsCaseAliasedDirectories(t *testing.T) {
	archive := writeTestBundle(t, map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}"), "config/Custom/a": []byte("a"), "config/custom/b": []byte("b")})
	_, err := Check(t.Context(), archive, "")
	require.ErrorContains(t, err, "case alias")
}

func TestRecoveryKeepsKeyCustodySeparateAcrossPathSpellings(t *testing.T) {
	dir, _, _ := recoveryFixture(t)
	outDir := t.TempDir()
	t.Chdir(outDir)
	_, err := Backup(t.Context(), dir, "backup.tar.gz", filepath.Join(outDir, "key"))
	require.ErrorContains(t, err, "separate directory")
	_, err = os.Stat(filepath.Join(outDir, "key"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecoveryRefusesDisappearingRecordedProviderStorage(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	inventory, err := Read(t.Context(), dir)
	require.NoError(t, err)
	data := filepath.Join(inventory.ProviderDir, "comfyui", "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(data, "workflow.json"), []byte("saved workflow"), 0o600))
	// Capture the same declarations after the provider data exists, as on startup.
	inventory, err = Read(t.Context(), dir)
	require.NoError(t, err)
	require.NoError(t, Save(t.Context(), inventory))
	require.NoError(t, os.Rename(inventory.ProviderDir, inventory.ProviderDir+"-unavailable"))
	_, err = Backup(t.Context(), dir, output, keyOutput)
	require.ErrorContains(t, err, "previously present recovery source is unavailable")
	_, err = os.Stat(output)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRecoveryExcludesProtectionKeyAliasesAndReservedCaseVariants(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	require.NoError(t, os.Link(filepath.Join(dir, "private", "credentials.key"), filepath.Join(dir, "key-alias")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "BACKUPS"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "BACKUPS", "old-node"), []byte("executable backup"), 0o600))
	manifest, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	for _, file := range manifest.Files {
		require.NotEqual(t, "config/key-alias", file.Name)
		require.NotContains(t, strings.ToLower(file.Name), "backups/")
	}
	_, err = Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
}

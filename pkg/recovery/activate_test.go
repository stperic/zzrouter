package recovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/sqlitedb"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// stageDatabase stages a database bundle of dir's store as it is now, as an
// online backup and recovery restore would.
func stageDatabase(t *testing.T, dir string) (string, Manifest) {
	t.Helper()
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	image := filepath.Join(t.TempDir(), "image.sqlite")
	meta, err := store.Capture(t.Context(), image)
	require.NoError(t, errors.Join(err, store.Close()))
	staging := filepath.Join(t.TempDir(), "staged")
	manifest, err := Restore(t.Context(), databaseBundle(t, image, StoreMeta(meta)), "", staging)
	require.NoError(t, err)
	return staging, manifest
}

// addKey creates the key id in dir's stopped store.
func addKey(t *testing.T, dir, id string) string {
	t.Helper()
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	_, raw, err := store.CreateKey(t.Context(), id, keys.VirtualKey{Name: id, Role: "user", TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, errors.Join(err, store.Close()))
	return raw
}

// activationFixture is a stopped installation that started once, holding a
// store with a team and a key, and the staging directory of a database bundle
// of that store; a key created after the backup is what activation replaces.
func activationFixture(t *testing.T) (dir, staging string, manifest Manifest) {
	t.Helper()
	dir = t.TempDir()
	lock, err := config.LockInstallation(dir)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	_, err = store.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, errors.Join(err, store.Close()))
	addKey(t, dir, "editor")
	staging, manifest = stageDatabase(t, dir)
	addKey(t, dir, "late")
	return dir, staging, manifest
}

// tree maps every entry under root to its content's digest, "" for a directory.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || d.IsDir() {
			entries[rel] = ""
			return err
		}
		data, err := os.ReadFile(path)
		entries[rel] = digest(data)
		return err
	}))
	return entries
}

// copyTree copies the tree at root into a new directory and returns it.
func copyTree(t *testing.T, root string) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), filepath.Base(root))
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o600)
	}))
	return to
}

// storeFiles reads dir's database, journal, staged copy and marker, "" for an
// absent one.
func storeFiles(t *testing.T, dir string) [4]string {
	t.Helper()
	var files [4]string
	for i, name := range []string{"access/access.sqlite", "access/access.sqlite-journal", "access/access.sqlite.staged", sqlstore.MarkerName} {
		if data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
			files[i] = digest(data)
		} else {
			require.ErrorIs(t, err, fs.ErrNotExist)
		}
	}
	return files
}

func requireRefusal(t *testing.T, err error, reason Reason) *Refusal {
	t.Helper()
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, reason, refusal.Reason, "%v", err)
	require.NotEmpty(t, refusal.NextAction)
	return refusal
}

// requireActivated requires dir to hold the store the staging manifest
// describes, opening and checking as a store, with the staging directory gone,
// no activation pending and before kept in the rollback directory.
func requireActivated(t *testing.T, dir, staging string, manifest Manifest, before [4]string) {
	t.Helper()
	require.NoDirExists(t, staging)
	require.NoError(t, PendingActivation(dir))
	files := storeFiles(t, dir)
	require.Equal(t, manifest.Staged.ImageSHA256, files[0], "the prepared image is the database")
	require.Equal(t, [2]string{}, [2]string{files[1], files[2]}, "no journal and no staged copy")
	copied := filepath.Join(t.TempDir(), "copy.sqlite")
	require.NoError(t, os.WriteFile(copied, readBytes(t, filepath.Join(dir, "access", "access.sqlite")), 0o600))
	img, err := sqlstore.CheckImage(t.Context(), copied)
	require.NoError(t, err)
	require.Equal(t, manifest.Staged.Generation, img.Generation)
	store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
	require.NoError(t, err)
	require.Equal(t, img.Meta, store.Meta())
	require.NotNil(t, store.Snapshot().Key("editor"))
	require.Nil(t, store.Snapshot().Key("late"), "the key created after the backup is gone")
	require.NoError(t, store.Close())
	require.Equal(t, before[0], digest(readBytes(t, filepath.Join(dir, rollbackDir, img.Generation, "store", "access.sqlite"))))
	require.Equal(t, before[3], digest(readBytes(t, filepath.Join(dir, rollbackDir, img.Generation, "store", sqlstore.MarkerName))))
}

// TestActivationPublishesTheStagedDatabase activates a staged database bundle
// into the installation it came from: the staging directory is gone, the
// rollback directory keeps the replaced database and is listed, and at the
// first start the owner's password and a raw app key work while sessions and
// app-key references issued before do not resolve, the history holds the
// restore, and the spend of a key the image lacks is forgotten.
func TestActivationPublishesTheStagedDatabase(t *testing.T) {
	dir, _, _ := recoveryFixture(t)
	before := issue(t, dir)
	staging, manifest := stageDatabase(t, dir)
	addKey(t, dir, "late")
	spendPath := filepath.Join(dir, "spend.json")
	lateScope, probeScope := quota.SpendScope{Kind: quota.ScopeKey, EntityID: "late"}, quota.SpendScope{Kind: quota.ScopeKey, EntityID: "probe"}
	spend, err := quota.LoadSpend(spendPath)
	require.NoError(t, err)
	spend.States[lateScope.String()] = &quota.SpendState{Kind: quota.ScopeKey, EntityID: "late", SpendMicro: 5}
	spend.States[probeScope.String()] = &quota.SpendState{Kind: quota.ScopeKey, EntityID: "probe", SpendMicro: 7}
	require.NoError(t, quota.SaveSpend(spendPath, spend))
	previous := storeFiles(t, dir)

	a, err := Activate(t.Context(), dir, staging, ActivateOptions{})
	require.NoError(t, err)
	gen := manifest.Staged.Generation
	require.Equal(t, Activation{Profile: ProfileDatabase, Store: a.Store, RollbackID: gen, Replaced: []string{"store"}}, a)
	require.Equal(t, gen, a.Store.Generation)
	require.NoDirExists(t, staging, "D4: the staging directory is removed")
	rollbacks, err := Rollbacks(dir)
	require.NoError(t, err)
	require.Len(t, rollbacks, 1)
	require.Equal(t, gen, rollbacks[0].ID)
	require.WithinDuration(t, utils.NowUTC(), rollbacks[0].CreatedAt, time.Minute)
	require.Positive(t, rollbacks[0].SizeBytes)
	if runtime.GOOS != "windows" {
		for _, path := range []string{filepath.Join(dir, rollbackDir), filepath.Join(dir, rollbackDir, gen), filepath.Join(dir, rollbackDir, gen, "store")} {
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "%s is owner-only", path)
		}
	}
	require.Equal(t, previous[0], digest(readBytes(t, filepath.Join(dir, rollbackDir, gen, "store", "access.sqlite"))))

	// The first start opens the store and reconciles spend.
	store, err := sqlstore.Open(t.Context(), dir, peopleOptions)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.Equal(t, gen, store.Meta().Generation)
	require.Nil(t, store.Snapshot().Key("late"))
	_, err = store.People().Authenticate(t.Context(), "owner", "a long test password")
	require.NoError(t, err, "the owner signs in with the same password")
	enforcer := quota.NewEnforcer(quota.NewRateLimiter(), quota.NewConcurrencyLimiter(), quota.NewSpendTracker(spendPath))
	core := control.NewCore(control.StaticKeySet{}, store, nil, enforcer, nil)
	t.Cleanup(func() { core.Stop(context.Background()) })
	_, err = core.AuthenticateReference(before.session)
	require.ErrorIs(t, err, control.ErrInvalidKey, "a session from before does not resolve")
	_, err = core.AuthenticateReference(before.app)
	require.ErrorIs(t, err, control.ErrInvalidKey, "an app-key reference from before does not resolve")
	_, err = core.Reference(before.raw)
	require.NoError(t, err, "the raw app key still authenticates")
	events, _, err := store.AuditPage(t.Context(), audit.Filter{Action: audit.ActionRestorePrepared}, "", 10)
	require.NoError(t, err)
	require.Len(t, events, 1, "the history records the restore")
	spend, err = quota.LoadSpend(spendPath)
	require.NoError(t, err)
	require.NotContains(t, spend.States, lateScope.String(), "the spend of a key the image lacks is gone")
	require.EqualValues(t, 7, spend.States[probeScope.String()].SpendMicro)
	require.EqualValues(t, 1750000, spend.States["team:workspace:"].SpendMicro)
}

// TestActivationRefusalsWriteNothing: each refusal carries its closed reason
// and next action, and leaves the installation and the staging directory
// byte-identical, as does every dry run.
func TestActivationRefusalsWriteNothing(t *testing.T) {
	editManifest := func(edit func(*Manifest)) func(t *testing.T, dir, staging string) {
		return func(t *testing.T, _, staging string) {
			path := filepath.Join(staging, "manifest.json")
			var m Manifest
			require.NoError(t, json.Unmarshal(readBytes(t, path), &m))
			edit(&m)
			data, err := json.Marshal(m)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
		}
	}
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, dir, staging string)
		reason  Reason
		store   sqlstore.Reason // a refusal of the image
		detail  string
	}{
		"INCOMPLETE": {prepare: func(t *testing.T, _, staging string) {
			require.NoError(t, os.WriteFile(filepath.Join(staging, incompleteName), nil, 0o600))
		}, reason: StagingInvalid, detail: "marked INCOMPLETE"},
		"an extra file": {prepare: func(t *testing.T, _, staging string) {
			require.NoError(t, os.WriteFile(filepath.Join(staging, "store", "notes.txt"), nil, 0o600))
		}, reason: StagingInvalid, detail: "entries other than"},
		"no staging manifest": {prepare: func(t *testing.T, _, staging string) {
			require.NoError(t, os.Remove(filepath.Join(staging, "manifest.json")))
		}, reason: StagingInvalid, detail: "no staging manifest"},
		"another profile":       {prepare: editManifest(func(m *Manifest) { m.Profile = "handoff" }), reason: StagingInvalid, detail: `profile "handoff"`},
		"an unstaged manifest":  {prepare: editManifest(func(m *Manifest) { m.Staged = nil }), reason: StagingInvalid, detail: "not a staged database bundle"},
		"an image digest":       {prepare: editManifest(func(m *Manifest) { m.Staged.ImageSHA256 = digest([]byte("another")) }), reason: StagingInvalid, detail: "SHA-256"},
		"a generation mismatch": {prepare: editManifest(func(m *Manifest) { m.Staged.Generation = "another" }), reason: StagingInvalid, detail: "generation"},
		"a newer schema": {prepare: func(t *testing.T, dir, staging string) {
			image := stagedImage(staging)
			require.NoError(t, os.WriteFile(image, readBytes(t, alterImage(t, image, "PRAGMA user_version = 99")), 0o600))
			editManifest(func(m *Manifest) { m.Staged.ImageSHA256 = digest(readBytes(t, image)) })(t, dir, staging)
		}, store: sqlstore.SchemaNewer},
		"another store": {prepare: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.RemoveAll(filepath.Join(dir, "access")))
			require.NoError(t, os.Remove(filepath.Join(dir, sqlstore.MarkerName)))
			store, err := sqlstore.Open(t.Context(), dir, sqlstore.Options{})
			require.NoError(t, err)
			require.NoError(t, store.Close())
		}, reason: ForeignStore},
		"an incomplete activation of another directory": {prepare: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, activationFile), []byte(`{"version":1,"profile":"database","staging":"/elsewhere","generation":"g"}`), 0o600))
		}, reason: ActivationIncomplete},
		"an activated directory": {prepare: func(t *testing.T, dir, staging string) {
			var m Manifest
			require.NoError(t, json.Unmarshal(readBytes(t, filepath.Join(staging, "manifest.json")), &m))
			require.NoError(t, os.MkdirAll(filepath.Join(dir, rollbackDir, m.Staged.Generation), 0o700))
		}, reason: AlreadyActivated},
	} {
		t.Run(name, func(t *testing.T) {
			dir, staging, _ := activationFixture(t)
			tc.prepare(t, dir, staging)
			installation, staged := tree(t, dir), tree(t, staging)
			for _, o := range []ActivateOptions{{DryRun: true}, {}} {
				_, err := Activate(t.Context(), dir, staging, o)
				if tc.store != "" {
					requireArchiveRefusal(t, err, tc.store)
				} else {
					require.Contains(t, requireRefusal(t, err, tc.reason).Detail, tc.detail)
				}
				require.Equal(t, installation, tree(t, dir), "the installation is unchanged")
				require.Equal(t, staged, tree(t, staging), "the staging directory is unchanged")
			}
		})
	}
	t.Run("a running node", func(t *testing.T) {
		dir, staging, _ := activationFixture(t)
		lock, err := config.LockInstallation(dir)
		require.NoError(t, err)
		defer func() { _ = lock.Close() }()
		installation, staged := tree(t, dir), tree(t, staging)
		_, err = Activate(t.Context(), dir, staging, ActivateOptions{})
		requireRefusal(t, err, InUse)
		require.Equal(t, installation, tree(t, dir))
		require.Equal(t, staged, tree(t, staging))
	})
	t.Run("an installation that never started", func(t *testing.T) {
		dir, staging, _ := activationFixture(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "installation.lock")))
		installation := tree(t, dir)
		_, err := Activate(t.Context(), dir, staging, ActivateOptions{})
		require.ErrorContains(t, err, "missing installation lock")
		require.Equal(t, installation, tree(t, dir), "no lock file is created")
	})
	t.Run("a rollback with nothing to roll back", func(t *testing.T) {
		dir, staging, _ := activationFixture(t)
		installation := tree(t, dir)
		_, err := Activate(t.Context(), dir, staging, ActivateOptions{Rollback: true})
		require.Contains(t, requireRefusal(t, err, NoActivation).NextAction, "by hand")
		require.Equal(t, installation, tree(t, dir))
	})
}

// TestActivationDryRunReports: a dry run reports what the activation would
// replace and changes nothing; a fresh installation admits another store.
func TestActivationDryRunReports(t *testing.T) {
	dir, staging, manifest := activationFixture(t)
	installation, staged := tree(t, dir), tree(t, staging)
	a, err := Activate(t.Context(), dir, staging, ActivateOptions{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, manifest.Staged.Generation, a.RollbackID)
	require.Equal(t, manifest.Staged.Generation, a.Store.Generation)
	require.Equal(t, []string{"store"}, a.Replaced)
	require.Equal(t, installation, tree(t, dir))
	require.Equal(t, staged, tree(t, staging))

	fresh := t.TempDir()
	lock, err := config.LockInstallation(fresh)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	a, err = Activate(t.Context(), fresh, staging, ActivateOptions{})
	require.NoError(t, err)
	require.Empty(t, a.Replaced, "a fresh installation had nothing to replace")
	id, err := sqlstore.MarkerStoreID(readBytes(t, filepath.Join(fresh, sqlstore.MarkerName)))
	require.NoError(t, err)
	require.Equal(t, manifest.Store.StoreID, id)
}

const (
	childDir      = "RECOVERY_ACTIVATE_DIR"
	childStaging  = "RECOVERY_ACTIVATE_STAGING"
	childStop     = "RECOVERY_ACTIVATE_STOP"
	childRollback = "RECOVERY_ACTIVATE_ROLLBACK"
)

// TestActivateChild is TestActivationSurvivesAKillAtEveryStep's child: it
// activates, or rolls back, and at the durable step its parent numbers stops
// until it is killed.
func TestActivateChild(t *testing.T) {
	dir := os.Getenv(childDir)
	if dir == "" {
		return
	}
	stop, err := strconv.Atoi(os.Getenv(childStop))
	require.NoError(t, err)
	steps := 0
	ctx := sqlitedb.WithStep(context.Background(), func(step string) {
		if steps++; steps == stop {
			fmt.Println("stopped at " + step)
			testproc.AwaitOwner() // killed here, or ended with its test
			os.Exit(1)
		}
	})
	_, err = Activate(ctx, dir, os.Getenv(childStaging), ActivateOptions{Rollback: os.Getenv(childRollback) != ""})
	require.NoError(t, err)
}

// killAt runs an activation, or a rollback, in a child process and kills it
// at its stop-th durable step, which it returns.
func killAt(t *testing.T, dir, staging string, stop int, rollback bool) string {
	t.Helper()
	child := host.Command(os.Args[0], "-test.run=^TestActivateChild$", "-test.count=1")
	owner, pid := testproc.OwnerEnv()
	child.Env = append(os.Environ(), childDir+"="+dir, childStaging+"="+staging, childStop+"="+strconv.Itoa(stop), owner+"="+pid)
	if rollback {
		child.Env = append(child.Env, childRollback+"=1")
	}
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if step, ok := strings.CutPrefix(lines.Text(), "stopped at "); ok {
			require.NoError(t, child.Process.Kill())
			_ = child.Wait()
			return step
		}
	}
	t.Fatalf("the child never reached step %d", stop)
	return ""
}

// stepsOf runs an activation, or a rollback, of copies of dir and staging and
// returns its durable steps.
func stepsOf(t *testing.T, dir, staging string, rollback bool) []string {
	t.Helper()
	var steps []string
	ctx := sqlitedb.WithStep(t.Context(), func(step string) { steps = append(steps, step) })
	_, err := Activate(ctx, copyTree(t, dir), staging, ActivateOptions{Rollback: rollback})
	require.NoError(t, err)
	return steps
}

// TestActivationSurvivesAKillAtEveryStep kills a real process at each durable
// step of an activation. Until the activation marker is removed, a start
// refuses with activation_incomplete; a rollback then restores the database
// and marker byte for byte, and a rerun instead completes the activation.
// Killed at each step of that rollback, a rerun of the rollback completes it.
func TestActivationSurvivesAKillAtEveryStep(t *testing.T) {
	dir, staging, manifest := activationFixture(t)
	before := storeFiles(t, dir)
	steps := stepsOf(t, dir, copyTree(t, staging), false)
	require.Equal(t, []string{"activation marker", "rollback record", "staged", "kept record", "kept access.sqlite", "kept access-store.json", "renamed", "store marker", "activation marker removed"}, steps)
	for k, step := range steps {
		t.Run(step, func(t *testing.T) {
			killed, staged := copyTree(t, dir), copyTree(t, staging)
			require.Equal(t, step, killAt(t, killed, staged, k+1, false))
			if k == len(steps)-1 {
				require.NoError(t, PendingActivation(killed), "the activation finished")
				_, err := Activate(t.Context(), killed, staged, ActivateOptions{})
				requireRefusal(t, err, AlreadyActivated)
				require.NoError(t, os.RemoveAll(staged))
				requireActivated(t, killed, staged, manifest, before)
				return
			}
			refusal := requireRefusal(t, PendingActivation(killed), ActivationIncomplete)
			require.Contains(t, refusal.NextAction, "zzrouter-node recovery activate "+staged+" --dir "+killed)
			undone := copyTree(t, killed)
			_, err := Activate(t.Context(), undone, staged, ActivateOptions{Rollback: true})
			require.NoError(t, err)
			require.Equal(t, before, storeFiles(t, undone), "the database and marker are as they were")
			require.NoError(t, PendingActivation(undone))
			require.NoDirExists(t, filepath.Join(undone, rollbackDir, manifest.Staged.Generation))
			_, err = Activate(t.Context(), killed, staged, ActivateOptions{})
			require.NoError(t, err, "a rerun completes the activation")
			requireActivated(t, killed, staged, manifest, before)
		})
	}

	// Every target replaced, the marker still there: the deepest rollback.
	replaced := copyTree(t, dir)
	require.Equal(t, "store marker", killAt(t, replaced, staging, len(steps)-1, false))
	undo := stepsOf(t, replaced, staging, true)
	require.Equal(t, []string{"restored access.sqlite", "restored access.sqlite-journal", "restored access-store.json", "rollback directory removed", "activation marker removed"}, undo)
	for j, step := range undo {
		t.Run("rollback/"+step, func(t *testing.T) {
			killed := copyTree(t, replaced)
			require.Equal(t, step, killAt(t, killed, staging, j+1, true))
			if j < len(undo)-1 {
				requireRefusal(t, PendingActivation(killed), ActivationIncomplete)
			}
			_, err := Activate(t.Context(), killed, staging, ActivateOptions{Rollback: true})
			if j == len(undo)-1 {
				requireRefusal(t, err, NoActivation)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, before, storeFiles(t, killed))
			require.NoError(t, PendingActivation(killed))
		})
	}
}

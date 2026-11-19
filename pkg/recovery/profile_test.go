package recovery

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/require"
)

// capturedImage captures a running store holding a team and a key into a
// file of its own, as an online backup does.
func capturedImage(t *testing.T) (string, StoreMeta) {
	t.Helper()
	store, err := sqlstore.Open(t.Context(), t.TempDir(), sqlstore.Options{})
	require.NoError(t, err)
	_, err = store.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, _, err = store.CreateKey(t.Context(), "editor", keys.VirtualKey{Name: "Editor", Role: "user", TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	image := filepath.Join(t.TempDir(), "image.sqlite")
	meta, err := store.Capture(t.Context(), image)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	return image, StoreMeta(meta)
}

// databaseBundle writes a database bundle of the image at path through the
// production writer.
func databaseBundle(t *testing.T, path string, meta StoreMeta) string {
	t.Helper()
	size, sum, err := hashFile(t.Context(), filepath.Dir(path), filepath.Base(path))
	require.NoError(t, err)
	manifest := Manifest{Version: 1, Profile: ProfileDatabase, Created: utils.NowUTC(), Store: &meta,
		Files: []File{{Name: imageName, Size: size, SHA256: sum, root: filepath.Dir(path), source: filepath.Base(path)}}}
	output := filepath.Join(t.TempDir(), "database.tar.gz")
	require.NoError(t, writeBundle(t.Context(), manifest, output))
	return output
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// alterImage runs query on a copy of the image at path and returns the copy.
func alterImage(t *testing.T, path, query string) string {
	t.Helper()
	altered := filepath.Join(t.TempDir(), "image.sqlite")
	require.NoError(t, os.WriteFile(altered, readBytes(t, path), 0o600))
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(altered))
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), query)
	require.NoError(t, errors.Join(err, db.Close()))
	return altered
}

// TestRecoveryDatabaseProfile checks and stages a bundle holding the image
// alone: the staging directory holds the prepared image and its manifest only.
func TestRecoveryDatabaseProfile(t *testing.T) {
	image, meta := capturedImage(t)
	archive := databaseBundle(t, image, meta)
	checked, err := Check(t.Context(), archive, "")
	require.NoError(t, err)
	require.Equal(t, ProfileDatabase, checked.Profile)
	require.Nil(t, checked.Inventory)
	require.Equal(t, meta, *checked.Store)

	destination := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(t.Context(), archive, "", destination)
	require.NoError(t, err)
	requireStaged(t, destination, restored, meta)
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"manifest.json", "store"}, names)
	// Placed as a store's database, the prepared image holds the records.
	live := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(live, "access"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "access", "access.sqlite"), readBytes(t, filepath.Join(destination, filepath.FromSlash(imageName))), 0o600))
	store, err := sqlstore.Open(t.Context(), live, sqlstore.Options{})
	require.NoError(t, err)
	require.Equal(t, "workspace", store.Snapshot().Key("editor").TeamID)
	require.Equal(t, restored.Staged.Generation, store.Meta().Generation)
	require.NoError(t, store.Close())
}

// requireArchiveRefusal requires err to refuse the archive's image for reason
// with the archive's advice, never the live store's.
func requireArchiveRefusal(t *testing.T, err error, reason sqlstore.Reason) {
	t.Helper()
	var refusal *sqlstore.Refusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, reason, refusal.Reason)
	require.Contains(t, err.Error(), "the archive's access database image is refused: "+refusal.Summary()+".")
	require.NotContains(t, err.Error(), refusal.NextAction(), "the live store's next action")
	require.NotContains(t, err.Error(), "remove the access directory")
}

// TestRecoveryProfileRefusals: each bundle breaks one profile rule, and both
// check and restore refuse it before creating anything.
func TestRecoveryProfileRefusals(t *testing.T) {
	image, meta := capturedImage(t)
	content := readBytes(t, image)
	node := []byte("node: {port: 9090}")
	marker := func(storeID string) []byte { return []byte(`{"version":1,"store_id":"` + storeID + `"}`) }
	database := func(m *Manifest) { m.Profile, m.Inventory, m.Store = ProfileDatabase, nil, &meta }
	with := func(edit func(*StoreMeta)) func(*Manifest) {
		return func(m *Manifest) {
			changed := meta
			edit(&changed)
			database(m)
			m.Store = &changed
		}
	}
	corrupt := func(t *testing.T) []byte {
		// Overwrite the first byte of the keys table's root page.
		altered := alterImage(t, image, "SELECT 1")
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(altered))
		require.NoError(t, err)
		var page, size int64
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT rootpage FROM sqlite_schema WHERE name = 'keys'").Scan(&page))
		require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA page_size").Scan(&size))
		require.NoError(t, db.Close())
		data := readBytes(t, altered)
		data[(page-1)*size] = 0xff
		return data
	}
	for name, tc := range map[string]struct {
		files  map[string][]byte
		edit   func(*Manifest)
		reason sqlstore.Reason // a refusal from the image check, or none
	}{
		"an unknown profile":                  {map[string][]byte{imageName: content}, func(m *Manifest) { database(m); m.Profile = "handoff" }, ""},
		"no profile":                          {map[string][]byte{"config/node.yaml": node}, func(m *Manifest) { m.Profile = "" }, ""},
		"a database bundle with an inventory": {map[string][]byte{imageName: content}, func(m *Manifest) { database(m); m.Inventory = &Inventory{Version: 1} }, ""},
		"a database bundle with a key":        {map[string][]byte{imageName: content}, func(m *Manifest) { database(m); m.KeySHA256 = digest(content) }, ""},
		"a database bundle with another file": {map[string][]byte{imageName: content, "config/node.yaml": node}, database, ""},
		"a database bundle without its image": {map[string][]byte{"config/node.yaml": node}, database, ""},
		"an image without its store":          {map[string][]byte{imageName: content}, func(m *Manifest) { database(m); m.Store = nil }, ""},
		"an installation image without store": {map[string][]byte{"config/node.yaml": node, imageName: content}, nil, ""},
		"an installation store without image": {map[string][]byte{"config/node.yaml": node}, func(m *Manifest) { m.Store = &meta }, ""},
		"an installation without inventory":   {map[string][]byte{"config/node.yaml": node, imageName: content}, func(m *Manifest) { m.Inventory, m.Store = nil, &meta }, ""},
		"a store marker without the image":    {map[string][]byte{"config/node.yaml": node, "config/access-store.json": marker(meta.StoreID)}, nil, ""},
		"a store marker naming another store": {map[string][]byte{"config/node.yaml": node, "config/access-store.json": marker("another"), imageName: content}, func(m *Manifest) { m.Store = &meta }, ""},
		"a store marker that is not one":      {map[string][]byte{"config/node.yaml": node, "config/access-store.json": []byte(`{"version":1}`), imageName: content}, func(m *Manifest) { m.Store = &meta }, ""},
		"a staging manifest":                  {map[string][]byte{imageName: content}, func(m *Manifest) { database(m); m.Staged = &Staged{} }, ""},
		"another store ID":                    {map[string][]byte{imageName: content}, with(func(s *StoreMeta) { s.StoreID = "another" }), ""},
		"another generation":                  {map[string][]byte{imageName: content}, with(func(s *StoreMeta) { s.Generation = "another" }), ""},
		"another schema version":              {map[string][]byte{imageName: content}, with(func(s *StoreMeta) { s.SchemaVersion++ }), ""},
		"another revision":                    {map[string][]byte{imageName: content}, with(func(s *StoreMeta) { s.Revision++ }), ""},
		"another sequence":                    {map[string][]byte{imageName: content}, with(func(s *StoreMeta) { s.Sequence-- }), ""},
		"a corrupt image, digests recomputed": {map[string][]byte{imageName: corrupt(t)}, database, sqlstore.IntegrityFailed},
		"an image in WAL mode":                {map[string][]byte{imageName: readBytes(t, alterImage(t, image, "PRAGMA journal_mode = WAL"))}, database, sqlstore.Unreadable},
		"an image with an added trigger":      {map[string][]byte{imageName: readBytes(t, alterImage(t, image, "CREATE TRIGGER planted AFTER INSERT ON teams BEGIN SELECT 1; END"))}, database, sqlstore.IntegrityFailed},
		"an image after ANALYZE":              {map[string][]byte{imageName: readBytes(t, alterImage(t, image, "ANALYZE"))}, database, sqlstore.IntegrityFailed},
		"an image a newer release wrote":      {map[string][]byte{imageName: readBytes(t, alterImage(t, image, "PRAGMA user_version = 99"))}, database, sqlstore.SchemaNewer},
	} {
		t.Run(name, func(t *testing.T) {
			var edits []func(*Manifest)
			if tc.edit != nil {
				edits = append(edits, tc.edit)
			}
			archive := writeTestBundle(t, tc.files, edits...)
			destination := filepath.Join(t.TempDir(), "restored")
			_, restoreErr := Restore(t.Context(), archive, "", destination)
			require.NoDirExists(t, destination)
			for _, err := range []error{second(Check(t.Context(), archive, "")), restoreErr} {
				require.Error(t, err)
				if tc.reason != "" {
					requireArchiveRefusal(t, err, tc.reason)
				} else {
					var refusal *sqlstore.Refusal
					require.False(t, errors.As(err, &refusal), "refused by its manifest, not its image: %v", err)
				}
			}
		})
	}
	newer := writeTestBundle(t, map[string][]byte{imageName: readBytes(t, alterImage(t, image, "PRAGMA user_version = 99"))}, database)
	_, err := Check(t.Context(), newer, "")
	require.ErrorContains(t, err, "only that release can check or restore this archive")
}

// TestRecoveryFailedPreparationLeavesIncomplete: an image that passes its
// check but cannot take the preparation's commit (its history's last event
// number leaves no room for the restore's row) stages nothing usable:
// INCOMPLETE stays, no staging manifest is written, and the staged image is
// as archived.
func TestRecoveryFailedPreparationLeavesIncomplete(t *testing.T) {
	image, meta := capturedImage(t)
	altered := alterImage(t, image, "INSERT INTO audit_events (seq, time, action, actor, subject, request_id) VALUES (9223372036854775807, 0, 'team.created', 'a', 'b', '')")
	archive := databaseBundle(t, altered, meta)
	_, err := Check(t.Context(), archive, "")
	require.NoError(t, err, "the image passes its check")
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(t.Context(), archive, "", destination)
	require.Error(t, err)
	require.FileExists(t, filepath.Join(destination, "INCOMPLETE"))
	require.NoFileExists(t, filepath.Join(destination, "manifest.json"))
	require.Equal(t, readBytes(t, altered), readBytes(t, filepath.Join(destination, filepath.FromSlash(imageName))))
}

// TestRecoveryImageNeverEntersMemory: reading a bundle streams its image to a
// file and keeps none of it among the small files held in memory.
func TestRecoveryImageNeverEntersMemory(t *testing.T) {
	image, meta := capturedImage(t)
	f, err := os.Open(databaseBundle(t, image, meta))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	compressed, err := gzip.NewReader(f)
	require.NoError(t, err)
	reader := tar.NewReader(compressed)
	manifest, err := readManifest(reader)
	require.NoError(t, err)
	images, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = images.Close() }()
	snapshots, err := readEntries(t.Context(), reader, manifest, nil, images)
	require.NoError(t, err)
	require.Empty(t, snapshots)
	require.Equal(t, readBytes(t, image), readBytes(t, imagePath(images)))
}

func second[T any](_ T, err error) error { return err }

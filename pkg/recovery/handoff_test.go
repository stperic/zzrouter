package recovery

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/control"
	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/version"
	"github.com/stretchr/testify/require"
)

// handoffNever is what a coordinator holds on disk that no handoff carries:
// node configuration, dotenv files, OAuth state, cluster CA and deny list,
// node identity, cluster and pairing state, private keys, the recovery
// layout, online backups, the rollback directory, update state, data
// directories, the pricing cache, provider installs and models. Paths are
// relative to the configuration directory, which in the fixture is also the
// data directory.
var handoffNever = []string{
	"node.yaml", ".env", ".env.local", ".env.production", "oauth.json",
	"ca/ca.key", "ca/ca.pem", "ca/deny_list.json", "identity/node.key", "identity/node.pem",
	"cluster/ca.pem", "cluster/coordinator_url", "pairing.txt", "private/credentials.key",
	InventoryFile, "access-store.json", "access/access.sqlite", "access/backups/old.tar.gz",
	"rollback/1/files/node.yaml", "update/request.json", "update-operation.json",
	"update-pending-confirm.json", "update-history.json", "pinned_runs.json", "downloads.json",
	"pricing/pricing-cache.json", "providers/mlx/manifest.json", "providers/mlx/venv/bin/python",
	"models/model.gguf",
}

// handoffUnknown is what no release writes today: a file added to the
// configuration later must not travel by default.
var handoffUnknown = []string{
	"future.yaml", "keys.yaml", "teams.yaml", "audit.jsonl", "cli.yaml", ".gitignore",
	"providers/future.yaml", "providers/on-demand/llamacpp/notes.txt",
	"providers/on-demand/llamacpp/config.yaml.bak", "providers/future-kind/custom/config.yaml",
	"providers/on-demand/llamacpp/assets/.hidden", "providers/on-demand/llamacpp/assets/nested/template.jinja",
}

// handoffInstallation is recoveryFixture's stopped coordinator with operator
// assets for a provider of every kind, model groups and pricing overrides,
// each written by its own store, and every handoffNever and handoffUnknown
// file on disk. It returns the sorted entries a handoff of it must hold.
func handoffInstallation(t *testing.T) (dir, output, keyOutput string, want []string) {
	t.Helper()
	dir, output, keyOutput = recoveryFixture(t)
	providers := filepath.Join(dir, "providers")
	// What the release wrote into the provider tree: settings, the ownership
	// receipt, and every shipped provider's config and schema.
	require.NoError(t, filepath.WalkDir(providers, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, relErr := filepath.Rel(dir, path)
			want = append(want, "config/"+filepath.ToSlash(rel))
			err = relErr
		}
		return err
	}))
	for _, provider := range []string{"on-demand/llamacpp", "external/ollama", "cloud/openai", "registries/huggingface"} {
		_, err := assets.NewDir(filepath.Join(providers, filepath.FromSlash(provider))).Write("chat-template.jinja", []byte("{{ messages }}"))
		require.NoError(t, err)
		want = append(want, "config/providers/"+provider+"/assets/chat-template.jinja")
	}
	groups := group.NewGroupStore()
	require.NoError(t, groups.Set("assistant", group.ModelGroup{Replicas: []group.Replica{{Name: "local", Model: "qwen", App: "llamacpp"}}}))
	require.NoError(t, groups.SaveToFile(filepath.Join(dir, "model_groups.yaml")))
	prices := pricing.NewStore(&config.PricingConfig{}, filepath.Join(dir, "pricing"))
	require.NoError(t, prices.LoadOverrides(filepath.Join(dir, "pricing_overrides.yaml")))
	require.NoError(t, prices.SetOverride(pricing.Override{Provider: "ollama-cloud", Model: pricing.WildcardModel}))
	want = append(want, "config/credentials.enc", "config/model_groups.yaml", "config/pricing_overrides.yaml", "config/spend.json", imageName)
	inventory, err := Read(t.Context(), dir)
	require.NoError(t, err)
	var paths []string
	for _, name := range slices.Concat(handoffNever, handoffUnknown) {
		paths = append(paths, filepath.Join(dir, filepath.FromSlash(name)))
	}
	// Provider installs and their data under the provider root, as on Linux.
	for _, name := range []string{"llamacpp/manifest.json", "llamacpp/bin/llama-server", "comfyui/data/workflow.json"} {
		paths = append(paths, filepath.Join(inventory.ProviderDir, filepath.FromSlash(name)))
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			continue // the fixture's own: node.yaml, the key, the layout, the database
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("not for a new coordinator"), 0o600))
	}
	slices.Sort(want)
	return dir, output, keyOutput, want
}

func names(files []File) []string {
	var list []string
	for _, file := range files {
		list = append(list, file.Name)
	}
	return list
}

// TestRecoveryHandoffCarriesExactlyItsAllowList: a handoff of a realistic
// coordinator holds exactly the allow-listed entries, none of what belongs to
// the old machine and no file of unknown purpose; names its source; and
// exports the protection key separately, its digest in the manifest.
func TestRecoveryHandoffCarriesExactlyItsAllowList(t *testing.T) {
	dir, output, keyOutput, want := handoffInstallation(t)
	before := issue(t, dir)
	manifest, err := BackupProfile(t.Context(), ProfileHandoff, dir, output, keyOutput)
	require.NoError(t, err)
	require.Equal(t, want, names(manifest.Files))
	require.Equal(t, ProfileHandoff, manifest.Profile)
	require.Equal(t, StoreMeta(before.meta), *manifest.Store)
	require.Equal(t, Origin{Release: version.Current.String(), Node: "recovery-test", Platform: runtime.GOOS}, *manifest.Source)
	require.Equal(t, handoffSeparate(), manifest.Separate)
	require.Nil(t, manifest.Inventory, "no inventory: the destination brings its own paths")
	require.Empty(t, manifest.MissingOptional)

	key, err := os.ReadFile(filepath.Join(dir, "private", "credentials.key"))
	require.NoError(t, err)
	exported, err := os.ReadFile(keyOutput)
	require.NoError(t, err)
	require.Equal(t, key, exported, "the key is exported separately")
	require.Equal(t, digest(key), manifest.KeySHA256)

	checked, err := Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
	require.Equal(t, want, names(checked.Files), "the archive holds what its manifest lists")
	require.Equal(t, *manifest.Source, *checked.Source)
	_, err = Check(t.Context(), output, "")
	require.ErrorContains(t, err, "separate protection-key")
}

// TestRecoveryHandoffStagesAPreparedImage: staging a handoff prepares its
// image like every profile's, so the sessions and app-key references the old
// coordinator issued do not resolve, while passwords and app keys work; the
// carried files are staged byte for byte, with the key beside them.
func TestRecoveryHandoffStagesAPreparedImage(t *testing.T) {
	dir, output, keyOutput, _ := handoffInstallation(t)
	before := issue(t, dir)
	manifest, err := BackupProfile(t.Context(), ProfileHandoff, dir, output, keyOutput)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(t.Context(), output, keyOutput, destination)
	require.NoError(t, err)
	requireStaged(t, destination, restored, *manifest.Store)
	require.Equal(t, manifest.Separate, restored.Separate)
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	var staged []string
	for _, entry := range entries {
		staged = append(staged, entry.Name())
	}
	require.Equal(t, []string{"config", "manifest.json", "protection", "store"}, staged)
	for _, name := range []string{"model_groups.yaml", "pricing_overrides.yaml", "spend.json", "credentials.enc", "providers/settings.yaml", "providers/on-demand/llamacpp/assets/chat-template.jinja"} {
		require.Equal(t, readBytes(t, filepath.Join(dir, filepath.FromSlash(name))), readBytes(t, filepath.Join(destination, "config", filepath.FromSlash(name))), name)
	}
	require.NoFileExists(t, filepath.Join(destination, "config", "node.yaml"))

	store := activate(t, destination)
	_, err = store.People().Authenticate(t.Context(), "owner", "a long test password")
	require.NoError(t, err, "the owner signs in with the same password")
	core := control.NewCore(control.StaticKeySet{}, store, nil, nil, nil)
	_, err = core.AuthenticateReference(before.session)
	require.ErrorIs(t, err, control.ErrInvalidKey, "the old coordinator's session does not resolve")
	_, err = core.AuthenticateReference(before.app)
	require.ErrorIs(t, err, control.ErrInvalidKey, "an app-key reference from the old coordinator does not resolve")
	_, err = core.Reference(before.raw)
	require.NoError(t, err, "the app key itself still signs in")
	events, _, err := store.AuditPage(t.Context(), audit.Filter{Action: audit.ActionRestorePrepared}, "", 10)
	require.NoError(t, err)
	require.Len(t, events, 1)

	key, err := secrets.LoadKeyFile(filepath.Join(destination, "protection", "credentials.key"))
	require.NoError(t, err)
	defer clear(key)
	vault, err := secrets.Open(filepath.Join(destination, "config", "credentials.enc"), key)
	require.NoError(t, err)
	record, err := vault.Snapshot("cloud")
	require.NoError(t, err)
	require.Equal(t, "synthetic-provider-secret", record.Values["token"])
	require.NoError(t, vault.Close())
}

// TestRecoveryHandoffCheckRefusesOtherEntries: check and restore refuse a
// handoff carrying anything its allow-list does not admit, naming the entry,
// and a handoff that breaks one of the profile's rules; restore creates
// nothing.
func TestRecoveryHandoffCheckRefusesOtherEntries(t *testing.T) {
	image, meta := capturedImage(t)
	content := readBytes(t, image)
	handoff := func(m *Manifest) {
		m.Profile, m.Inventory, m.Store = ProfileHandoff, nil, &meta
		m.Source, m.Separate = &Origin{Release: "0.3.0", Node: "coord-1", Platform: "linux"}, handoffSeparate()
	}
	const definition = "config/providers/cloud/openai/config.yaml"
	base := map[string][]byte{imageName: content, definition: []byte("enabled: false\n"), "config/model_groups.yaml": []byte("version: \"1\"\n")}
	with := func(name string, data []byte) map[string][]byte {
		files := map[string][]byte{name: data}
		for name, data := range base {
			files[name] = data
		}
		return files
	}
	_, err := Check(t.Context(), writeTestBundle(t, base, handoff), "")
	require.NoError(t, err, "the handoff itself passes")

	refuse := func(t *testing.T, archive, want string) {
		t.Helper()
		destination := filepath.Join(t.TempDir(), "restored")
		_, restoreErr := Restore(t.Context(), archive, "", destination)
		require.NoDirExists(t, destination)
		for _, err := range []error{second(Check(t.Context(), archive, "")), restoreErr} {
			require.ErrorContains(t, err, want)
			var refusal *sqlstore.Refusal
			require.False(t, errors.As(err, &refusal), "refused by its manifest, not its image: %v", err)
		}
	}
	for _, name := range slices.Concat(handoffNever, handoffUnknown) {
		t.Run(name, func(t *testing.T) {
			entry := "config/" + name
			refuse(t, writeTestBundle(t, with(entry, []byte("node: {port: 9090}")), handoff), entry)
		})
	}
	// Logical data and provider roots, and case variants of carried entries,
	// which the loader reads on a case-insensitive filesystem.
	for _, name := range []string{"data/pinned_runs.json", "data/downloads.json", "data/update-history.json", "providers/llamacpp/manifest.json", "providers/comfyui/data/workflow.json",
		"config/providers/cloud/groq/Config.yaml", "config/providers/Registries/huggingface/config.yaml", "config/providers/cloud/openai/Assets/template.jinja", "config/Spend.json"} {
		t.Run(name, func(t *testing.T) {
			refuse(t, writeTestBundle(t, with(name, []byte("{}")), handoff), name)
		})
	}
	image2 := map[string][]byte{imageName: content, "config/model_groups.yaml": base["config/model_groups.yaml"]}
	withHandoff := func(edit func(*Manifest)) func(*Manifest) { return func(m *Manifest) { handoff(m); edit(m) } }
	for name, tc := range map[string]struct {
		files map[string][]byte
		edit  func(*Manifest)
		want  string
	}{
		"no image and no store":         {map[string][]byte{definition: base[definition]}, withHandoff(func(m *Manifest) { m.Store = nil }), "holds the access database image"},
		"an image without its store":    {base, withHandoff(func(m *Manifest) { m.Store = nil }), "travel together"},
		"a store without its image":     {map[string][]byte{definition: base[definition]}, handoff, "travel together"},
		"no source":                     {base, withHandoff(func(m *Manifest) { m.Source = nil }), "names its source"},
		"an inventory":                  {base, withHandoff(func(m *Manifest) { m.Inventory = &Inventory{Version: 1} }), "no inventory"},
		"missing optional state":        {base, withHandoff(func(m *Manifest) { m.MissingOptional = []string{"data/downloads.json"} }), "no inventory"},
		"connections without a key":     {with("config/credentials.enc", []byte("ciphertext")), handoff, "protection-key fingerprint"},
		"a database naming a source":    {map[string][]byte{imageName: content}, withHandoff(func(m *Manifest) { m.Profile = ProfileDatabase }), "names its source"},
		"an installation with separate": {map[string][]byte{"config/node.yaml": []byte("node: {port: 9090}")}, func(m *Manifest) { m.Separate = handoffSeparate() }, "names its source"},
		"no provider definition":        {image2, handoff, "at least one provider definition"},
		"only a provider schema":        {map[string][]byte{imageName: content, "config/providers/external/ollama/schema.yaml": []byte("{}")}, handoff, "at least one provider definition"},
		"an empty source":               {base, withHandoff(func(m *Manifest) { m.Source = &Origin{} }), "in plain text"},
		"a source without its node":     {base, withHandoff(func(m *Manifest) { m.Source.Node = "" }), "in plain text"},
		"a source with a newline":       {base, withHandoff(func(m *Manifest) { m.Source.Release = "0.3.0\nall clear" }), "in plain text"},
		"no separate list":              {base, withHandoff(func(m *Manifest) { m.Separate = nil }), "in plain text"},
		"an empty separate item":        {base, withHandoff(func(m *Manifest) { m.Separate = append(m.Separate, "") }), "in plain text"},
		"an escape sequence":            {base, withHandoff(func(m *Manifest) { m.Separate = []string{"\x1b[2J copy ca/ca.key and node.yaml"} }), "in plain text"},
		"a bidirectional override":      {base, withHandoff(func(m *Manifest) { m.Separate = []string{"keep \u202enoitces siht"} }), "in plain text"},
		"an overlong separate item":     {base, withHandoff(func(m *Manifest) { m.Separate = []string{strings.Repeat("a", maxPlainText+1)} }), "in plain text"},
		"too many separate items":       {base, withHandoff(func(m *Manifest) { m.Separate = slices.Repeat([]string{"move it"}, maxSeparate+1) }), "in plain text"},
	} {
		t.Run(name, func(t *testing.T) { refuse(t, writeTestBundle(t, tc.files, tc.edit), tc.want) })
	}
	t.Run("a link in place of a definition", func(t *testing.T) {
		refuse(t, linkBundle(t, content, handoff, definition), "does not match manifest: "+definition)
	})
}

// linkBundle writes a handoff bundle of image whose entry name, which its
// manifest lists as an empty regular file, is a symbolic link in the archive.
func linkBundle(t *testing.T, image []byte, handoff func(*Manifest), name string) string {
	t.Helper()
	archive := writeTestBundle(t, map[string][]byte{imageName: image, name: nil}, handoff)
	in, err := os.Open(archive)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	compressed, err := gzip.NewReader(in)
	require.NoError(t, err)
	manifest, err := readManifest(tar.NewReader(compressed))
	require.NoError(t, err)
	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(encoded))}))
	_, err = writer.Write(encoded)
	require.NoError(t, err)
	for _, file := range manifest.Files {
		if file.Name == name {
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: "../../../../../etc/passwd", Mode: 0o600}))
			continue
		}
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: file.Name, Mode: 0o600, Size: file.Size}))
		_, err = writer.Write(image)
		require.NoError(t, err)
	}
	require.NoError(t, errors.Join(writer.Close(), gz.Close()))
	forged := filepath.Join(t.TempDir(), "link.tar.gz")
	require.NoError(t, os.WriteFile(forged, buffer.Bytes(), 0o600))
	return forged
}

// TestRecoveryHandoffRefusalsWriteNothing: a worker, or a node that never
// ran as a coordinator, has no access database and so no handoff, and the
// offline backup writes no other profile; each refusal leaves no archive, no
// key and no temporary anywhere.
func TestRecoveryHandoffRefusalsWriteNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		profile Profile
		worker  bool
		check   func(t *testing.T, err error)
	}{
		"a worker":         {ProfileHandoff, true, func(t *testing.T, err error) { require.ErrorIs(t, err, sqlstore.ErrNoStore) }},
		"a database":       {ProfileDatabase, false, func(t *testing.T, err error) { require.ErrorContains(t, err, "the running coordinator's backup API") }},
		"an unknown kind":  {"everything", false, func(t *testing.T, err error) { require.ErrorContains(t, err, `not "everything"`) }},
		"no profile given": {"", false, func(t *testing.T, err error) { require.ErrorContains(t, err, `not ""`) }},
	} {
		t.Run(name, func(t *testing.T) {
			dir, output, keyOutput := recoveryFixture(t)
			if tc.worker {
				require.NoError(t, os.RemoveAll(filepath.Join(dir, "access")))
				require.NoError(t, os.Remove(filepath.Join(dir, sqlstore.MarkerName)))
			}
			temp := privateTemp(t)
			_, err := BackupProfile(t.Context(), tc.profile, dir, output, keyOutput)
			tc.check(t, err)
			emptyDir(t, filepath.Dir(output), "no archive and no temporary beside it")
			emptyDir(t, filepath.Dir(keyOutput), "no key and no temporary beside it")
			emptyDir(t, temp, "nothing in the system's temporary directory")
		})
	}
}

// TestRecoveryHandoffRefusesLinksAndCaseVariants: what the node's loader
// would read through a link or under another spelling on a case-insensitive
// filesystem is refused by a handoff backup, never silently dropped, with
// the installation profile's reason for a link; links elsewhere in the tree,
// such as a provider install's environment, are not carried and do not stop
// it. A refusal writes nothing.
func TestRecoveryHandoffRefusesLinksAndCaseVariants(t *testing.T) {
	// link moves dir's rel out of the tree and links it back.
	link := func(t *testing.T, dir, rel string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		moved := filepath.Join(t.TempDir(), "moved")
		require.NoError(t, os.Rename(path, moved))
		if err := os.Symlink(moved, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	rename := func(from, to string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			require.NoError(t, os.Rename(filepath.Join(dir, filepath.FromSlash(from)), filepath.Join(dir, filepath.FromSlash(to))))
		}
	}
	linked := func(rel string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) { link(t, dir, rel) }
	}
	withAsset := func(rel string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			_, err := assets.NewDir(filepath.Join(dir, "providers", "cloud", "openai")).Write("template.jinja", []byte("{{ messages }}"))
			require.NoError(t, err)
			link(t, dir, rel)
		}
	}
	for name, tc := range map[string]struct {
		edit     func(t *testing.T, dir string)
		provider string // a provider the loader still reads after edit
		want     string
	}{
		"a linked provider tree":      {linked("providers"), "openai", "must not contain links: providers"},
		"a linked kind":               {linked("providers/cloud"), "openai", "must not contain links: " + filepath.Join("providers", "cloud")},
		"a linked provider":           {linked("providers/cloud/openai"), "openai", "must not contain links: " + filepath.Join("providers", "cloud", "openai")},
		"a linked definition":         {linked("providers/cloud/groq/config.yaml"), "groq", "must not contain links: " + filepath.Join("providers", "cloud", "groq", "config.yaml")},
		"linked assets":               {withAsset("providers/cloud/openai/assets"), "", "must not contain links: " + filepath.Join("providers", "cloud", "openai", "assets")},
		"a linked asset":              {withAsset("providers/cloud/openai/assets/template.jinja"), "", "must not contain links: " + filepath.Join("providers", "cloud", "openai", "assets", "template.jinja")},
		"a definition's case variant": {rename("providers/cloud/groq/config.yaml", "providers/cloud/groq/Config.yaml"), "", "a handoff refuses config/providers/cloud/groq/Config.yaml"},
		"a kind's case variant":       {rename("providers/registries", "providers/Registries"), "", "a handoff refuses config/providers/Registries/huggingface/config.yaml"},
		"links in a provider's install": {func(t *testing.T, dir string) {
			for _, rel := range []string{"providers/mlx-vlm/venv/bin/python", "providers/on-demand/mlx/venv"} {
				path := filepath.Join(dir, filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				if err := os.Symlink(filepath.Join(t.TempDir(), "python3"), path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
		}, "mlx", ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir, output, keyOutput := recoveryFixture(t)
			tc.edit(t, dir)
			if tc.provider != "" {
				apps, err := config.LoadAppsConfig(filepath.Join(dir, "providers"))
				require.NoError(t, err)
				_, ok := apps.LookupApp(tc.provider)
				require.True(t, ok, "the loader reads %s", tc.provider)
			}
			manifest, err := BackupProfile(t.Context(), ProfileHandoff, dir, output, keyOutput)
			if tc.want == "" {
				require.NoError(t, err)
				for _, file := range manifest.Files {
					require.NotContains(t, file.Name, "venv")
				}
				return
			}
			require.ErrorContains(t, err, tc.want)
			emptyDir(t, filepath.Dir(output), "no archive and no temporary beside it")
			emptyDir(t, filepath.Dir(keyOutput), "no key and no temporary beside it")
		})
	}
}

// TestRecoveryBackupRefusesWhatItsCheckRefuses: the checker's rules hold
// before a backup publishes anything, so an installation holding an
// authority file under another spelling writes no archive and no key.
func TestRecoveryBackupRefusesWhatItsCheckRefuses(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "OAuth.json"), []byte("{}"), 0o600))
	_, err := Backup(t.Context(), dir, output, keyOutput)
	require.ErrorContains(t, err, "canonical spelling: config/OAuth.json")
	emptyDir(t, filepath.Dir(output), "no archive and no temporary beside it")
	emptyDir(t, filepath.Dir(keyOutput), "no key and no temporary beside it")
}

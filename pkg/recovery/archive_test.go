package recovery

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/access/filestore"
	"github.com/stperic/zzrouter/pkg/access/keys"
	"github.com/stperic/zzrouter/pkg/access/people"
	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/audit"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

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
	accessStore, err := filestore.Open(t.Context(), dir, filestore.Options{})
	require.NoError(t, err)
	_, err = accessStore.CreateTeam(t.Context(), "workspace", teams.Team{Name: "Workspace", Kind: teams.KindShared}, audit.Event{})
	require.NoError(t, err)
	_, _, err = accessStore.CreateKey(t.Context(), "editor", keys.VirtualKey{Name: "Editor", Role: "user", TeamID: "workspace", TeamRole: "member"}, audit.Event{})
	require.NoError(t, err)
	require.NoError(t, accessStore.Close())
	peopleStore, err := people.Open(t.Context(), filepath.Join(dir, "access", "people.json"), people.Options{
		ValidateExternalIssuer: func(context.Context, string) error { return nil }, ValidateTeam: func(context.Context, string) error { return nil },
	})
	require.NoError(t, err)
	grant, err := peopleStore.Issue(t.Context(), people.GrantRequest{Purpose: people.PurposeSetup, TeamID: "workspace", Role: "admin", Issuer: people.Issuer{Reference: "test"}})
	require.NoError(t, err)
	bound, err := peopleStore.Exchange(t.Context(), grant.Token, "test-proof")
	require.NoError(t, err)
	_, err = peopleStore.ClaimOwner(t.Context(), bound.Token, "test-proof", people.CreatePerson{Username: "owner", Name: "Owner", Password: "a long test password"})
	require.NoError(t, err)
	require.NoError(t, peopleStore.Close())
	require.NoError(t, quota.SaveSpend(filepath.Join(dir, "spend.json"), &quota.SpendFile{Version: 1, States: map[string]*quota.SpendState{"team:workspace:": {EntityID: "workspace", Kind: quota.ScopeTeam, SpendMicro: 1750000}}}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pairing.txt"), []byte("transient-pairing-proof"), 0o600))
	output = filepath.Join(t.TempDir(), "backup.tar.gz")
	keyOutput = filepath.Join(t.TempDir(), "credentials.key")
	return
}

func TestRecoveryRoundTripPreservesAuthorityAndSpend(t *testing.T) {
	dir, output, keyOutput := recoveryFixture(t)
	manifest, err := Backup(t.Context(), dir, output, keyOutput)
	require.NoError(t, err)
	require.False(t, slices.ContainsFunc(manifest.Files, func(file File) bool {
		return file.Name == "config/private/credentials.key" || file.Name == "config/pairing.txt" || file.Name == "config/installation.lock"
	}))
	checked, err := Check(t.Context(), output, keyOutput)
	require.NoError(t, err)
	require.Equal(t, len(manifest.Files), len(checked.Files))
	require.NotEmpty(t, checked.MissingOptional, "never-installed providers are explicitly reported, not silently omitted")
	destination := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(t.Context(), output, keyOutput, destination)
	require.NoError(t, err)
	for _, name := range []string{"node.yaml", "keys.yaml", "teams.yaml", "spend.json", "access/people.json", "credentials.enc"} {
		before, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		after, err := os.ReadFile(filepath.Join(destination, "config", name))
		require.NoError(t, err)
		require.Equal(t, before, after, name)
	}
	restoredPeople, err := people.Open(t.Context(), filepath.Join(destination, "config", "access", "people.json"), people.Options{
		ValidateExternalIssuer: func(context.Context, string) error { return nil }, ValidateTeam: func(context.Context, string) error { return nil },
	})
	require.NoError(t, err)
	person, err := restoredPeople.Authenticate(t.Context(), "owner", "a long test password")
	require.NoError(t, err)
	require.Equal(t, "admin", person.Role)
	require.Equal(t, "workspace", person.TeamID)
	require.NoError(t, restoredPeople.Close())
	keyData, err := os.ReadFile(filepath.Join(destination, "config", "keys.yaml"))
	require.NoError(t, err)
	restoredKeys, err := filestore.DecodeKeys(keyData)
	require.NoError(t, err)
	require.Equal(t, "workspace", restoredKeys["editor"].TeamID)
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
	_, err = os.Stat(filepath.Join(destination, "INCOMPLETE"))
	require.ErrorIs(t, err, os.ErrNotExist)
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
	for _, name := range []string{"../escape", "config/../../escape", "config/C:drive", "config/CON", "config/name.", "config\\escape", "config/private/key", "config/owner.lock", "config/update-pending-confirm.json", "config/update-operation.json", "config/backups/executable", "config/TEAMS.YAML", "config/Access/people.json"} {
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
	_, err = filestore.DecodeKeys([]byte("keys: {broken: null}"))
	require.Error(t, err, "malformed keys must never panic")
}

func writeTestBundle(t *testing.T, files map[string][]byte) string {
	t.Helper()
	manifest := Manifest{Version: 1, Inventory: Inventory{Version: 1}}
	for name, data := range files {
		manifest.Files = append(manifest.Files, File{Name: name, Size: int64(len(data)), SHA256: digest(data)})
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

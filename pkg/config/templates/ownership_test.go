package templates

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func requireOwnershipIdentity(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("historical recovery requires a nonroot node identity")
	}
}

func ownershipSource(files map[string]string) fstest.MapFS {
	src := fstest.MapFS{}
	for rel, body := range files {
		src[providersRoot+"/"+rel] = &fstest.MapFile{Data: []byte(body)}
	}
	return src
}

func putOwnershipFile(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

func ownershipBytes(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	return body
}

func readOwnership(t *testing.T, dir string) ownershipReceipt {
	t.Helper()
	var receipt ownershipReceipt
	require.NoError(t, json.Unmarshal(ownershipBytes(t, filepath.Join(dir, ownershipFile)), &receipt))
	return receipt
}

func writeOwnership(t *testing.T, dir string, receipt ownershipReceipt) {
	t.Helper()
	body, err := json.Marshal(receipt)
	require.NoError(t, err)
	putOwnershipFile(t, dir, ownershipFile, string(body))
}

func TestOwnershipGeneratedFromSelectorBeforeValues(t *testing.T) {
	requireOwnershipIdentity(t)
	dir := t.TempDir()
	const rel = "on-demand/engine/config.yaml"
	const body = "runtime: {execution: {command: engine}}\npinned_version: null\nfeatures: {generate: {default: true}}\nsecret: do-not-record\n"
	src := ownershipSource(map[string]string{rel: body, "on-demand/engine/schema.yaml": "parameters: {}", "on-demand/engine/assets/workflow.json": "{}"})
	_, _, err := prepareManagedFiles(src, dir)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(dir, rel), "ownership must precede seeding")
	var template map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(body), &template))
	receipt := readOwnership(t, dir)
	require.Len(t, receipt.Files, 1)
	assert.ElementsMatch(t, managedPaths(template), receipt.Files[rel])
	assert.NotContains(t, string(ownershipBytes(t, filepath.Join(dir, ownershipFile))), "do-not-record")
	before, err := os.Stat(filepath.Join(dir, ownershipFile))
	require.NoError(t, err)
	if runtime.GOOS != "windows" { // Windows reports ACL-governed files as 0666
		assert.Equal(t, os.FileMode(0600), before.Mode().Perm())
	}
	_, _, err = prepareManagedFiles(src, dir)
	require.NoError(t, err)
	after, err := os.Stat(filepath.Join(dir, ownershipFile))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "an unchanged receipt must not be republished")
}

func TestOwnershipRollbackRetainsAllHistoricalEvidence(t *testing.T) {
	requireOwnershipIdentity(t)
	const rel = "on-demand/engine/config.yaml"
	old := ownershipSource(map[string]string{rel: "install: {runtimes: {python: {packages: [old]}}}\n"})
	middle := ownershipSource(map[string]string{rel: "runtime: {execution: {command: middle}}\ninstall: {runtimes: {python: {packages: [middle]}}}\n"})
	newest := ownershipSource(map[string]string{rel: "runtime: {execution: {command: newest}, data_dir: data}\ninstall: {runtimes: {python: {source: {repository: newer}}}}\npinned_version: latest\n"})
	for _, tc := range []struct {
		name         string
		intermediate bool
	}{{"R+1 to R", false}, {"R+2 to R+1 to R", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := putOwnershipFile(t, dir, rel, string(newest[providersRoot+"/"+rel].Data)+"enabled: false\ndefaults: {parameters: {temperature: '0.42'}}\nmodels: [{match: operator-model}]\n")
			_, err := reconcile(newest, dir)
			require.NoError(t, err)
			// Model the descriptor emitted by a future selector, without changing globals.
			receipt := readOwnership(t, dir)
			receipt.Files[rel] = append(receipt.Files[rel], []string{"future_recipe"}, []string{"runtime", "future_spine"})
			writeOwnership(t, dir, receipt)
			live := map[string]any{}
			require.NoError(t, yaml.Unmarshal(ownershipBytes(t, path), &live))
			live["future_recipe"] = "new"
			live["runtime"].(map[string]any)["future_spine"] = "new"
			live["runtime"].(map[string]any)["endpoint"] = "http://operator"
			data, err := yaml.Marshal(live)
			require.NoError(t, err)
			putOwnershipFile(t, dir, rel, string(data))
			if tc.intermediate {
				_, err = reconcile(middle, dir)
				require.NoError(t, err)
				assert.Contains(t, readOwnership(t, dir).Files[rel], []string{"runtime", "data_dir"})
				assert.Contains(t, readOwnership(t, dir).Files[rel], []string{"future_recipe"})
				// An interrupted/late writer may leave historical bytes again before retry.
				require.NoError(t, yaml.Unmarshal(ownershipBytes(t, path), &live))
				live["future_recipe"] = "again"
				live["runtime"].(map[string]any)["data_dir"] = "again"
				data, err = yaml.Marshal(live)
				require.NoError(t, err)
				putOwnershipFile(t, dir, rel, string(data))
			}
			_, err = reconcile(old, dir)
			require.NoError(t, err)
			live = map[string]any{}
			require.NoError(t, yaml.Unmarshal(ownershipBytes(t, path), &live))
			assert.NotContains(t, live, "future_recipe")
			assert.NotContains(t, live, "pinned_version")
			assert.Equal(t, map[string]any{"endpoint": "http://operator"}, live["runtime"])
			assert.Equal(t, false, live["enabled"])
			assert.Equal(t, map[string]any{"parameters": map[string]any{"temperature": "0.42"}}, live["defaults"])
			assert.Equal(t, []any{map[string]any{"match": "operator-model"}}, live["models"])
			assert.NotContains(t, string(ownershipBytes(t, path)), "repository")
			assert.Contains(t, readOwnership(t, dir).Files[rel], []string{"future_recipe"})
			changed, err := reconcile(old, dir)
			require.NoError(t, err)
			assert.Empty(t, changed)
		})
	}
}

func TestOwnershipQuarantineRestoreAndCustomProvider(t *testing.T) {
	requireOwnershipIdentity(t)
	dir := t.TempDir()
	const oldRel, newRel = "cloud/old/config.yaml", "on-demand/new/config.yaml"
	old := ownershipSource(map[string]string{oldRel: "protocol: openai\n"})
	newer := ownershipSource(map[string]string{oldRel: "protocol: openai\n", newRel: "runtime: {}\n"})
	_, _, err := prepareManagedFiles(newer, dir)
	require.NoError(t, err)
	const original = "# operator header\nenabled: true\nnewer_recipe: retained\n"
	live := putOwnershipFile(t, dir, newRel, original)
	schema := putOwnershipFile(t, dir, "on-demand/new/schema.yaml", "newer-schema")
	asset := putOwnershipFile(t, dir, "on-demand/new/assets/workflow.json", "operator-workflow")
	custom := putOwnershipFile(t, dir, "on-demand/custom/config.yaml", "unknown_custom_key: keep")
	for range 2 {
		_, _, err = prepareManagedFiles(old, dir)
		require.NoError(t, err)
		assert.NoFileExists(t, live)
		assert.Equal(t, original, string(ownershipBytes(t, live+".rollback")))
	}
	assert.Equal(t, "newer-schema", string(ownershipBytes(t, schema)))
	assert.Equal(t, "operator-workflow", string(ownershipBytes(t, asset)))
	assert.Equal(t, "unknown_custom_key: keep", string(ownershipBytes(t, custom)))
	for range 2 {
		_, _, err = prepareManagedFiles(newer, dir)
		require.NoError(t, err)
		assert.Equal(t, original, string(ownershipBytes(t, live)))
		assert.NoFileExists(t, live+".rollback")
	}
}

func TestOwnershipArchiveCollisionAndInterruptedPass(t *testing.T) {
	requireOwnershipIdentity(t)
	dir := t.TempDir()
	old := ownershipSource(map[string]string{"cloud/old/config.yaml": "{}"})
	newer := ownershipSource(map[string]string{"cloud/old/config.yaml": "{}", "cloud/a/config.yaml": "{}", "cloud/z/config.yaml": "{}"})
	_, _, err := prepareManagedFiles(newer, dir)
	require.NoError(t, err)
	a := putOwnershipFile(t, dir, "cloud/a/config.yaml", "first")
	z := putOwnershipFile(t, dir, "cloud/z/config.yaml", "second")
	require.NoError(t, os.Symlink("absent", z+".rollback"))
	_, _, err = prepareManagedFiles(old, dir)
	require.ErrorIs(t, err, fs.ErrExist, "a dangling destination must also refuse replacement")
	assert.NoFileExists(t, a)
	assert.Equal(t, "first", string(ownershipBytes(t, a+".rollback")))
	assert.Equal(t, "second", string(ownershipBytes(t, z)))
	assert.Len(t, readOwnership(t, dir).Files, 3, "the union must survive a partial archive pass")
	require.NoError(t, os.Remove(z+".rollback"))
	_, _, err = prepareManagedFiles(old, dir)
	require.NoError(t, err)
	putOwnershipFile(t, dir, "cloud/z/config.yaml", "operator collision")
	_, _, err = prepareManagedFiles(newer, dir)
	require.ErrorIs(t, err, fs.ErrExist)
	assert.Equal(t, "operator collision", string(ownershipBytes(t, z)))
	assert.Equal(t, "second", string(ownershipBytes(t, z+".rollback")))
	require.NoError(t, os.Remove(z))
	_, _, err = prepareManagedFiles(newer, dir)
	require.NoError(t, err)
	assert.Equal(t, "first", string(ownershipBytes(t, a)))
	assert.Equal(t, "second", string(ownershipBytes(t, z)))
}

func TestOwnershipInvalidReceiptStopsEveryWriter(t *testing.T) {
	requireOwnershipIdentity(t)
	const rel = "cloud/openai/config.yaml"
	bad := map[string][]byte{"malformed": []byte("{"), "null": []byte("null"), "unsupported": []byte(`{"format":2,"files":{}}`), "missing format": []byte(`{"files":{}}`), "null files": []byte(`{"format":1,"files":null}`), "oversized": bytes.Repeat([]byte(" "), maxOwnershipBytes+1)}
	full := ownershipReceipt{Format: 1, Files: map[string][][]string{rel: {{"x"}}}}
	body, err := json.Marshal(full)
	require.NoError(t, err)
	full.Files[rel][0][0] = strings.Repeat("x", maxOwnershipBytes-len(body)+1)
	bad["union too large"], err = json.Marshal(full)
	require.NoError(t, err)
	require.Len(t, bad["union too large"], maxOwnershipBytes)
	for name, id := range map[string]string{"traversal": "../a/config.yaml", "absolute": "/cloud/a/config.yaml", "windows": `cloud\a\config.yaml`, "drive": "C:/a/config.yaml", "schema": "cloud/a/schema.yaml", "settings": "settings.yaml", "deep file": "cloud/a/sub/config.yaml"} {
		body, err := json.Marshal(ownershipReceipt{Format: 1, Files: map[string][][]string{id: {{"install"}}}})
		require.NoError(t, err)
		bad[name] = body
	}
	for name, path := range map[string][]string{"empty": {}, "deep key": {"a", "b", "c"}, "slash": {"runtime/escape", "key"}, "parent": {"runtime", ".."}, "backslash": {`runtime\escape`}, "colon": {"x:y"}, "nul": {"x\x00y"}} {
		body, err := json.Marshal(ownershipReceipt{Format: 1, Files: map[string][][]string{rel: {path}}})
		require.NoError(t, err)
		bad[name] = body
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			for _, writer := range []struct {
				name string
				run  func(string) error
			}{{"seed", InstallDefaults}, {"reconcile", func(dir string) error { _, err := ReconcileManagedSpine(dir); return err }}} {
				t.Run(writer.name, func(t *testing.T) {
					dir := t.TempDir()
					live := putOwnershipFile(t, dir, rel, "version_source: {type: stale}\n")
					putOwnershipFile(t, dir, ownershipFile, string(raw))
					err := writer.run(dir)
					require.Error(t, err)
					if name == "oversized" || name == "union too large" {
						assert.ErrorIs(t, err, utils.ErrResponseTooLarge)
					}
					assert.Equal(t, raw, ownershipBytes(t, filepath.Join(dir, ownershipFile)))
					assert.Equal(t, "version_source: {type: stale}\n", string(ownershipBytes(t, live)))
					assert.NoDirExists(t, filepath.Join(dir, "on-demand"), "seeding must not precede publication")
				})
			}
		})
	}
}

func TestOwnershipPublicationAndRepairIOFailures(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not gate root or Windows the same way")
	}
	const rel = "cloud/a/config.yaml"
	const healthyRel = "cloud/z/config.yaml"
	src := ownershipSource(map[string]string{rel: "install: {new: true}\n", healthyRel: "install: {new: true}\n"})
	t.Run("publication precedes mutation", func(t *testing.T) {
		dir := t.TempDir()
		live := putOwnershipFile(t, dir, rel, "install: {old: true}\n")
		require.NoError(t, os.Chmod(dir, 0500))
		t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0700)) })
		_, err := reconcile(src, dir)
		require.ErrorIs(t, err, fs.ErrPermission)
		assert.NoFileExists(t, filepath.Join(dir, ownershipFile))
		assert.Equal(t, "install: {old: true}\n", string(ownershipBytes(t, live)))
	})
	t.Run("write failure aggregates and retries", func(t *testing.T) {
		dir := t.TempDir()
		live := putOwnershipFile(t, dir, rel, "install: {old: true}\n")
		healthy := putOwnershipFile(t, dir, healthyRel, "install: {old: true}\n")
		_, _, err := prepareManagedFiles(src, dir)
		require.NoError(t, err)
		parent := filepath.Dir(live)
		require.NoError(t, os.Chmod(parent, 0500))
		t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0700)) })
		changed, err := reconcile(src, dir)
		require.ErrorIs(t, err, fs.ErrPermission)
		assert.Contains(t, changed, healthyRel)
		assert.Equal(t, "install: {old: true}\n", string(ownershipBytes(t, live)))
		assert.Contains(t, string(ownershipBytes(t, healthy)), "new: true")
		require.NoError(t, os.Chmod(parent, 0700))
		_, err = reconcile(src, dir)
		require.NoError(t, err)
		assert.Contains(t, string(ownershipBytes(t, live)), "new: true")
	})
	t.Run("rename failures leave source for retry", func(t *testing.T) {
		dir := t.TempDir()
		live := putOwnershipFile(t, dir, rel, "enabled: true\n")
		_, _, err := prepareManagedFiles(src, dir)
		require.NoError(t, err)
		old := ownershipSource(map[string]string{healthyRel: "install: {new: true}\n"})
		parent := filepath.Dir(live)
		t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0700)) })
		for _, target := range []fs.FS{old, src} {
			require.NoError(t, os.Chmod(parent, 0500))
			_, _, err = prepareManagedFiles(target, dir)
			require.ErrorIs(t, err, fs.ErrPermission)
			require.NoError(t, os.Chmod(parent, 0700))
			_, _, err = prepareManagedFiles(target, dir)
			require.NoError(t, err)
		}
		assert.Equal(t, "enabled: true\n", string(ownershipBytes(t, live)))
	})
	t.Run("read failure propagates after other repairs", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, rel), 0700))
		healthy := putOwnershipFile(t, dir, healthyRel, "install: {old: true}\n")
		changed, err := reconcile(src, dir)
		require.Error(t, err)
		assert.Contains(t, changed, healthyRel)
		assert.Contains(t, string(ownershipBytes(t, healthy)), "new: true")
	})
}

func TestOwnershipProviderDirectorySymlink(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "cloud"), 0700))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "cloud", "engine")))
	putOwnershipFile(t, target, "config.yaml", "enabled: false\ninstall: {old: true}\n")
	_, err := reconcile(ownershipSource(map[string]string{"cloud/engine/config.yaml": "install: {new: true}\n"}), dir)
	require.NoError(t, err)
	assert.Contains(t, string(ownershipBytes(t, filepath.Join(target, "config.yaml"))), "new: true")
}

func TestOwnershipRootBootstrapNeverReadsReceipt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root; an isolated UID overlay can also exercise this branch")
	}
	dir := t.TempDir()
	const poisoned = "not a receipt"
	path := putOwnershipFile(t, dir, ownershipFile, poisoned)
	live := putOwnershipFile(t, dir, "on-demand/comfyui/config.yaml", "enabled: false\nruntime: {max_concurrent_requests: 99}\n")
	_, err := PrepareProviders(dir)
	require.NoError(t, err, "root bootstrap must not decode service-owned evidence")
	assert.Equal(t, poisoned, string(ownershipBytes(t, path)))
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(ownershipBytes(t, live), &doc))
	assert.NotContains(t, doc["runtime"], "max_concurrent_requests")
	assert.Contains(t, doc["runtime"], "execution")
	assert.Equal(t, false, doc["enabled"])
}

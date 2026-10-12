package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/model/integrity"
)

func TestDeletionPhysicalPathRetainsFeatureVerificationContract(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "physical")
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.Symlink(dir, alias))
	writeModel(t, dir, "weights.gguf", "weights")
	require.NoError(t, integrity.WriteManifest(dir, &integrity.ModelManifest{Version: 1, Files: []integrity.FileChecksum{{RelativePath: "weights.gguf", Size: 7}, {RelativePath: "mmproj.gguf", Size: 9, Feature: "vision"}}}))
	physical, err := filepath.EvalSymlinks(filepath.Join(alias, "weights.gguf"))
	require.NoError(t, err)
	err = VerifyLocalWeights(t.Context(), physical, []string{filepath.Join(alias, "mmproj.gguf")})
	require.ErrorIs(t, err, ErrModelIncomplete, "a selected missing feature cannot disappear when the weights path is frozen")
}

func TestDeletionDirectorySymlinkVerificationContract(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "model.safetensors", "weights")
	require.NoError(t, integrity.WriteManifest(dir, &integrity.ModelManifest{Version: 1, Files: []integrity.FileChecksum{{RelativePath: "model.safetensors", Size: 7}}}))
	outside := filepath.Join(t.TempDir(), "outside.safetensors")
	require.NoError(t, os.WriteFile(outside, []byte("weights"), 0600))
	require.NoError(t, os.Remove(filepath.Join(dir, "model.safetensors")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "model.safetensors")))
	require.Error(t, VerifyLocalWeights(t.Context(), dir, nil), "directory launches must retain manifest symlink rejection")
}

func TestDeletionShardSymlinkVerificationContract(t *testing.T) {
	dir := t.TempDir()
	first, second := "m-00001-of-00002.gguf", "m-00002-of-00002.gguf"
	writeModel(t, dir, first, "weights")
	writeModel(t, dir, second, "weights")
	require.NoError(t, integrity.WriteManifest(dir, &integrity.ModelManifest{Version: 1, Files: []integrity.FileChecksum{{RelativePath: first, Size: 7}, {RelativePath: second, Size: 7}}}))
	outside := filepath.Join(t.TempDir(), "outside.gguf")
	require.NoError(t, os.WriteFile(outside, []byte("weights"), 0600))
	require.NoError(t, os.Remove(filepath.Join(dir, second)))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, second)))
	require.Error(t, VerifyLocalWeights(t.Context(), filepath.Join(dir, first), nil), "all selected shards must retain manifest symlink rejection")
}

func TestDeletionMainSymlinkVerificationContract(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "weights.gguf")
	writeModel(t, dir, "weights.gguf", "weights")
	require.NoError(t, integrity.WriteManifest(dir, &integrity.ModelManifest{Version: 1, Files: []integrity.FileChecksum{{RelativePath: "weights.gguf", Size: 7}}}))
	outside := filepath.Join(t.TempDir(), "outside.gguf")
	require.NoError(t, os.WriteFile(outside, []byte("weights"), 0600))
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.Symlink(outside, file))
	require.Error(t, VerifyLocalWeights(t.Context(), file, nil), "the original manifest must retain selected-file type validation")
}

func writeModel(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestVerifyLocalWeights(t *testing.T) {
	t.Run("only the launched GGUF set is required", func(t *testing.T) {
		for _, missing := range []string{"mmproj-F16.gguf", "model-Q3_K_M.gguf", "Q4_K_M/model-Q4_K_M-00002-of-00002.gguf"} {
			t.Run(missing, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("ZZROUTER_MODEL_DIR", root)
				dir := filepath.Join(root, "org", "model")
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "Q4_K_M"), 0o755))
				shard := writeModel(t, dir, "Q4_K_M/model-Q4_K_M-00001-of-00002.gguf", "first shard")
				writeModel(t, dir, "Q4_K_M/model-Q4_K_M-00002-of-00002.gguf", "second shard")
				writeModel(t, dir, "model-Q3_K_M.gguf", "sibling weights")
				writeModel(t, dir, "mmproj-F16.gguf", "projector")
				manifest, err := integrity.CreateManifest(t.Context(), dir, "org/model", "")
				require.NoError(t, err)
				for i := range manifest.Files {
					if manifest.Files[i].RelativePath == "mmproj-F16.gguf" {
						manifest.Files[i].Feature = "vision"
					}
				}
				require.NoError(t, integrity.WriteManifest(dir, manifest))
				require.NoError(t, os.Remove(filepath.Join(dir, missing)))
				err = VerifyLocalWeights(t.Context(), shard, nil)
				if missing == "Q4_K_M/model-Q4_K_M-00002-of-00002.gguf" {
					require.ErrorIs(t, err, ErrModelIncomplete)
				} else {
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("no manifest is not an error", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "weights")
		require.NoError(t, VerifyLocalWeights(t.Context(), dir, nil))
	})

	t.Run("complete download passes", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "weights")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))

		require.NoError(t, VerifyLocalWeights(t.Context(), dir, nil))
	})

	t.Run("directory weights do not require unused feature files", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "weights")
		require.NoError(t, integrity.WriteManifest(dir, &integrity.ModelManifest{Version: 1,
			Files: []integrity.FileChecksum{
				{RelativePath: "model.safetensors", Size: 7},
				{RelativePath: "mmproj-F16.gguf", Size: 9, Feature: "vision"},
			}}))
		require.NoError(t, VerifyLocalWeights(t.Context(), dir, nil))
	})

	t.Run("truncated download is named as such", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "the full weights")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))

		// Interrupted download: the file exists but is short.
		writeModel(t, dir, "model.safetensors", "trunc")

		err = VerifyLocalWeights(t.Context(), dir, nil)
		require.ErrorIs(t, err, ErrModelIncomplete)
	})

	t.Run("a file path is verified against its directory", func(t *testing.T) {
		dir := t.TempDir()
		gguf := writeModel(t, dir, "model-Q4_K_M.gguf", "GGUF weights")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))
		writeModel(t, dir, "model-Q4_K_M.gguf", "short")

		require.ErrorIs(t, VerifyLocalWeights(t.Context(), gguf, nil), ErrModelIncomplete)
	})

	t.Run("a shard in a quant subdirectory is verified against the model", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ZZROUTER_MODEL_DIR", root)
		dir := filepath.Join(root, "org", "model")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Mkdir(filepath.Join(dir, "Q8_0"), 0o755))
		shard := writeModel(t, dir, "Q8_0/m-Q8_0-00001-of-00002.gguf", "GGUF shard one")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))
		writeModel(t, dir, "Q8_0/m-Q8_0-00001-of-00002.gguf", "short")

		require.ErrorIs(t, VerifyLocalWeights(t.Context(), shard, nil), ErrModelIncomplete)
	})

	t.Run("a manifest above the models root covers nothing", func(t *testing.T) {
		above := t.TempDir()
		root := filepath.Join(above, "models")
		t.Setenv("ZZROUTER_MODEL_DIR", root)
		dir := filepath.Join(root, "org", "model")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		gguf := writeModel(t, dir, "m.gguf", "GGUF")
		require.NoError(t, integrity.WriteManifest(above, &integrity.ModelManifest{Version: 1,
			Files: []integrity.FileChecksum{{RelativePath: "missing.gguf", Size: 9}}}))

		require.NoError(t, VerifyLocalWeights(t.Context(), gguf, nil))
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		assert.NoError(t, VerifyLocalWeights(t.Context(), "", nil))
	})
}

func TestVerifyLocalWeights_RequiresWeights(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		manifest bool
		complete bool
	}{
		{"gemma metadata only", map[string]string{".gitattributes": "x", "README.md": "x", "chat_template.jinja": "x", "config.json": "{}", "generation_config.json": "{}"}, true, false},
		{"MLX metadata only", map[string]string{"README.md": "x", "chat_template.jinja": "x", "config.json": "{}", "generation_config.json": "{}"}, false, false},
		{"missing indexed shard", map[string]string{"config.json": "{}", "model-00001-of-00002.safetensors": "weights", "model.safetensors.index.json": `{"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}`}, true, false},
		{"complete indexed model", map[string]string{"config.json": "{}", "model-00001-of-00002.safetensors": "weights", "model-00002-of-00002.safetensors": "weights", "model.safetensors.index.json": `{"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}`}, true, true},
		{"missing GGUF shard without manifest", map[string]string{"model-00001-of-00002.gguf": "weights"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, contents := range c.files {
				writeModel(t, dir, name, contents)
			}
			if c.manifest {
				manifest, err := integrity.CreateManifest(t.Context(), dir, "org/model", "")
				require.NoError(t, err)
				require.NoError(t, integrity.WriteManifest(dir, manifest))
			}
			err := VerifyLocalWeights(t.Context(), dir, nil)
			if c.complete {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrModelIncomplete)
			}
		})
	}
}

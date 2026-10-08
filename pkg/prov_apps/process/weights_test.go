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

func writeModel(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestVerifyLocalWeights(t *testing.T) {
	t.Run("no manifest is not an error", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "weights")
		require.NoError(t, VerifyLocalWeights(t.Context(), dir))
	})

	t.Run("complete download passes", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "weights")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))

		require.NoError(t, VerifyLocalWeights(t.Context(), dir))
	})

	t.Run("truncated download is named as such", func(t *testing.T) {
		dir := t.TempDir()
		writeModel(t, dir, "model.safetensors", "the full weights")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))

		// Interrupted download: the file exists but is short.
		writeModel(t, dir, "model.safetensors", "trunc")

		err = VerifyLocalWeights(t.Context(), dir)
		require.ErrorIs(t, err, ErrModelIncomplete)
	})

	t.Run("a file path is verified against its directory", func(t *testing.T) {
		dir := t.TempDir()
		gguf := writeModel(t, dir, "model-Q4_K_M.gguf", "GGUF weights")
		manifest, err := integrity.CreateManifest(context.Background(), dir, "org/model", "")
		require.NoError(t, err)
		require.NoError(t, integrity.WriteManifest(dir, manifest))
		writeModel(t, dir, "model-Q4_K_M.gguf", "short")

		require.ErrorIs(t, VerifyLocalWeights(t.Context(), gguf), ErrModelIncomplete)
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

		require.ErrorIs(t, VerifyLocalWeights(t.Context(), shard), ErrModelIncomplete)
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

		require.NoError(t, VerifyLocalWeights(t.Context(), gguf))
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		assert.NoError(t, VerifyLocalWeights(t.Context(), ""))
	})
}

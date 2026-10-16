package modelregistry

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

func TestRegistry_ManifestRepositoryHasOneCatalogOwner(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files, names []string
	}{
		{"tensor", []string{"model.safetensors"}, []string{"model"}},
		{"tensor-shards", []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"}, []string{"model-00001-of-00002", "model-00002-of-00002"}},
		{"gguf", []string{"weights-Q4.gguf"}, []string{"weights-Q4"}},
		{"nested-gguf", []string{"Q8/weights-00001-of-00002.gguf", "Q8/weights-00002-of-00002.gguf"}, []string{"weights"}},
		{"marked-tensor", []string{"config.json", "model.safetensors"}, []string{"org/model"}},
		{"marked-gguf", []string{".gitattributes", "Q8/weights-00001-of-00002.gguf", "Q8/weights-00002-of-00002.gguf"}, []string{"weights"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			SetModelsRootDirOverride(root)
			t.Cleanup(ClearModelsRootDirOverride)
			r, err := NewRegistry(func() *config.AppsConfig { return &config.AppsConfig{} })
			require.NoError(t, err)
			t.Cleanup(r.Stop)
			dir := filepath.Join(root, "org/model")
			require.NoError(t, integrity.RecordPending(dir, "org/model", "", integrity.FileChecksum{RelativePath: tc.files[len(tc.files)-1], Size: 4}))
			models, err := r.ListAllModels()
			require.NoError(t, err)
			require.Len(t, models, 1)
			require.Equal(t, "org/model", models[0].Name)
			require.True(t, models[0].Incomplete)
			for _, name := range tc.files {
				path := filepath.Join(dir, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
				require.NoError(t, os.WriteFile(path, []byte("GGUF"), 0600))
				require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: name, Size: 4}))
			}
			// A completed feature is never a GGUF weights identity.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "projector.gguf"), []byte("GGUF"), 0600))
			require.NoError(t, integrity.RecordFiles(dir, "org/model", "", integrity.FileChecksum{RelativePath: "projector.gguf", Size: 4, Feature: "vision"}))
			r.InvalidateScanCache()
			models, err = r.ListAllModels()
			require.NoError(t, err)
			var names []string
			for _, model := range models {
				names = append(names, model.Name)
			}
			sort.Strings(names)
			require.Equal(t, tc.names, names, "repair must hand off without a repository or shard duplicate")
		})
	}
}

func TestRegistry_CompletedManifestPreservesListedNames(t *testing.T) {
	root := t.TempDir()
	SetModelsRootDirOverride(root)
	t.Cleanup(ClearModelsRootDirOverride)
	r, err := NewRegistry(func() *config.AppsConfig { return &config.AppsConfig{} })
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	for repo, files := range map[string][]string{
		"tensor": {"model.safetensors"}, "pytorch": {"pytorch_model.bin"},
		"gguf": {"weights.gguf"}, "nested": {"Q8/weights-00001-of-00002.gguf", "Q8/weights-00002-of-00002.gguf"},
		"marked": {"config.json", "model.safetensors"}, "marked-gguf": {".gitattributes", "weights.gguf"},
	} {
		dir := filepath.Join(root, "org", repo)
		for _, file := range files {
			path := filepath.Join(dir, file)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, []byte("GGUF"), 0600))
		}
	}
	before, err := r.ListAllModelsCopy()
	require.NoError(t, err)
	for _, model := range before {
		dir := filepath.Join(root, filepath.FromSlash(model.SourceID))
		require.NoError(t, integrity.RecordFiles(dir, model.SourceID, ""))
	}
	r.InvalidateScanCache()
	after, err := r.ListAllModels()
	require.NoError(t, err)
	require.Len(t, after, len(before))
	for i, model := range before {
		require.Equal(t, model.Name, after[i].Name)
		require.Equal(t, model.FullPath, after[i].FullPath)
		require.Equal(t, model.SourceID, after[i].SourceID)
	}
}

func TestDeleteSelectedFreezesRepositoryMaterializationDirectory(t *testing.T) {
	root := t.TempDir()
	SetModelsRootDirOverride(root)
	t.Cleanup(ClearModelsRootDirOverride)
	r, err := NewRegistry(func() *config.AppsConfig { return &config.AppsConfig{} })
	require.NoError(t, err)
	t.Cleanup(r.Stop)
	dir := filepath.Join(root, "org/model")
	first := filepath.Join(dir, "Q8/weights-00001-of-00002.gguf")
	second := filepath.Join(dir, "Q8/weights-00002-of-00002.gguf")
	require.NoError(t, os.MkdirAll(filepath.Dir(first), 0700))
	for _, path := range []string{first, second, filepath.Join(dir, "keep.txt")} {
		require.NoError(t, os.WriteFile(path, []byte("GGUF"), 0600))
	}
	target, err := r.SelectDeletion(&metadata.ModelMetadata{Name: "weights", FullPath: first, SourceRepo: metadata.SourceHuggingFace, SourceID: "org/model", Format: metadata.FormatGGUF})
	require.NoError(t, err)
	require.Len(t, target.Paths, 2)
	release, err := integrity.AcquireMaterialization(t.Context(), dir, true)
	require.NoError(t, err)
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	changed, err := r.DeleteSelected(t.Context(), target)
	require.False(t, changed)
	var refusal httperr.Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, 409, refusal.Status)
	require.Equal(t, string(httperr.CodeDownloadInProgress), refusal.Code)
	for _, path := range []string{first, second} {
		require.FileExists(t, path)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	changed, err = r.DeleteSelected(ctx, target)
	require.False(t, changed)
	require.ErrorIs(t, err, context.Canceled)
	unlock()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Q8/later.gguf"), []byte("GGUF"), 0600))
	changed, err = r.DeleteSelected(t.Context(), target)
	require.NoError(t, err)
	require.True(t, changed)
	require.NoFileExists(t, first)
	require.NoFileExists(t, second)
	require.FileExists(t, filepath.Join(dir, "Q8/later.gguf"))
	require.FileExists(t, filepath.Join(dir, "keep.txt"))
}

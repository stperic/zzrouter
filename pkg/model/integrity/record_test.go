package integrity

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Files recorded one at a time, from concurrent downloads, all land in the
// manifest; recording a path again replaces its entry.
func TestRecordFiles_AddsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for _, name := range []string{"a.gguf", "b.gguf", "c.gguf", "d.gguf"} {
		wg.Go(func() {
			assert.NoError(t, RecordFiles(dir, "org/m", "", FileChecksum{RelativePath: name, Size: 1}))
		})
	}
	wg.Wait()
	require.NoError(t, RecordFiles(dir, "org/m", "", FileChecksum{RelativePath: "a.gguf", Size: 5, Feature: "vision"}))

	m, err := ReadManifest(dir)
	require.NoError(t, err)
	require.Len(t, m.Files, 4)
	assert.Equal(t, int64(8), m.TotalSize)
	assert.Equal(t, "org/m", m.ModelName)
	for _, f := range m.Files {
		if f.RelativePath == "a.gguf" {
			assert.Equal(t, "vision", f.Feature)
		}
	}
}

// Hashing a directory again keeps what each file is for, and never counts
// a download still in progress as model content.
func TestCreateManifest_KeepsFeaturesAndSkipsPartials(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "w.gguf"), []byte("w"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mmproj.gguf"), []byte("p"), 0o600))
	partial, err := CreatePartial(filepath.Join(dir, "next.gguf"))
	require.NoError(t, err)
	require.NoError(t, partial.Close())
	require.NoError(t, RecordFiles(dir, "org/m", "", FileChecksum{RelativePath: "mmproj.gguf", Size: 1, Feature: "vision"}))

	m, err := CreateManifest(context.Background(), dir, "org/m", "")
	require.NoError(t, err)
	got := map[string]string{}
	for _, f := range m.Files {
		got[f.RelativePath] = f.Feature
	}
	assert.Equal(t, map[string]string{"w.gguf": "", "mmproj.gguf": "vision"}, got)
}

// Two downloads of one file write into different partials.
func TestCreatePartial_IsUnique(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "w.gguf")
	a, err := CreatePartial(dest)
	require.NoError(t, err)
	b, err := CreatePartial(dest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	assert.NotEqual(t, a.Name(), b.Name())
	assert.True(t, IsBookkeeping(filepath.Base(a.Name())))
}

// Holds is the one rule for "this file is here as planned": recorded at the
// planned size, and hash when one is given, and on disk at that size.
func TestHolds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "w.gguf"), []byte("weights"), 0o600))
	require.NoError(t, RecordFiles(dir, "org/m", "", FileChecksum{RelativePath: "w.gguf", Size: 7, SHA256: "abc"},
		FileChecksum{RelativePath: "gone.gguf", Size: 4}))
	m, err := ReadManifest(dir)
	require.NoError(t, err)

	for _, tc := range []struct {
		name, rel, sha string
		size           int64
		want           bool
	}{
		{"as recorded", "w.gguf", "", 7, true},
		{"hash given in any case", "w.gguf", " ABC ", 7, true},
		{"other hash", "w.gguf", "def", 7, false},
		{"other size", "w.gguf", "", 8, false},
		{"recorded but not on disk", "gone.gguf", "", 4, false},
		{"not recorded", "x.gguf", "", 1, false},
	} {
		_, ok := m.Holds(dir, tc.rel, tc.size, tc.sha)
		assert.Equal(t, tc.want, ok, tc.name)
	}
	var none *ModelManifest
	_, ok := none.Holds(dir, "w.gguf", 7, "")
	assert.False(t, ok, "no manifest holds nothing")
}

func TestMaterializationWaiterCancellation(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireMaterialization(t.Context(), dir)
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = AcquireMaterialization(ctx, dir)
	require.ErrorIs(t, err, context.Canceled)
	other, err := AcquireMaterialization(t.Context(), t.TempDir())
	require.NoError(t, err, "unrelated repository is not blocked")
	other()
	require.NoError(t, RecordFiles(dir, "org/model", "", FileChecksum{RelativePath: "model.gguf", Size: 1}), "manifest updates do not deadlock the materializer")
}

func TestMaterializationRootAliasesShareGate(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	release, err := AcquireMaterialization(t.Context(), filepath.Join(root, "org/model"))
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = AcquireMaterialization(ctx, filepath.Join(alias, "org/model"))
	require.ErrorIs(t, err, context.Canceled)
	direct, err := locksForDirectory(filepath.Join(root, "org/model"))
	require.NoError(t, err)
	aliased, err := locksForDirectory(filepath.Join(alias, "org/model"))
	require.NoError(t, err)
	assert.Same(t, direct, aliased)
}

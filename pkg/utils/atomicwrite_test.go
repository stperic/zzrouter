package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtomicWriteFile_CreatesFileWithContentAndPerm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	data := []byte(`{"k":1}`)
	require.NoError(t, AtomicWriteFile(path, data, 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	}
}

func TestAtomicWriteFileReplacementRefusalPreservesDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(path, []byte("previous"), 0o600))
	before, err := os.Stat(path)
	require.NoError(t, err)
	calls := 0
	err = atomicWriteFileWithRename(path, []byte("candidate"), 0o644, func(string, string) error {
		calls++
		return os.ErrPermission
	})
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Equal(t, 1, calls, "refused replacement must not remove and retry")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "previous", string(data))
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.Mode(), after.Mode())
	assert.True(t, os.SameFile(before, after))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "refused replacement cleans the temporary file")
}

func TestAtomicWriteFile_OverwritesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	require.NoError(t, AtomicWriteFile(path, []byte("old"), 0o644))
	require.NoError(t, AtomicWriteFile(path, []byte("new"), 0o644))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), got)
}

func TestAtomicWriteFile_RaisesPermFromTempDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm bits below owner-write are advisory on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// CreateTemp lands at 0600; verify chmod widens to 0644 before rename.
	require.NoError(t, AtomicWriteFile(path, []byte("x"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o644), info.Mode().Perm())
}

func TestAtomicWriteFile_LowersPermToSensitive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm bits below owner-write are advisory on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.yaml")

	require.NoError(t, AtomicWriteFile(path, []byte("k:v"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
}

func TestAtomicWriteFile_LeavesNoTempOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	require.NoError(t, AtomicWriteFile(path, []byte("x"), 0o600))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.Contains(e.Name(), ".tmp."),
			"unexpected temp leftover: %s", e.Name())
	}
}

func TestAtomicWriteFile_FailsCleanlyWhenParentDirMissing(t *testing.T) {
	t.Parallel()
	// CreateTemp fails before any tempfile is materialized — guards the
	// early-return path: no panic, no leak (parent doesn't exist to
	// leak into), error surfaced.
	bogus := filepath.Join(t.TempDir(), "missing", "state.json")
	err := AtomicWriteFile(bogus, []byte("x"), 0o600)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrPostRenameDurability),
		"missing-dir failure must NOT be ErrPostRenameDurability — that sentinel is post-rename only")

	_, statErr := os.Stat(filepath.Dir(bogus))
	assert.True(t, os.IsNotExist(statErr))
}

func TestAtomicWriteFile_RenameReplacesInodeNotInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode semantics are POSIX-specific")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	require.NoError(t, AtomicWriteFile(path, []byte("v1"), 0o600))
	infoBefore, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, AtomicWriteFile(path, []byte("v2"), 0o600))
	infoAfter, err := os.Stat(path)
	require.NoError(t, err)

	// os.SameFile compares the underlying inode/device. Equal here
	// would mean we wrote in place rather than rename-replaced — that
	// would defeat atomicity (a partial write would be observable).
	assert.False(t, os.SameFile(infoBefore, infoAfter),
		"second write must rename-replace (new inode), not rewrite the original file")
}

func TestAtomicWriteFile_ConcurrentWritersLeaveNoTempLeftover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			payload := []byte{byte(i)}
			_ = AtomicWriteFile(path, payload, 0o600)
		}()
	}
	wg.Wait()

	// Last-writer-wins is the documented semantics; we only assert
	// here that no .tmp.* sibling survived the race. A leftover would
	// mean an error path skipped its cleanup.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.Contains(e.Name(), ".tmp."),
			"unexpected temp leftover under concurrent writers: %s", e.Name())
	}
}

func TestAtomicWriteFile_ReplacesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test requires elevated perms on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")

	require.NoError(t, os.WriteFile(target, []byte("real-original"), 0o600))
	require.NoError(t, os.Symlink(target, link))

	// Write through the symlink path. Documented semantics: the symlink
	// is REPLACED with a regular file (rename-over-symlink), not
	// followed. This is the standard behavior of rename(2) and is the
	// correct semantic for atomic state-file writes — following the
	// link would defeat atomicity if the target lives on a different
	// filesystem.
	require.NoError(t, AtomicWriteFile(link, []byte("new"), 0o600))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Zero(t, info.Mode()&os.ModeSymlink, "link must be replaced with a regular file")

	// The original target file is untouched.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("real-original"), got)
}

func TestAtomicWriteFile_OverwritePreservesAtomicityOnReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Write twice with different sizes: the second write must replace,
	// not append, and must leave no .tmp.* sibling.
	require.NoError(t, AtomicWriteFile(path, []byte("aaaa"), 0o600))
	require.NoError(t, AtomicWriteFile(path, []byte("b"), 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), got, "second write must fully replace, not append")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.Contains(e.Name(), ".tmp."),
			"unexpected temp leftover after replace: %s", e.Name())
	}
}

package assets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"qwen3.8-system-anywhere.jinja", "a", "A_b-1.json", strings.Repeat("x", MaxNameLen)} {
		assert.NoError(t, ValidateName(ok), ok)
	}
	for _, bad := range []string{
		"", "../x", "a/b", `a\b`, ".hidden", "-dash", "_under", "x y", "é.jinja",
		strings.Repeat("x", MaxNameLen+1), "..", ".", "a.", "a\n", "a\x00", "a:b", "a\uff0fb",
	} {
		assert.ErrorIs(t, ValidateName(bad), ErrInvalidName, "%q", bad)
	}
}

func TestDir_WriteReadListDelete(t *testing.T) {
	d := NewDir(t.TempDir())

	created, err := d.Write("b.jinja", []byte("bee"))
	require.NoError(t, err)
	assert.True(t, created)
	created, err = d.Write("b.jinja", []byte("bee2"))
	require.NoError(t, err)
	assert.False(t, created, "a second write replaces")
	_, err = d.Write("a.jinja", []byte("ay"))
	require.NoError(t, err)

	got, err := d.Read("b.jinja")
	require.NoError(t, err)
	assert.Equal(t, "bee2", string(got))

	infos, err := d.List()
	require.NoError(t, err)
	require.Len(t, infos, 2)
	assert.Equal(t, "a.jinja", infos[0].Name, "sorted by name")
	assert.Equal(t, Digest([]byte("bee2")), infos[1].SHA256)
	assert.Equal(t, int64(4), infos[1].Size)

	p, err := d.Path("a.jinja")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(p))

	require.NoError(t, d.Delete("a.jinja"))
	_, err = d.Read("a.jinja")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, d.Delete("a.jinja"), ErrNotFound)
}

// A name resolves only to a regular file written into the directory: a
// symlink planted there would let a name reach any file the engine's user
// can read.
func TestDir_PathRefusesSymlinkAndDirectory(t *testing.T) {
	providerDir := t.TempDir()
	d := NewDir(providerDir)
	_, err := d.Write("real.jinja", []byte("x"))
	require.NoError(t, err)

	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("s"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(providerDir, DirName, "link.jinja")))
	require.NoError(t, os.Mkdir(filepath.Join(providerDir, DirName, "sub"), 0o755))

	_, err = d.Path("link.jinja")
	assert.ErrorIs(t, err, ErrNotRegular)
	_, err = d.Read("link.jinja")
	assert.ErrorIs(t, err, ErrNotRegular)
	_, err = d.Path("sub")
	assert.ErrorIs(t, err, ErrNotRegular)

	infos, err := d.List()
	require.NoError(t, err)
	require.Len(t, infos, 1, "a symlink, a directory and a hidden temp file are not assets")
	assert.Equal(t, "real.jinja", infos[0].Name)
}

func TestDir_ListSkipsOrphanedTemp(t *testing.T) {
	providerDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(providerDir, DirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(providerDir, DirName, ".x.jinja.tmp.123"), []byte("half"), 0o600))

	infos, err := NewDir(providerDir).List()
	require.NoError(t, err)
	assert.Empty(t, infos)
}

func TestDir_SizeCaps(t *testing.T) {
	d := NewDir(t.TempDir())

	_, err := d.Write("big", make([]byte, MaxAssetBytes+1))
	assert.ErrorIs(t, err, ErrTooLarge)

	for i := range MaxProviderBytes / MaxAssetBytes {
		_, err := d.Write(string(rune('a'+i)), make([]byte, MaxAssetBytes))
		require.NoError(t, err)
	}
	_, err = d.Write("one-more", []byte("x"))
	assert.ErrorIs(t, err, ErrProviderTooLarge)
	_, err = d.Write("a", make([]byte, MaxAssetBytes))
	assert.NoError(t, err, "replacing an asset counts its new size, not both")
}

func TestDir_ReplaceAndMatches(t *testing.T) {
	d := NewDir(t.TempDir())
	_, err := d.Write("keep", []byte("old"))
	require.NoError(t, err)
	_, err = d.Write("drop", []byte("gone"))
	require.NoError(t, err)

	want := map[string][]byte{"keep": []byte("new"), "add": []byte("fresh")}
	assert.False(t, d.Matches(want))
	require.NoError(t, d.Replace(want))
	assert.True(t, d.Matches(want))

	all, err := d.ReadAll()
	require.NoError(t, err)
	assert.Equal(t, want, all)

	assert.True(t, NewDir(t.TempDir()).Matches(map[string][]byte{}), "no directory holds no assets")
}

// A bad entry anywhere in the set is refused before anything changes.
func TestDir_ReplaceValidatesFirst(t *testing.T) {
	d := NewDir(t.TempDir())
	_, err := d.Write("keep", []byte("old"))
	require.NoError(t, err)

	assert.ErrorIs(t, d.Replace(map[string][]byte{"ok": []byte("x"), "../evil": []byte("y")}), ErrInvalidName)
	assert.ErrorIs(t, d.Replace(map[string][]byte{"ok": make([]byte, MaxAssetBytes+1)}), ErrTooLarge)

	all, err := d.ReadAll()
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"keep": []byte("old")}, all)
}

// A symlinked assets/ would turn every name into a path under its target.
func TestDir_RefusesSymlinkedDirectory(t *testing.T) {
	providerDir := t.TempDir()
	elsewhere := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(elsewhere, "passwd"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(providerDir, DirName)))
	d := NewDir(providerDir)

	_, err := d.Path("passwd")
	assert.ErrorIs(t, err, ErrNotRegular)
	_, err = d.ReadAll()
	assert.ErrorIs(t, err, ErrNotRegular)
	_, err = d.Write("new", []byte("x"))
	assert.ErrorIs(t, err, ErrNotRegular)
	_, statErr := os.Stat(filepath.Join(elsewhere, "new"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing written through the link")
}

// A file placed by hand is held to the caps a write enforces, since every
// read of it rides a sync push.
func TestDir_CapsHandPlacedFiles(t *testing.T) {
	providerDir := t.TempDir()
	dir := filepath.Join(providerDir, DirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "huge"), make([]byte, MaxAssetBytes+1), 0o600))
	d := NewDir(providerDir)

	_, err := d.Path("huge")
	assert.ErrorIs(t, err, ErrTooLarge)
	_, err = d.ReadAll()
	assert.ErrorIs(t, err, ErrTooLarge)

	require.NoError(t, os.Remove(filepath.Join(dir, "huge")))
	for i := range MaxProviderBytes/MaxAssetBytes + 1 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, string(rune('a'+i))), make([]byte, MaxAssetBytes), 0o600))
	}
	_, err = d.ReadAll()
	assert.ErrorIs(t, err, ErrProviderTooLarge)
}

// A replace must clear a stray that breaks the caps rather than be
// blocked by it: the file that fails a read is the file being removed.
func TestDir_ReplaceRemovesOversizedStray(t *testing.T) {
	providerDir := t.TempDir()
	dir := filepath.Join(providerDir, DirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray"), make([]byte, MaxAssetBytes+1), 0o600))
	d := NewDir(providerDir)

	want := map[string][]byte{"keep": []byte("x")}
	require.NoError(t, d.Replace(want))
	assert.True(t, d.Matches(want))
}

func TestValidateSet_CountCap(t *testing.T) {
	set := map[string][]byte{}
	for i := range MaxAssets + 1 {
		set[fmt.Sprintf("a%d", i)] = nil
	}
	err := ValidateSet(set)
	assert.ErrorIs(t, err, ErrTooMany)
	assert.ErrorIs(t, err, ErrInvalidSet)
	assert.ErrorIs(t, ValidateSet(map[string][]byte{"../x": nil}), ErrInvalidSet)
}

// The zero Dir holds nothing: with an empty path, a name would otherwise
// resolve against the process's working directory.
func TestDir_ZeroValueHoldsNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("present.jinja", []byte("x"), 0o600))

	var d Dir
	_, err := d.Path("present.jinja")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = d.ReadAll()
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = d.Write("new.jinja", []byte("x"))
	assert.ErrorIs(t, err, ErrNotFound)
	_, statErr := os.Stat("new.jinja")
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// A file over the caps blocks every write and sync of its provider, so
// deleting it must not be blocked by the same cap.
func TestDir_DeletesOversizedFile(t *testing.T) {
	providerDir := t.TempDir()
	dir := filepath.Join(providerDir, DirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "huge"), make([]byte, MaxAssetBytes+1), 0o600))
	d := NewDir(providerDir)

	_, err := d.Write("small", []byte("x"))
	require.ErrorIs(t, err, ErrTooLarge, "the stray blocks writes")
	require.NoError(t, d.Exists("huge"))

	require.NoError(t, d.Delete("huge"))
	_, err = d.Write("small", []byte("x"))
	assert.NoError(t, err, "removing it unblocks the provider")
}

func TestValidateName_WindowsDeviceNames(t *testing.T) {
	for _, name := range []string{"nul", "NUL", "con.jinja", "Com1.txt", "lpt9.json", "aux"} {
		assert.ErrorIs(t, ValidateName(name), ErrInvalidName, name)
	}
	for _, name := range []string{"null", "console.jinja", "com10", "lpt0.json", "my-con.jinja"} {
		assert.NoError(t, ValidateName(name), name)
	}
}

// Names one case-insensitive filesystem would store as a single file are
// refused, both as a whole set and as one write beside existing assets.
func TestNamesDifferingOnlyInCase(t *testing.T) {
	err := ValidateSet(map[string][]byte{"T.jinja": []byte("a"), "t.jinja": []byte("b")})
	assert.ErrorIs(t, err, ErrInvalidSet)
	assert.ErrorIs(t, err, ErrNameCollision)

	d := NewDir(t.TempDir())
	_, err = d.Write("t.jinja", []byte("a"))
	require.NoError(t, err)
	_, err = d.Write("T.jinja", []byte("b"))
	assert.ErrorIs(t, err, ErrNameCollision)
	_, err = d.Write("t.jinja", []byte("c"))
	assert.NoError(t, err, "replacing the same name is not a collision")

	// A pair placed by hand on a case-sensitive coordinator is refused at
	// read, so it is never pushed to a worker that would store one file.
	if _, err := os.Lstat(filepath.Join(d.path, "T.jinja")); err == nil {
		t.Skip("case-insensitive filesystem: two names differing only in case cannot both exist")
	}
	require.NoError(t, os.WriteFile(filepath.Join(d.path, "T.jinja"), []byte("b"), 0o600))
	_, err = d.ReadAll()
	assert.ErrorIs(t, err, ErrNameCollision)
	require.NoError(t, d.Delete("T.jinja"), "the stray can still be removed")
}

// A name resolves only as spelled on disk. On a case-insensitive
// coordinator "QWEN.jinja" would otherwise stat as "qwen.jinja", pass
// validation there, and then miss on a case-sensitive worker.
func TestLookupIsCaseExact(t *testing.T) {
	d := NewDir(t.TempDir())
	_, err := d.Write("qwen.jinja", []byte("a"))
	require.NoError(t, err)

	_, err = d.Path("QWEN.jinja")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, d.Exists("QWEN.jinja"), ErrNotFound)
	_, err = d.Read("QWEN.jinja")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, d.Delete("QWEN.jinja"), ErrNotFound)
	require.NoError(t, d.Exists("qwen.jinja"), "the wrong-case delete removed nothing")
}

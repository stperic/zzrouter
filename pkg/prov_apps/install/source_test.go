package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sourceEntry struct {
	name, body, link string
	typeflag         byte
	mode, size       int64
}

type cancelExtractionContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelExtractionContext) Err() error {
	c.checks++
	if c.checks == 6 {
		c.cancel()
	}
	return c.Context.Err()
}

func sourceArchive(t *testing.T, entries ...sourceEntry) []byte {
	t.Helper()
	if len(entries) > 0 {
		root, _, _ := strings.Cut(entries[0].name, "/")
		found := false
		for _, entry := range entries {
			found = found || entry.name == root+"/requirements.txt"
		}
		if !found {
			entries = append([]sourceEntry{{name: root + "/requirements.txt", body: "torch\n"}}, entries...)
		}
	}
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	w := tar.NewWriter(gz)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0644
		}
		size := int64(len(entry.body))
		if entry.size > size {
			size = entry.size
		}
		require.NoError(t, w.WriteHeader(&tar.Header{Name: entry.name, Mode: mode, Size: size, Typeflag: entry.typeflag, Linkname: entry.link}))
		_, err := w.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	_ = w.Close() // Oversized-entry fixtures deliberately omit their declared body.
	require.NoError(t, gz.Close())
	return data.Bytes()
}

func extractFixture(t *testing.T, data []byte) (string, config.InstallSource, string) {
	t.Helper()
	parent := t.TempDir()
	archive := filepath.Join(parent, "release.tar.gz")
	require.NoError(t, os.WriteFile(archive, data, 0600))
	return archive, config.InstallSource{SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), StripComponents: 1, Requirements: "requirements.txt"}, parent
}

func extractSource(ctx context.Context, archive, candidate, directory string, source config.InstallSource) error {
	_, err := ReadSource(ctx, archive, candidate, directory, source)
	return err
}

func TestSourceExtractionFreshTreeAndPinBeforeEntries(t *testing.T) {
	data := sourceArchive(t, sourceEntry{name: "release/", typeflag: tar.TypeDir}, sourceEntry{name: "release/main.py", body: "print('help')"}, sourceEntry{name: "release/deps/requirements.txt", body: "torch\n"})
	archive, source, parent := extractFixture(t, data)
	require.NoError(t, extractSource(t.Context(), archive, parent, "payload/source", source))
	body, err := os.ReadFile(filepath.Join(parent, "payload/source/deps/requirements.txt"))
	require.NoError(t, err)
	assert.Equal(t, "torch\n", string(body))
	require.Error(t, extractSource(t.Context(), archive, parent, "payload/source", source), "existing trees cannot be reused")
	body, err = os.ReadFile(filepath.Join(parent, "payload/source/main.py"))
	require.NoError(t, err)
	assert.Equal(t, "print('help')", string(body))
	// A non-gzip body discriminates hash-first ordering from gzip/header parsing.
	archive, source, parent = extractFixture(t, []byte("not even gzip"))
	source.SHA256 = strings.Repeat("a", 64)
	require.ErrorContains(t, extractSource(t.Context(), archive, parent, "source", source), "checksum mismatch")
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "checksum failure cannot create an extraction directory")
}

func TestSourceExtractionRefusesHostileEntriesAndCleansCandidate(t *testing.T) {
	good := sourceEntry{name: "release/main.py", body: "old code"}
	for name, entry := range map[string]sourceEntry{
		"absolute":        {name: "/escape"},
		"parent":          {name: "release/../escape"},
		"stripped parent": {name: "../escape"},
		"dot":             {name: "./release/file"},
		"drive":           {name: "C:/release/file"},
		"backslash":       {name: `release\escape`},
		"symlink outside": {name: "release/link", typeflag: tar.TypeSymlink, link: "../../escape"},
		"symlink inside":  {name: "release/link", typeflag: tar.TypeSymlink, link: "main.py"},
		"hardlink":        {name: "release/link", typeflag: tar.TypeLink, link: "release/main.py"},
		"device":          {name: "release/device", typeflag: tar.TypeChar},
		"fifo":            {name: "release/fifo", typeflag: tar.TypeFifo},
		"setuid":          {name: "release/root", mode: 04755},
		"setgid":          {name: "release/group", mode: 02755},
		"sticky":          {name: "release/sticky", mode: 01755},
		"duplicate":       good,
		"two roots":       {name: "other/file"},
		"file root":       {name: "release"},
		"file ancestor":   {name: "release/main.py/file"},
		"oversized file":  {name: "release/big", size: sourceFileLimit + 1},
		"oversized name":  {name: "release/" + strings.Repeat("x", 1025)},
	} {
		t.Run(name, func(t *testing.T) {
			archive, source, parent := extractFixture(t, sourceArchive(t, good, entry))
			require.Error(t, extractSource(t.Context(), archive, parent, "source", source))
			_, err := os.Lstat(filepath.Join(parent, "source"))
			require.True(t, os.IsNotExist(err), "partial candidate must be removed")
			_, err = os.Lstat(filepath.Join(parent, "escape"))
			require.True(t, os.IsNotExist(err), "entries cannot escape the candidate")
		})
	}
}

func TestSourceExtractionBoundsIntegrityAndCancellation(t *testing.T) {
	valid := sourceArchive(t, sourceEntry{name: "root/file", body: "contents"})
	for name, data := range map[string][]byte{"truncated body": sourceArchive(t, sourceEntry{name: "root/file", body: "short", size: 100}), "empty": sourceArchive(t)} {
		t.Run(name, func(t *testing.T) {
			archive, source, parent := extractFixture(t, data)
			require.Error(t, extractSource(t.Context(), archive, parent, "source", source))
			_, err := os.Lstat(filepath.Join(parent, "source"))
			require.True(t, os.IsNotExist(err))
		})
	}
	archive, source, parent := extractFixture(t, valid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, extractSource(ctx, archive, parent, "source", source), context.Canceled)
	file, err := os.OpenFile(archive, os.O_WRONLY, 0600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(SourceArchiveLimit+1))
	require.NoError(t, file.Close())
	require.ErrorContains(t, extractSource(t.Context(), archive, parent, "source", source), "invalid source archive")
	// An existing symlink parent cannot redirect writes outside the owned candidate.
	archive, source, parent = extractFixture(t, valid)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(parent, "linked")))
	require.Error(t, extractSource(t.Context(), archive, parent, "linked/source", source))
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSourceExtractionBoundsEntryCount(t *testing.T) {
	entries := []sourceEntry{}
	for n := 0; n <= sourceEntryLimit; n++ {
		entries = append(entries, sourceEntry{name: fmt.Sprintf("root/%d", n), typeflag: tar.TypeDir})
	}
	archive, source, parent := extractFixture(t, sourceArchive(t, entries...))
	require.Error(t, extractSource(t.Context(), archive, parent, "source", source))
	_, err := os.Stat(filepath.Join(parent, "source"))
	require.True(t, os.IsNotExist(err))

}

func TestSourceExtractionCancellationRemovesPartialTree(t *testing.T) {
	archive, source, parent := extractFixture(t, sourceArchive(t, sourceEntry{name: "root/file", body: strings.Repeat("x", 1<<20)}))
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &cancelExtractionContext{Context: base, cancel: cancel}
	require.ErrorIs(t, extractSource(ctx, archive, parent, "source", source), context.Canceled)
	require.GreaterOrEqual(t, ctx.checks, 6, "cancellation occurs during streamed extraction")
	_, err := os.Lstat(filepath.Join(parent, "source"))
	require.True(t, os.IsNotExist(err))
}

func TestSourceRequirementsAreStrictApprovedRoots(t *testing.T) {
	roots, err := parseSourceRequirements([]byte("# release roots\n\nTorch\nMy_Package>=1.2,!=1.3.*\r\nnumpy~=2.0\n"))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"torch": "", "my-package": ">=1.2,!=1.3.*", "numpy": "~=2.0"}, roots)
	for _, line := range []string{"", "# only comments", "--index-url https://evil.test", "-r another.txt", "-e ./code", "engine @ https://evil.test/code.whl", "https://evil.test", "../code", "engine[extra]", "engine; python_version > '3'", "engine>=1 --hash=sha256:abc", "engine==", "engine\nENGINE", "my_package\nmy-package"} {
		_, err := parseSourceRequirements([]byte(line))
		require.Error(t, err, line)
	}
	roots, err = parseSourceRequirements([]byte("engine>=1\n"))
	require.NoError(t, err)
	report := `{"version":"1","install":[{"metadata":{"name":"engine","version":"1.5"},"requested":true,"download_info":{"url":"https://files.pythonhosted.org/engine.whl","archive_info":{"hashes":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}]}`
	inventory, err := ParsePipReport([]byte(report), roots)
	require.NoError(t, err)
	assert.Equal(t, "engine==1.5 --hash=sha256:"+strings.Repeat("a", 64)+"\n", InventoryRequirements(inventory))
	_, err = ParsePipReport([]byte(strings.Replace(report, `"name":"engine"`, `"name":"other"`, 1)), roots)
	require.ErrorContains(t, err, "unexpected root")
	_, err = ParsePipReport([]byte(strings.Replace(report, strings.Repeat("a", 64), "", 1)), roots)
	require.ErrorContains(t, err, "missing SHA256")
}

func TestSourceRequirementsUseWholeVerifiedArchive(t *testing.T) {
	valid := sourceArchive(t, sourceEntry{name: "root/requirements.txt", body: "torch>=2\n"})
	archive, source, parent := extractFixture(t, valid)
	roots, err := ReadSource(t.Context(), archive, "", "", source)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"torch": ">=2"}, roots)
	files, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, files, 1, "requirements reading creates no extraction tree")
	for name, entries := range map[string][]sourceEntry{
		"unsafe later entry": {{name: "root/requirements.txt", body: "torch\n"}, {name: "root/../escape"}},
		"directory member":   {{name: "root/requirements.txt", typeflag: tar.TypeDir}},
		"oversized member":   {{name: "root/requirements.txt", size: (64 << 10) + 1}},
		"duplicate member":   {{name: "root/requirements.txt", body: "torch\n"}, {name: "root/requirements.txt", body: "numpy\n"}},
		"invalid roots":      {{name: "root/requirements.txt", body: "--index-url https://evil.test\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			archive, source, parent := extractFixture(t, sourceArchive(t, entries...))
			_, err := ReadSource(t.Context(), archive, "", "", source)
			require.Error(t, err)
			require.Error(t, extractSource(t.Context(), archive, parent, "source", source))
			_, err = os.Stat(filepath.Join(parent, "source"))
			require.True(t, os.IsNotExist(err))
		})
	}
	// Integrity of the complete compressed bytes belongs to the release SHA pin.
	for _, corrupted := range [][]byte{valid[:len(valid)-4], append(bytes.Clone(valid), 'x')} {
		require.NoError(t, os.WriteFile(archive, corrupted, 0600))
		_, err := ReadSource(t.Context(), archive, "", "", source)
		require.ErrorContains(t, err, "checksum mismatch")
	}
}

func TestSourceExtractionRefusesSparseExpansionBeforeWriting(t *testing.T) {
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	// Global PAX retains sparse keys. Convert it to a local PAX header below.
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "pax", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{
		"GNU.sparse.major": "0", "GNU.sparse.minor": "1", "GNU.sparse.numblocks": "1", "GNU.sparse.map": "0,1", "GNU.sparse.size": "1024",
	}}))
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "root/sparse", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}))
	_, err := writer.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "root/requirements.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 6}))
	_, err = writer.Write([]byte("torch\n"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	data := raw.Bytes()
	data[156] = tar.TypeXHeader
	for n := 148; n < 156; n++ {
		data[n] = ' '
	}
	checksum := 0
	for _, value := range data[:512] {
		checksum += int(value)
	}
	copy(data[148:156], fmt.Sprintf("%06o\x00 ", checksum))
	reader := tar.NewReader(bytes.NewReader(data))
	header, err := reader.Next()
	require.NoError(t, err)
	require.Equal(t, byte(tar.TypeReg), header.Typeflag)
	require.Equal(t, int64(1024), header.Size, "Go synthesizes bytes outside the bounded gzip reader")
	require.Contains(t, header.PAXRecords, "GNU.sparse.map")
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Len(t, body, 1024)
	var compressed bytes.Buffer
	zipped := gzip.NewWriter(&compressed)
	_, err = zipped.Write(data)
	require.NoError(t, err)
	require.NoError(t, zipped.Close())
	archive, source, parent := extractFixture(t, compressed.Bytes())
	_, err = ReadSource(t.Context(), archive, "", "", source)
	require.ErrorContains(t, err, "unsafe source archive")
	require.ErrorContains(t, extractSource(t.Context(), archive, parent, "source", source), "unsafe source archive")
	_, err = os.Stat(filepath.Join(parent, "source"))
	require.True(t, os.IsNotExist(err), "sparse entries cannot write amplified files")
}

package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/security"
)

// SourceArchiveLimit bounds downloads of code/assets, excluding model weights.
const SourceArchiveLimit = 64 << 20
const sourceExpandedLimit, sourceFileLimit, sourceEntryLimit = 512 << 20, 64 << 20, 10000

// ReadSource verifies the archive before reading entries and returns its declared roots.
// A nonempty candidate also extracts into a fresh, confined directory beneath it.
func ReadSource(ctx context.Context, archive, candidate, directory string, source config.InstallSource) (roots map[string]string, resultErr error) {
	info, err := os.Lstat(archive)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > SourceArchiveLimit {
		return nil, fmt.Errorf("invalid source archive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := security.VerifySHA256(archive, source.SHA256); err != nil {
		return nil, err
	}
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer zipped.Close()
	var root *os.Root
	if candidate != "" {
		root, err = os.OpenRoot(candidate)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := root.MkdirAll(path.Dir(directory), 0700); err != nil {
			return nil, err
		}
		if err := root.Mkdir(directory, 0700); err != nil {
			return nil, err
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, root.RemoveAll(directory))
			}
		}()
	}
	limited := &io.LimitedReader{R: sourceReader{ctx, zipped}, N: sourceExpandedLimit + 1}
	reader := tar.NewReader(limited)
	var data []byte
	top := ""
	for count := 0; ; count++ {
		h, err := reader.Next()
		if err == io.EOF && limited.N > 0 {
			break
		}
		if err != nil {
			return nil, err
		}
		first, relative, safe := sourceEntryPath(h, top, count, limited.N)
		if !safe {
			return nil, fmt.Errorf("unsafe source archive entry %q", h.Name)
		}
		top = first
		var body io.Reader = reader
		if relative == source.Requirements {
			if h.Typeflag != tar.TypeReg || h.Size > 64<<10 || data != nil {
				return nil, fmt.Errorf("invalid requirements member")
			}
			data, err = io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(data)
		}
		if root == nil || relative == "" {
			continue
		}
		target := path.Join(directory, relative)
		dir := path.Dir(target)
		if h.Typeflag == tar.TypeDir {
			dir = target
		}
		if err := root.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		out, err := root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600|os.FileMode(h.Mode&0100))
		if err != nil {
			return nil, err
		}
		_, copyErr := io.CopyN(out, body, h.Size)
		if err := errors.Join(copyErr, out.Close()); err != nil {
			return nil, err
		}
	}
	return parseSourceRequirements(data)
}

func sourceEntryPath(h *tar.Header, top string, count int, remaining int64) (string, string, bool) {
	name := strings.TrimSuffix(h.Name, "/")
	first, relative, nested := strings.Cut(name, "/")
	for key := range h.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return "", "", false
		}
	}
	safe := count < sourceEntryLimit && remaining > 0 && len(name) <= 1024 && fs.ValidPath(name) && !strings.ContainsAny(name, "\\:") && h.Mode & ^int64(0777) == 0 && h.Size >= 0 && h.Size <= sourceFileLimit && (h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeReg) && (top == "" || first == top) && (nested || h.Typeflag == tar.TypeDir)
	return first, relative, safe
}

type sourceReader struct {
	ctx context.Context
	io.Reader
}

func (r sourceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func parseSourceRequirements(data []byte) (map[string]string, error) {
	roots := map[string]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, constraint := line, ""
		if at := strings.IndexAny(line, "<>=!~ \t"); at >= 0 {
			name, constraint = line[:at], strings.TrimSpace(line[at:])
		}
		name = strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(name))
		_, duplicate := roots[name]
		if err := config.ValidateSpecifier(constraint); err != nil || !config.ValidDistribution(name) || duplicate || len(roots) >= 512 {
			return nil, fmt.Errorf("source requirements: invalid or duplicate root %q", line)
		}
		roots[name] = constraint
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("source requirements are empty")
	}
	return roots, nil
}

package huggingface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// progressInterval throttles progress callbacks on fast links.
const progressInterval = 100 * time.Millisecond

// planned is one file a download fetches, and the feature it belongs to.
type planned struct {
	file    metadata.TreeFileEntry
	feature string
}

// Download fetches what req names from a repository into the model's
// directory, adding to what is there. Each file is written beside its
// destination and renamed into place once complete and verified, then
// recorded in the integrity manifest with the feature it belongs to. A file
// the manifest already holds at the listed size and hash is not fetched
// again, and nothing already in the directory is removed.
func (hfc *Connector) Download(ctx context.Context, req metadata.DownloadRequest, progress func(status string, done, total int64)) error {
	var files []planned
	if req.Files != nil {
		for _, f := range req.Files {
			if err := validateRepoFileName(f.Name); err != nil {
				return err
			}
			files = append(files, planned{file: f.TreeFileEntry, feature: f.Feature})
		}
	} else {
		listing, err := hfc.getRepoFiles(ctx, req.Repo)
		if err != nil {
			return fmt.Errorf("failed to get repo files: %w", err)
		}
		files, err = plan(listing, req)
		if err != nil {
			return err
		}
	}

	dir := filepath.Join(hfc.modelsDir, filepath.FromSlash(req.Repo))
	release, err := integrity.AcquireMaterialization(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	manifest, err := integrity.ReadManifest(dir)
	if err != nil {
		return err
	}
	var held []integrity.FileChecksum
	if !req.Force {
		files, held = missing(files, manifest, dir)
	}
	if len(held) > 0 {
		if err := integrity.RecordFiles(dir, req.Repo, hfc.downloadBase+req.Repo, held...); err != nil {
			return err
		}
	}

	var total, done int64
	for _, f := range files {
		total += f.file.Size
	}
	var last time.Time
	report := func(status string) {
		if progress != nil {
			progress(status, done, total)
		}
	}

	sourceURL := hfc.downloadBase + req.Repo
	for _, f := range files {
		report("downloading")
		dest := filepath.Join(dir, filepath.FromSlash(f.file.Name))
		sum, err := hfc.fetch(ctx, req.Repo, f.file, dest, func(n int64) {
			done += n
			if now := utils.Now(); now.Sub(last) >= progressInterval {
				last = now
				report("downloading")
			}
		})
		if err != nil {
			return fmt.Errorf("failed to download %s: %w", f.file.Name, err)
		}
		info, err := os.Stat(dest)
		if err != nil {
			return err
		}
		if err := integrity.RecordFiles(dir, req.Repo, sourceURL, integrity.FileChecksum{
			RelativePath: filepath.FromSlash(f.file.Name), SHA256: sum,
			Size: info.Size(), ModTime: info.ModTime(), Feature: f.feature,
		}); err != nil {
			return fmt.Errorf("record %s: %w", f.file.Name, err)
		}
	}
	return nil
}

// plan returns the files req names in a repository listing: the weights it
// asks for (every shard of a split set) or the whole repository, and one
// file for each wanted feature. A feature's files are never weights.
func plan(listing []metadata.TreeFileEntry, req metadata.DownloadRequest) ([]planned, error) {
	var featureGlobs []string
	for _, globs := range req.Features {
		featureGlobs = append(featureGlobs, globs...)
	}
	byPath := make(map[string]metadata.TreeFileEntry, len(listing))
	all := make([]layout.File, 0, len(listing))
	var weights []layout.File
	for _, e := range listing {
		byPath[e.Name] = e
		f := layout.File{Path: e.Name, Size: e.Size}
		all = append(all, f)
		if !layout.Matches(e.Name, featureGlobs) {
			weights = append(weights, f)
		}
	}

	var out []planned
	switch {
	case req.Weights == "":
		for _, f := range weights {
			out = append(out, planned{file: byPath[f.Path]})
		}
	default:
		v, ok := layout.Pick(layout.GGUFVariants(weights), req.Weights)
		if !ok {
			e, exact := byPath[req.Weights]
			if !exact || layout.Matches(e.Name, featureGlobs) {
				return nil, fmt.Errorf("no weights matching %q in registry %q", req.Weights, req.Repo)
			}
			v.Files = []layout.File{{Path: e.Name}}
		}
		for _, f := range v.Files {
			out = append(out, planned{file: byPath[f.Path]})
		}
	}

	for _, name := range req.Want {
		globs, ok := req.Features[name]
		if !ok {
			return nil, fmt.Errorf("feature %q is not one this engine declares", name)
		}
		f, ok := layout.FirstMatch(all, globs)
		if !ok {
			if slices.Contains(req.Optional, name) {
				continue
			}
			return nil, fmt.Errorf("registry %q has no file for feature %q", req.Repo, name)
		}
		out = append(out, planned{file: byPath[f.Path], feature: name})
	}
	for _, p := range out {
		if err := validateRepoFileName(p.file.Name); err != nil {
			return nil, fmt.Errorf("download %s: %w", p.file.Name, err)
		}
	}
	return out, nil
}

// Plan resolves weights and feature declarations against one registry listing.
// The result is shared by registry downloads and peer transfers.
func Plan(listing []metadata.TreeFileEntry, req metadata.DownloadRequest) ([]metadata.DownloadFile, error) {
	ps, err := plan(listing, req)
	if err != nil {
		return nil, err
	}
	files := make([]metadata.DownloadFile, 0, len(ps))
	for _, p := range ps {
		files = append(files, metadata.DownloadFile{TreeFileEntry: p.file, Feature: p.feature})
	}
	return files, nil
}

// missing splits files into those to fetch and those dir already holds as
// listed: recorded in the manifest with the listed size, and the listed
// hash when there is one, and present on disk at that size. A file that
// fails any of these is fetched again, which is how a truncated download
// repairs. A held file is returned with the feature it is fetched for, so
// one that arrived before it was asked for as a feature gets its tag.
func missing(files []planned, manifest *integrity.ModelManifest, dir string) (fetch []planned, held []integrity.FileChecksum) {
	for _, p := range files {
		h, ok := manifest.Holds(dir, p.file.Name, p.file.Size, p.file.SHA256)
		if !ok {
			fetch = append(fetch, p)
			continue
		}
		if h.Feature != p.feature {
			h.Feature = p.feature
			held = append(held, h)
		}
	}
	return fetch, held
}

// fetch downloads one file to dest through its partial path and returns
// its SHA256. The partial is removed on any failure, cancellation
// included, so an interrupted download leaves nothing behind.
func (hfc *Connector) fetch(ctx context.Context, repo string, file metadata.TreeFileEntry, dest string, progress func(int64)) (sum string, err error) {
	req, err := hfc.newGet(ctx,
		fmt.Sprintf("%s%s/resolve/main/%s", hfc.downloadBase, repo, encodeRepoFilePath(file.Name)))
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	out, err := integrity.CreatePartial(dest)
	if err != nil {
		return "", err
	}
	partial := out.Name()
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = os.Remove(partial)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hasher, progressWriter(progress)), contextReader{ctx, resp.Body}); err != nil {
		return "", err
	}
	sum = hex.EncodeToString(hasher.Sum(nil))
	if want := strings.ToLower(strings.TrimSpace(file.SHA256)); want != "" && sum != want {
		return "", fmt.Errorf("sha256 mismatch for %s: got %s, want %s", file.Name, sum, want)
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	// On Windows this fails while an engine holds dest open; the
	// download reports it rather than replacing a file in use.
	return sum, os.Rename(partial, dest)
}

// progressWriter reports the bytes written through it.
type progressWriter func(int64)

func (p progressWriter) Write(b []byte) (int, error) {
	if p != nil {
		p(int64(len(b)))
	}
	return len(b), nil
}

// contextReader stops a copy between reads once ctx is done.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

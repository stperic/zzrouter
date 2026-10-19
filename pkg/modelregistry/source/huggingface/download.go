package huggingface

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stperic/common/download"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// progressInterval throttles progress callbacks on fast links.
const progressInterval = 100 * time.Millisecond

// Destination owns local materialization paths; source selection stays here.
type Destination interface {
	Directory() (string, error)
	Path(string) (string, error)
	CheckSpace(int64) error
	NoReplace() bool
}

// CatalogDestination is the explicitly selected LLM repository directory.
type CatalogDestination string

// Directory is the repository's materialization and integrity root.
func (d CatalogDestination) Directory() (string, error) { return string(d), nil }

// NoReplace preserves catalog replacement and repair behavior.
func (d CatalogDestination) NoReplace() bool { return false }

// Path preserves catalog filenames under the repository directory.
func (d CatalogDestination) Path(name string) (string, error) {
	return filepath.Join(string(d), filepath.FromSlash(name)), nil
}

// CheckSpace preserves catalog provisioning's existing capacity behavior.
func (d CatalogDestination) CheckSpace(_ int64) error { return nil }

// Download fetches what req names from a repository into the model's
// directory, adding to what is there. Each file is written beside its
// destination and published into place once complete and verified, then
// recorded in the integrity manifest with the feature it belongs to. A file
// the manifest already holds at the listed size and hash is not fetched
// again, and nothing already in the directory is removed.
func (hfc *Connector) Download(ctx context.Context, req metadata.DownloadRequest, progress func(status string, done, total int64), destination Destination) error {
	if err := req.ValidateTarget(); err != nil {
		return err
	}
	providerStore := req.Target.EffectiveOwner() == metadata.OwnerProviderStore
	if destination == nil {
		return fmt.Errorf("download requires a resolved destination")
	}
	files, err := hfc.downloadFiles(ctx, req)
	if err != nil {
		return err
	}
	dir, err := destination.Directory()
	if err != nil {
		return err
	}
	release, err := integrity.AcquireMaterialization(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	if _, err := destination.Directory(); err != nil {
		return err
	}
	sourceURL := hfc.downloadBase + req.Repo
	files, err = owed(req, files, dir, sourceURL, destination)
	if err != nil {
		return err
	}

	var total, done int64
	for _, f := range files {
		total += f.Size
	}
	var last time.Time
	report := func(status string) {
		if progress != nil {
			progress(status, done, total)
		}
	}

	for _, f := range files {
		report("downloading")
		dest, err := destination.Path(f.Name)
		if err != nil {
			return err
		}
		if destination.NoReplace() {
			if err := utils.RequireAbsent(dest); err != nil {
				return fileExistsError(err)
			}
		}
		if err := destination.CheckSpace(f.Size); err != nil {
			return err
		}
		sum, err := hfc.fetch(ctx, req.Repo, req.EffectiveRevision(), f.TreeFileEntry, dest, destination.NoReplace(), func(n int64) {
			done += n
			if now := utils.Now(); n < 0 || now.Sub(last) >= progressInterval {
				last = now
				report("downloading")
			}
		})
		if err != nil {
			return fmt.Errorf("failed to download %s: %w", f.Name, err)
		}
		dest, err = destination.Path(f.Name)
		if err != nil {
			return err
		}
		info, err := os.Stat(dest)
		if err != nil {
			return err
		}
		relative := filepath.FromSlash(f.Name)
		if providerStore {
			relative, err = filepath.Rel(dir, dest)
			if err != nil {
				return err
			}
		}
		if _, err := destination.Directory(); err != nil {
			return err
		}
		if err := integrity.RecordFiles(dir, req.Repo, sourceURL, integrity.FileChecksum{
			RelativePath: relative, SHA256: sum,
			Size: info.Size(), ModTime: info.ModTime(), Feature: f.Feature,
		}); err != nil {
			return fmt.Errorf("record %s: %w", f.Name, err)
		}
	}
	return nil
}

func fileExistsError(err error) error {
	if errors.Is(err, fs.ErrExist) {
		return errors.Join(err, httperr.Error{Status: http.StatusConflict, Code: string(httperr.CodeFileExists), Message: "provider model file already exists"})
	}
	return err
}

func (hfc *Connector) downloadFiles(ctx context.Context, req metadata.DownloadRequest) ([]metadata.DownloadFile, error) {
	var files []metadata.DownloadFile
	if req.Files != nil {
		files = req.Files
		for _, f := range files {
			if err := validateRepoFileName(f.Name); err != nil {
				return nil, err
			}
		}
	} else {
		listing, err := hfc.getRepoFilesRevision(ctx, req.Repo, req.EffectiveRevision())
		if err != nil {
			return nil, fmt.Errorf("failed to get repo files: %w", err)
		}
		files, err = Plan(listing.Files, req)
		if err != nil {
			return nil, err
		}
	}

	if req.Target.EffectiveOwner() == metadata.OwnerProviderStore && (len(files) != 1 || files[0].Name != req.Weights || files[0].Feature != "") {
		return nil, fmt.Errorf("provider download requires exactly the requested source file")
	}
	return files, nil
}

// Plan returns the files req names in a repository listing: the weights it
// asks for (every shard of a split set) or the whole repository, and one
// file for each wanted feature. A feature's files are never weights.
func Plan(listing []metadata.TreeFileEntry, req metadata.DownloadRequest) ([]metadata.DownloadFile, error) {
	if req.Target.EffectiveOwner() == metadata.OwnerProviderStore {
		for _, f := range listing {
			if f.Name == req.Weights {
				if err := validateRepoFileName(f.Name); err != nil {
					return nil, err
				}
				return []metadata.DownloadFile{{TreeFileEntry: f}}, nil
			}
		}
		return nil, fmt.Errorf("no file %q in registry %q", req.Weights, req.Repo)
	}
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

	out := make([]metadata.DownloadFile, 0)
	switch {
	case req.Weights == "":
		for _, f := range weights {
			out = append(out, metadata.DownloadFile{TreeFileEntry: byPath[f.Path]})
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
			out = append(out, metadata.DownloadFile{TreeFileEntry: byPath[f.Path]})
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
		out = append(out, metadata.DownloadFile{TreeFileEntry: byPath[f.Path], Feature: name})
	}
	for _, p := range out {
		if err := validateRepoFileName(p.Name); err != nil {
			return nil, fmt.Errorf("download %s: %w", p.Name, err)
		}
	}
	return out, nil
}

// owed returns the files a download must fetch into dir and records the rest
// of the plan in the manifest: files it already holds keep their feature tag,
// and model files still to fetch are pending until they arrive.
func owed(req metadata.DownloadRequest, files []metadata.DownloadFile, dir, sourceURL string, destination Destination) ([]metadata.DownloadFile, error) {
	manifest, err := integrity.ReadManifest(dir)
	if err != nil || req.Target.EffectiveOwner() == metadata.OwnerProviderStore {
		return files, err
	}
	var held []integrity.FileChecksum
	if !req.Force {
		files, held = missing(files, manifest, dir)
	}
	if len(held) > 0 {
		if _, err := destination.Directory(); err != nil {
			return nil, err
		}
		if err := integrity.RecordFiles(dir, req.Repo, sourceURL, held...); err != nil {
			return nil, err
		}
	}
	pending := make([]integrity.FileChecksum, 0, len(files))
	for _, f := range files {
		pending = append(pending, integrity.FileChecksum{RelativePath: f.Name, Size: f.Size, SHA256: f.SHA256, Feature: f.Feature})
	}
	return files, integrity.RecordPending(dir, req.Repo, sourceURL, pending...)
}

// missing splits files into those to fetch and those dir already holds as
// listed: recorded in the manifest with the listed size, and the listed
// hash when there is one, and present on disk at that size. A file that
// fails any of these is fetched again, which is how a truncated download
// repairs. A held file is returned with the feature it is fetched for, so
// one that arrived before it was asked for as a feature gets its tag.
func missing(files []metadata.DownloadFile, manifest *integrity.ModelManifest, dir string) (fetch []metadata.DownloadFile, held []integrity.FileChecksum) {
	for _, p := range files {
		h, ok := manifest.Holds(dir, p.Name, p.Size, p.SHA256)
		if !ok {
			fetch = append(fetch, p)
			continue
		}
		if h.Feature != p.Feature {
			h.Feature = p.Feature
			held = append(held, h)
		}
	}
	return fetch, held
}

func (hfc *Connector) fetch(ctx context.Context, repo, revision string, file metadata.TreeFileEntry, dest string, noReplace bool, progress func(int64)) (string, error) {
	req, err := hfc.newGet(ctx, fmt.Sprintf("%s%s/resolve/%s/%s", hfc.downloadBase, repo, url.PathEscape(revision), encodeRepoFilePath(file.Name)))
	if err != nil {
		return "", err
	}
	var done int64
	result, err := download.Fetch(hfc.httpClient(httpTimeout), req, dest, download.Options{
		Hash: true, NoReplace: noReplace, Exclusive: true,
		Progress: func(n, _ int64) {
			if progress != nil {
				progress(n - done)
			}
			done = n
		},
		Verify: func(result download.Result) error {
			if want := strings.ToLower(strings.TrimSpace(file.SHA256)); want != "" && result.SHA256 != want {
				return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", file.Name, result.SHA256, want)
			}
			return nil
		},
	})
	if noReplace {
		err = fileExistsError(err)
	}
	return result.SHA256, err
}

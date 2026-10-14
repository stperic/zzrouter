package prov_apps

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/preflight"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ModelStoreDestination resolves files in one provider-declared category.
type ModelStoreDestination struct {
	root string
	name string
}

// NewModelStoreDestination selects the root from config, never from request paths.
func NewModelStoreDestination(cfg config.ServiceConfig, category, name string) (*ModelStoreDestination, error) {
	root, err := ModelStoreCategoryPath(cfg, category)
	if err != nil {
		return nil, err
	}
	d := &ModelStoreDestination{root: root, name: name}
	_, err = ModelStoreFilePath(root, name, false)
	return d, err
}

func modelStoreRoot(cfg config.ServiceConfig) (string, error) {
	if cfg.Mode != constants.AppModeOnDemand || cfg.RunScope != config.RunScopeNode || cfg.Runtime == nil || cfg.Runtime.ModelStore == nil {
		return "", httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeProviderModelStoreUnsupported), Message: "provider does not declare a node-scoped model store"}
	}
	provider := fsroot.ProviderDir(cfg.Name)
	store, err := cfg.Runtime.ModelStorePath(provider)
	if err != nil {
		return "", err
	}
	if err := modelStoreContained(provider, store); err != nil {
		return "", err
	}
	return store, nil
}

// ModelStoreCategoryPath selects a confined category from the provider declaration.
func ModelStoreCategoryPath(cfg config.ServiceConfig, category string) (string, error) {
	store, err := modelStoreRoot(cfg)
	if err != nil {
		return "", err
	}
	if !slices.Contains(cfg.Runtime.ModelStore.Categories, category) {
		return "", httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeInvalidModelCategory), Message: fmt.Sprintf("invalid model category %q", category)}
	}
	root := filepath.Join(store, category)
	if err := modelStoreContained(store, root); err != nil {
		return "", err
	}
	return utils.ResolvePath(root)
}

// Directory is the category's integrity and materialization-lock root.
func (d *ModelStoreDestination) Directory() string { return d.root }

// Path maps the selected source file to its validated provider destination.
func (d *ModelStoreDestination) Path(_ string) (string, error) {
	return ModelStoreFilePath(d.root, d.name, true)
}

// CheckSpace checks the destination filesystem when the source size is known.
func (d *ModelStoreDestination) CheckSpace(size int64) error {
	if size <= 0 {
		return nil
	}
	path, err := ModelStoreFilePath(d.root, d.name, false)
	if err != nil {
		return err
	}
	return preflight.DiskSpace(filepath.Dir(path), uint64(size))
}

func invalidModelFile(message string) error {
	return httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeInvalidFileName), Message: message}
}

// ValidateModelFileName is the single lexical rule for provider model files.
func ValidateModelFileName(name string) error {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return invalidModelFile("invalid provider model filename")
	}
	for _, part := range strings.Split(name, "/") {
		if integrity.IsBookkeeping(part) {
			return invalidModelFile("invalid provider model filename: bookkeeping name")
		}
	}
	return nil
}

// ModelStoreFilePath checks existing ancestors before creation and the resolved
// parent afterward. Reads/deletes pass create=false and inspect the final Lstat.
func ModelStoreFilePath(root, name string, create bool) (string, error) {
	if err := ValidateModelFileName(name); err != nil {
		return "", err
	}
	dest := filepath.Join(root, filepath.FromSlash(name))
	parent := filepath.Dir(dest)
	if err := modelStoreContained(root, parent); err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return "", err
		}
		if err := modelStoreContained(root, parent); err != nil {
			return "", err
		}
	}
	parent, err := utils.ResolvePath(parent)
	return filepath.Join(parent, filepath.Base(dest)), err
}

func modelStoreContained(root, path string) error {
	root, err := utils.ResolvePath(root)
	if err != nil {
		return err
	}
	path, err = utils.ResolvePath(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return invalidModelFile("provider model path escapes its root")
	}
	return nil
}

// ModelStoreFile describes one regular file in a provider category.
type ModelStoreFile struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
}

// ModelStoreCategory includes empty categories and sorted relative filenames.
type ModelStoreCategory struct {
	Name  string           `json:"name"`
	Files []ModelStoreFile `json:"files"`
}

// ListModelStore reads the filesystem without requiring the provider to run.
func ListModelStore(ctx context.Context, cfg config.ServiceConfig, category string) ([]ModelStoreCategory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := modelStoreRoot(cfg); err != nil {
		return nil, err
	}
	categories := slices.Clone(cfg.Runtime.ModelStore.Categories)
	if category != "" {
		categories = []string{category}
	}
	slices.Sort(categories)
	out := make([]ModelStoreCategory, 0, len(categories))
	for _, category := range categories {
		root, err := ModelStoreCategoryPath(cfg, category)
		if err != nil {
			return nil, err
		}
		entry := ModelStoreCategory{Name: category, Files: []ModelStoreFile{}}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path == root {
				if !d.IsDir() {
					return invalidModelFile("provider model category is not a directory")
				}
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			if ValidateModelFileName(name) != nil {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if _, err := ModelStoreFilePath(root, name, false); err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				entry.Files = append(entry.Files, ModelStoreFile{Name: name, SizeBytes: info.Size(), ModifiedAt: info.ModTime().UTC()})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		slices.SortFunc(entry.Files, func(a, b ModelStoreFile) int { return strings.Compare(a.Name, b.Name) })
		out = append(out, entry)
	}
	return out, nil
}

// DeleteModelStoreFile unlinks only an Lstat-confirmed regular file.
func DeleteModelStoreFile(root, name string) error {
	path, err := ModelStoreFilePath(root, name, false)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return invalidModelFile("provider model is not a regular file")
		}
		err = os.Remove(path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return httperr.Error{Status: http.StatusNotFound, Code: string(httperr.CodeFileNotFound), Message: "provider model file not found"}
	}
	return err
}

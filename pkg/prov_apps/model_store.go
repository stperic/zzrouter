package prov_apps

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ModelStoreDestination resolves files in one provider-declared category.
type ModelStoreDestination struct {
	root string
	name string
}

// NewModelStoreDestination selects the root from config, never from request paths.
func NewModelStoreDestination(cfg config.ServiceConfig, category, name string) (*ModelStoreDestination, error) {
	if cfg.Mode != constants.AppModeOnDemand || cfg.RunScope != config.RunScopeNode || cfg.Runtime == nil || cfg.Runtime.ModelStore == nil {
		return nil, fmt.Errorf("provider does not declare a node-scoped model store")
	}
	if !slices.Contains(cfg.Runtime.ModelStore.Categories, category) {
		return nil, fmt.Errorf("invalid model category %q", category)
	}
	provider := fsroot.ProviderDir(cfg.Name)
	store, err := cfg.Runtime.ModelStorePath(provider)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(store, category)
	if err := modelStoreContained(provider, store); err != nil {
		return nil, err
	}
	if err := modelStoreContained(store, root); err != nil {
		return nil, err
	}
	root, err = utils.ResolvePath(root)
	if err != nil {
		return nil, err
	}
	d := &ModelStoreDestination{root: root, name: name}
	_, err = ModelStoreFilePath(root, name, false)
	return d, err
}

// Directory is the category's integrity and materialization-lock root.
func (d *ModelStoreDestination) Directory() string { return d.root }

// Path maps the selected source file to its validated provider destination.
func (d *ModelStoreDestination) Path(_ string) (string, error) {
	return ModelStoreFilePath(d.root, d.name, true)
}

// ValidateModelFileName is the single lexical rule for provider model files.
func ValidateModelFileName(name string) error {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid provider model filename")
	}
	for _, part := range strings.Split(name, "/") {
		if integrity.IsBookkeeping(part) {
			return fmt.Errorf("invalid provider model filename: bookkeeping name")
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
		return fmt.Errorf("provider model path escapes its root")
	}
	return nil
}

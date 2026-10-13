package config

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode"
)

// ModelStoreConfig declares category directories under a provider's data directory.
type ModelStoreConfig struct {
	Directory  string   `yaml:"directory" json:"directory"`
	Categories []string `yaml:"categories" json:"categories"`
}

func validProviderDirectory(dir string) bool {
	return dir != "." && fs.ValidPath(dir) && !strings.ContainsAny(dir, "\\:$") &&
		strings.IndexFunc(dir, unicode.IsControl) < 0
}

func (p *OnDemandProvider) validateDataDirectory() error {
	rt := p.Runtime
	if rt.DataDir != "" {
		if !validProviderDirectory(rt.DataDir) {
			return fmt.Errorf("runtime.data_dir must be a relative provider directory")
		}
		for _, reserved := range []string{SubdirBin, SubdirVenv, rt.Execution.WorkingDir} {
			dir, reserved := strings.ToLower(rt.DataDir), strings.ToLower(reserved)
			if reserved != "" && (dir == reserved || strings.HasPrefix(dir, reserved+"/") || strings.HasPrefix(reserved, dir+"/")) {
				return fmt.Errorf("runtime.data_dir overlaps provider execution or installation directories")
			}
		}
	}
	if rt.DataDir == "" {
		for _, arg := range rt.Execution.Args {
			if strings.Contains(arg, PlaceholderDataDir) {
				return fmt.Errorf("%s requires runtime.data_dir", PlaceholderDataDir)
			}
		}
	}
	if rt.ModelStore == nil {
		return nil
	}
	if p.RunScope != RunScopeNode || rt.DataDir == "" {
		return fmt.Errorf("runtime.model_store requires node run_scope and runtime.data_dir")
	}
	if !validProviderDirectory(rt.ModelStore.Directory) {
		return fmt.Errorf("runtime.model_store.directory must be a relative data directory")
	}
	if len(rt.ModelStore.Categories) == 0 {
		return fmt.Errorf("runtime.model_store.categories must not be empty")
	}
	seen := make(map[string]bool, len(rt.ModelStore.Categories))
	for _, category := range rt.ModelStore.Categories {
		if !validModelCategory(category) || seen[category] {
			return fmt.Errorf("runtime.model_store.categories must contain unique lowercase directory identifiers")
		}
		seen[category] = true
	}
	return nil
}

func validModelCategory(category string) bool {
	if category == "" {
		return false
	}
	for i, c := range category {
		if (c < 'a' || c > 'z') && (i == 0 || (c != '_' && (c < '0' || c > '9'))) {
			return false
		}
	}
	return true
}

// DataPath resolves the declared data directory under a provider's install directory.
func (prc *AppRuntimeConfig) DataPath(providerDir string) (string, error) {
	if prc == nil || prc.DataDir == "" {
		return "", fmt.Errorf("provider does not declare runtime.data_dir")
	}
	return filepath.Abs(filepath.Join(providerDir, filepath.FromSlash(prc.DataDir)))
}

// ModelStorePath derives the model root from the same data declaration used by launch.
func (prc *AppRuntimeConfig) ModelStorePath(providerDir string) (string, error) {
	if prc == nil || prc.ModelStore == nil {
		return "", fmt.Errorf("provider does not declare runtime.model_store")
	}
	data, err := prc.DataPath(providerDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(data, filepath.FromSlash(prc.ModelStore.Directory)), nil
}

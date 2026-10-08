package install

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// ToolkitSelection keeps release-owned search paths separate from caller variables.
type ToolkitSelection struct {
	root string
	venv string
}

// Variables returns the fixed compiler selectors for this managed runtime.
func (t ToolkitSelection) Variables() map[string]string {
	if t.root == "" {
		return nil
	}
	compiler := filepath.Join(t.root, "bin", "nvcc")
	if runtime.GOOS == "windows" {
		compiler += ".exe"
	}
	return map[string]string{
		"CUDA_HOME": t.root, "CUDA_PATH": t.root, "CUDA_LIB_PATH": filepath.Join(t.root, "lib"),
		"CUDACXX": compiler, "FLASHINFER_NVCC": compiler, "CUDA_MANAGED_ROOT": t.root,
	}
}

// Compose sets managed search paths after validation while retaining host C++ tools.
func (t ToolkitSelection) Compose(base []string) ([]string, error) {
	if t.root == "" {
		return slices.Clone(base), nil
	}
	values := map[string]string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if runtime.GOOS == "windows" && strings.EqualFold(key, "PATH") {
			key = "PATH"
		}
		values[key] = value
	}
	paths := []string{filepath.Join(t.root, "bin")}
	subdir := "bin"
	if runtime.GOOS == "windows" {
		subdir = "Scripts"
		paths = append(paths, filepath.Join(t.root, "lib"))
	}
	paths = append(paths, filepath.Join(t.venv, subdir))
	for _, path := range filepath.SplitList(values["PATH"]) {
		if filepath.IsAbs(path) && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	values["PATH"] = strings.Join(paths, string(os.PathListSeparator))
	values["LD_LIBRARY_PATH"] = filepath.Join(t.root, "lib")
	for key, value := range t.Variables() {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result, nil
}

// ManagedToolkitEnvironment owns toolkit selection after all caller environment tiers.
func ManagedToolkitEnvironment(runtimeID string, recipe config.InstallRecipe, pythonVersion string) ToolkitSelection {
	return ToolkitEnvironmentAt(fsroot.ProviderVenvDir(runtimeID), recipe, pythonVersion)
}

// ToolkitEnvironmentAt selects a release-owned layout under a managed venv.
func ToolkitEnvironmentAt(venv string, recipe config.InstallRecipe, pythonVersion string) ToolkitSelection {
	if recipe.Toolkit == nil || *recipe.Toolkit == "" {
		return ToolkitSelection{}
	}
	var major, minor int
	_, _ = fmt.Sscanf(pythonVersion, "%d.%d", &major, &minor)
	root := filepath.Join(venv, "lib", "python"+strconv.Itoa(major)+"."+strconv.Itoa(minor), "site-packages", "nvidia", "cu13")
	if runtime.GOOS == "windows" {
		root = filepath.Join(venv, "Lib", "site-packages", "nvidia", "cu13")
	}
	return ToolkitSelection{root: root, venv: venv}
}

// InstalledToolkitEnvironment returns actual installed metadata rather than the desired recipe.
func InstalledToolkitEnvironment(runtimeID string) (ToolkitSelection, error) {
	manifest, err := fsroot.ReadInstallManifest(runtimeID)
	if err != nil {
		return ToolkitSelection{}, err
	}
	if manifest == nil || manifest.Recipe == nil || manifest.Recipe.Toolkit == nil || *manifest.Recipe.Toolkit == "" {
		return ToolkitSelection{}, nil
	}
	if manifest.Interpreter == nil {
		return ToolkitSelection{}, fmt.Errorf("managed toolkit interpreter metadata missing")
	}
	if err := manifest.Recipe.Validate(); err != nil {
		return ToolkitSelection{}, fmt.Errorf("invalid installed recipe: %w", err)
	}
	environment := ManagedToolkitEnvironment(runtimeID, *manifest.Recipe, manifest.Interpreter.Version)
	if _, err := os.Stat(environment.Variables()["FLASHINFER_NVCC"]); err != nil {
		return ToolkitSelection{}, fmt.Errorf("managed toolkit missing; reinstall runtime: %w", err)
	}
	compiler, err := filepath.EvalSymlinks(environment.Variables()["FLASHINFER_NVCC"])
	if err != nil {
		return ToolkitSelection{}, err
	}
	venv, err := filepath.EvalSymlinks(fsroot.ProviderVenvDir(runtimeID))
	if err != nil {
		return ToolkitSelection{}, err
	}
	relative, err := filepath.Rel(venv, compiler)
	if err != nil || !fs.ValidPath(filepath.ToSlash(relative)) {
		return ToolkitSelection{}, fmt.Errorf("compiler escaped managed runtime; reinstall required")
	}
	return environment, nil
}

// ApplyToolkitEnvironment prevents config and request overrides from selecting a host toolkit.
func ApplyToolkitEnvironment(ctx context.Context, environment map[string]string, runtimeID string) (ToolkitSelection, error) {
	if err := ctx.Err(); err != nil {
		return ToolkitSelection{}, err
	}
	toolkit, err := InstalledToolkitEnvironment(runtimeID)
	if err != nil {
		return ToolkitSelection{}, err
	}
	for key, value := range toolkit.Variables() {
		environment[key] = value
	}
	return toolkit, nil
}

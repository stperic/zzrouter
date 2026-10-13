package prov_apps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

func TestProviderModelStorePaths(t *testing.T) {
	install, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	fsroot.SetProviderRootOverride(install)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	cfg := config.ServiceConfig{Name: "studio", Mode: constants.AppModeOnDemand, RunScope: config.RunScopeNode,
		Runtime: &config.AppRuntimeConfig{DataDir: "storage", ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae"}}}}
	d, err := NewModelStoreDestination(cfg, "vae", "nested/model.safetensors")
	require.NoError(t, err)
	root, err := cfg.Runtime.ModelStorePath(fsroot.ProviderDir(cfg.Name))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "vae"), d.Directory())
	_, err = os.Stat(d.Directory())
	require.ErrorIs(t, err, os.ErrNotExist, "selection must create no directories")
	path, err := d.Path("source/different.safetensors")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "vae/nested/model.safetensors"), path)
	_, err = NewModelStoreDestination(cfg, "checkpoints", "model")
	require.Error(t, err)
	cfg.RunScope = "model"
	_, err = NewModelStoreDestination(cfg, "vae", "model")
	require.Error(t, err)
}

func TestProviderModelStoreNameAndContainment(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	for _, name := range []string{"", ".", "..", "../escape", "/absolute", "C:/drive", `dir\file`, "a//b", "a/./b", "a/../b", "a\x00b", "a\nb", ".zzrouter-manifest.json", "dir/file.zzpart"} {
		t.Run(name, func(t *testing.T) {
			_, err := ModelStoreFilePath(root, name, true)
			require.Error(t, err)
		})
	}
	for _, name := range []string{".ZZROUTER-MANIFEST.JSON", ".zzrouter-manifeſt.json", "dir/file.ZZPART", "file.", "dir /file"} {
		require.NoError(t, ValidateModelFileName(name), "bookkeeping uses exact Linux filename components")
	}

	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	_, err = ModelStoreFilePath(root, "escape/new/file", true)
	require.ErrorContains(t, err, "escapes")
	_, err = os.Stat(filepath.Join(outside, "new"))
	require.ErrorIs(t, err, os.ErrNotExist, "refusal must precede outside mkdir")
	require.NoError(t, os.Mkdir(filepath.Join(root, "inside"), 0700))
	require.NoError(t, os.Symlink(filepath.Join(root, "inside"), filepath.Join(root, "alias")))
	path, err := ModelStoreFilePath(root, "alias/file", true)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "inside/file"), path)
}

func TestProviderModelStoreCategoryCannotRedefineRoot(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	cfg := config.ServiceConfig{Name: "studio", Mode: constants.AppModeOnDemand, RunScope: config.RunScopeNode,
		Runtime: &config.AppRuntimeConfig{DataDir: "data", ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae"}}}}
	root, err := cfg.Runtime.ModelStorePath(fsroot.ProviderDir(cfg.Name))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0700))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "vae")))
	_, err = NewModelStoreDestination(cfg, "vae", "file")
	require.ErrorContains(t, err, "escapes")
}

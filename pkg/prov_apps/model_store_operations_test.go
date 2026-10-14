package prov_apps

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

func TestProviderModelStoreInventoryDeleteAndCapacity(t *testing.T) {
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	cfg := config.ServiceConfig{Name: "studio", Mode: constants.AppModeOnDemand, RunScope: config.RunScopeNode,
		Runtime: &config.AppRuntimeConfig{DataDir: "data", ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae", "loras"}}}}
	list, err := ListModelStore(t.Context(), cfg, "")
	require.NoError(t, err)
	require.Equal(t, []ModelStoreCategory{{Name: "loras", Files: []ModelStoreFile{}}, {Name: "vae", Files: []ModelStoreFile{}}}, list)
	dest, err := NewModelStoreDestination(cfg, "vae", "a/file")
	require.NoError(t, err)
	require.NoError(t, dest.CheckSpace(0))
	require.NoError(t, dest.CheckSpace(-1))
	require.NoError(t, dest.CheckSpace(1))
	require.Error(t, dest.CheckSpace(math.MaxInt64))
	for _, name := range []string{"a/file", "a.gguf", "empty", ".zzrouter-manifest.json", "a/hidden.zzpart"} {
		path := filepath.Join(dest.Directory(), name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		data := []byte("manual")
		if name == "empty" {
			data = nil
		}
		require.NoError(t, os.WriteFile(path, data, 0600))
	}
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dest.Directory(), "escape")))
	require.NoError(t, os.Symlink(filepath.Join(dest.Directory(), "a.gguf"), filepath.Join(dest.Directory(), "link")))
	list, err = ListModelStore(t.Context(), cfg, "vae")
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Len(t, list[0].Files, 3)
	require.Equal(t, "a.gguf", list[0].Files[0].Name)
	require.Equal(t, "a/file", list[0].Files[1].Name)
	require.EqualValues(t, 0, list[0].Files[2].SizeBytes)
	require.Equal(t, "UTC", list[0].Files[0].ModifiedAt.Location().String())
	for _, name := range []string{"a", "link", "escape", "../file"} {
		err := DeleteModelStoreFile(dest.Directory(), name)
		var wire httperr.Error
		require.True(t, errors.As(err, &wire))
		require.Equal(t, string(httperr.CodeInvalidFileName), wire.Code)
	}
	require.NoError(t, DeleteModelStoreFile(dest.Directory(), "a/file"))
	err = DeleteModelStoreFile(dest.Directory(), "a/file")
	var wire httperr.Error
	require.ErrorAs(t, err, &wire)
	require.Equal(t, string(httperr.CodeFileNotFound), wire.Code)
	_, err = ListModelStore(t.Context(), cfg, "ghost")
	require.ErrorAs(t, err, &wire)
	require.Equal(t, string(httperr.CodeInvalidModelCategory), wire.Code)
	cfg.Runtime.ModelStore = nil
	_, err = ListModelStore(t.Context(), cfg, "vae")
	require.ErrorAs(t, err, &wire)
	require.Equal(t, string(httperr.CodeProviderModelStoreUnsupported), wire.Code)
}

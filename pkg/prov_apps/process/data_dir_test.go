package process

import (
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/require"
)

func TestBuildDataDirAndModelRootShareDeclaration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "${HF_HOME}", "${PORT}", "${MODEL_PATH}", "${MODEL}", "$literal")
	fsroot.SetProviderRootOverride(root)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	t.Setenv("HF_HOME", "must-not-rewrite-the-install-path")
	cfg := &config.ServiceConfig{Name: "studio", Runtime: &config.AppRuntimeConfig{
		ModelStore: &config.ModelStoreConfig{Directory: "models", Categories: []string{"vae"}},
		Execution:  config.ExecutionConfig{Command: "python", Args: []string{"--base-directory", config.PlaceholderDataDir, "--cache=${DATA_DIR}/cache", "--hf-home", "${HF_HOME}"}},
	}}
	for _, dir := range []string{"data", "storage/nested"} {
		cfg.Runtime.DataDir = dir
		launch, err := NewCommandBuilder(cfg).Build(ResolvedModel{}, 8188, nil, nil)
		require.NoError(t, err)
		data, err := cfg.Runtime.DataPath(fsroot.ProviderDir(cfg.Name))
		require.NoError(t, err)
		models, err := cfg.Runtime.ModelStorePath(fsroot.ProviderDir(cfg.Name))
		require.NoError(t, err)
		require.True(t, filepath.IsAbs(data))
		require.Equal(t, filepath.Join(root, cfg.Name, filepath.FromSlash(dir)), data)
		require.Equal(t, data, argAfter(t, launch.Args, "--base-directory"))
		require.Contains(t, launch.Args, "--cache="+data+"/cache")
		require.Equal(t, filepath.Join(data, "models"), models)
		require.Equal(t, "must-not-rewrite-the-install-path", argAfter(t, launch.Args, "--hf-home"))
	}
}

func TestBuildKeepsExpandedEnvironmentLiteral(t *testing.T) {
	value := "${PORT}/${MODEL}/${MODEL_PATH}/${DATA_DIR}"
	t.Setenv("HF_HOME", value)
	cfg := &config.ServiceConfig{Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "engine", Args: []string{"${HF_HOME}"}}}}
	launch, err := NewCommandBuilder(cfg).Build(ResolvedModel{}, 8188, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []string{value}, launch.Args)
}

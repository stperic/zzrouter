package process

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLauncherAppliesPreparedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix pwd fixture")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	run := instance.NewInstance("working-directory", "studio", "", 0, 0, 0)
	defer run.Cancel()
	run.SetResolved(instance.Resolved{WorkingDir: dir})
	var output bytes.Buffer
	result, err := NewLauncher(&SecurityConfig{}, true, t.TempDir()).Launch(t.Context(), run, "/bin/pwd", nil, nil, &output)
	require.NoError(t, err)
	_, err = result.Wait()
	require.NoError(t, err)
	if result.StdinPipe != nil {
		_ = result.StdinPipe.Close()
	}
	require.Equal(t, dir, strings.TrimSpace(output.String()))
	require.Equal(t, dir, result.Command.WorkingDir)
	svc := config.ServiceConfig{Name: "studio", Runtime: &config.AppRuntimeConfig{Execution: config.ExecutionConfig{Command: "python", WorkingDir: "source"}}}
	prepared, err := NewCommandBuilder(&svc).Build(ResolvedModel{}, 8188, nil, nil)
	require.NoError(t, err)
	require.Equal(t, fsroot.ProviderDir("studio")+"/source", prepared.WorkingDir)
}

func TestReconciledProviderWorkingDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix pwd fixture")
	}
	configDir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(configDir))
	for _, provider := range []string{"vllm", "llamacpp", "mlx"} {
		filename := filepath.Join(configDir, "on-demand", provider, "config.yaml")
		data, err := os.ReadFile(filename)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, yaml.Unmarshal(data, &doc))
		doc["enabled"] = true
		doc["runtime"].(map[string]any)["execution"].(map[string]any)["working_dir"] = "./models"
		data, err = yaml.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filename, data, 0o600))
	}
	changed, err := templates.ReconcileManagedSpine(configDir)
	require.NoError(t, err)
	apps, err := config.LoadAppsConfig(configDir)
	require.NoError(t, err)
	providerRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	fsroot.SetProviderRootOverride(providerRoot)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	source := filepath.Join(fsroot.ProviderDir("comfyui"), "source")
	require.NoError(t, os.MkdirAll(source, 0o755))
	inherited, err := os.Stat(".")
	require.NoError(t, err)
	for _, provider := range []string{"vllm", "llamacpp", "mlx", "comfyui"} {
		t.Run(provider, func(t *testing.T) {
			svc, ok := apps.LookupApp(provider)
			require.True(t, ok)
			prepared, err := NewCommandBuilder(&svc).Build(ResolvedModel{Name: "local-model", Path: filepath.Join(providerRoot, "weights")}, svc.Runtime.BasePort, nil, nil)
			require.NoError(t, err)
			run := instance.NewInstance(provider+"-working-directory", provider, "", 0, 0, 0)
			defer run.Cancel()
			run.SetResolved(instance.Resolved{WorkingDir: prepared.WorkingDir})
			var output bytes.Buffer
			result, err := NewLauncher(&SecurityConfig{}, true, t.TempDir()).Launch(t.Context(), run, "/bin/pwd", nil, nil, &output)
			require.NoError(t, err)
			_, err = result.Wait()
			require.NoError(t, err)
			if result.StdinPipe != nil {
				_ = result.StdinPipe.Close()
			}
			if provider == "comfyui" {
				require.Equal(t, source, prepared.WorkingDir)
				require.Equal(t, source, strings.TrimSpace(output.String()))
				require.NotContains(t, changed, "on-demand/comfyui/config.yaml")
			} else {
				require.Empty(t, prepared.WorkingDir)
				printed, err := os.Stat(strings.TrimSpace(output.String()))
				require.NoError(t, err)
				require.True(t, os.SameFile(inherited, printed), "the run inherits the node's directory; compare identity, not spelling, on case-insensitive filesystems")
				require.Contains(t, changed, "on-demand/"+provider+"/config.yaml")
				require.True(t, svc.IsEnabled())
				require.NoDirExists(t, filepath.Join(fsroot.ProviderDir(provider), "models"))
			}
			require.Equal(t, prepared.WorkingDir, result.Command.WorkingDir)
		})
	}
	changed, err = templates.ReconcileManagedSpine(configDir)
	require.NoError(t, err)
	require.Empty(t, changed)
}

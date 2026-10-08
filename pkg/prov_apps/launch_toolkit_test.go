//go:build !windows

package prov_apps

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestPreparedToolkitReachesNativeLifecycleChild(t *testing.T) {
	isolateProviderRoot(t)
	m, svc, _ := featureLaunchFixture(t)
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/vllm/config.yaml")
	require.NoError(t, err)
	var provider config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(data, &provider))
	recipe := provider.Install.Runtimes["vllm"]
	venv := fsroot.ProviderVenvDir(svc.Name)
	root := filepath.Join(venv, "lib", "python3.13", "site-packages", "nvidia", "cu13")
	compiler := filepath.Join(root, "bin", "nvcc")
	require.NoError(t, os.MkdirAll(filepath.Dir(compiler), 0700))
	require.NoError(t, os.WriteFile(compiler, []byte("synthetic compiler"), 0700))
	require.NoError(t, fsroot.WriteInstallManifest(svc.Name, &fsroot.InstallManifest{Recipe: &recipe, Interpreter: &fsroot.ManifestInterpreter{Version: "3.13.7"}}))
	prepared, err := m.prepareLaunch(svc, "vllm", "chat", LaunchRequest{Provider: "vllm", Model: "vendor/model", EnvVars: map[string]string{"CUDA_LIB_PATH": "/host/cuda/lib"}}, 8100)
	require.NoError(t, err)
	require.NotNil(t, prepared.executionEnvironment)
	assert.Equal(t, filepath.Join(root, "lib"), prepared.resolved.Environment["CUDA_LIB_PATH"])
	inst := instance.NewInstance("toolkit-child", "vllm", "vendor/model", 8100, 0, 0)
	inst.LogFilePath = filepath.Join(t.TempDir(), "engine.log")
	file, err := os.Create(inst.LogFilePath)
	require.NoError(t, err)
	require.NoError(t, m.instances.Register(inst))
	probe := &health.ReadinessProbe{Timeout: time.Minute, LogPatterns: health.LogPatterns{Failure: []health.PatternMatcher{{Pattern: "RuntimeError:"}}}}
	require.NoError(t, probe.Validate())
	args := []string{"-c", "printf '%s|%s|%s|%s|%s\\n' \"$PATH\" \"$LD_LIBRARY_PATH\" \"$CUDACXX\" \"$FLASHINFER_NVCC\" \"$CUDA_LIB_PATH\"; printf 'RuntimeError: synthetic fixture exit\\n'; exit 23"}
	m.runInstanceLifecycle(inst, "/bin/sh", args, prepared.resolved.Environment, file, health.CheckConfig{ReadinessProbe: probe}, prepared.executionEnvironment)
	output, err := os.ReadFile(inst.LogFilePath)
	require.NoError(t, err)
	assert.Contains(t, string(output), filepath.Join(root, "bin")+string(os.PathListSeparator))
	assert.Contains(t, string(output), "|"+filepath.Join(root, "lib")+"|"+compiler+"|"+compiler+"|"+filepath.Join(root, "lib"))
	assert.NotContains(t, string(output), "/host/cuda/lib")
	inst.Cancel()
}

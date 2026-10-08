package builtins_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/builtins/pythonvenv"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestVLLMInstallVerification(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("vLLM's managed install requires Unix")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	root := t.TempDir()
	fsroot.SetProviderRootOverride(root)
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })

	// Synthetic verification remains executable on any Unix host; dispatcher guards
	// are tested separately with the actual shipped platform restrictions.
	data, err := templates.AppsFS.ReadFile("files/providers/on-demand/vllm/config.yaml")
	require.NoError(t, err)
	var declared config.OnDemandProvider
	require.NoError(t, yaml.Unmarshal(data, &declared))
	sc := config.ServiceConfig{Name: "vllm", Install: declared.Install, Requirements: declared.Requirements, Runtime: &config.AppRuntimeConfig{Execution: declared.Runtime.Execution}, PinnedVersion: declared.PinnedVersion, VersionSource: declared.VersionSource}
	snapshot, err := install.ResolveRecipe(sc, "", "vllm", "")
	require.NoError(t, err)
	data, err = templates.AppsFS.ReadFile("files/providers/on-demand/vllm/schema.yaml")
	require.NoError(t, err)
	schemaConfig, err := schema.LoadYAMLSchema(data)
	require.NoError(t, err)
	inst := pythonvenv.New(pythonvenv.Config{Name: "vllm", SchemaProvider: "vllm", Recipe: func(string, string) (install.RecipeSnapshot, error) { return snapshot, nil }, Checks: func(string, string) (schema.RuntimeChecks, error) {
		return schemaConfig.Diagnostics.Runtimes["vllm"], nil
	}})
	plan, err := inst.InstallPlan(context.Background(), "0.27.1")
	require.NoError(t, err)
	assert.NotContains(t, plan.Steps[2].Command, "--extra-index-url")
	assert.NotContains(t, plan.Steps[2].Command, "cu128")
	assert.Contains(t, plan.Steps[2].Command, "vllm==0.27.1")
	assert.Equal(t, 90*time.Minute, plan.Steps[2].Timeout)

	var verify install.Step
	for _, step := range plan.Steps {
		if step.Verify.Type == "runtime_checks" && step.Command != "" {
			verify = step
		}
	}
	require.NotZero(t, verify.Number)
	rendered, err := install.RuntimeCheckCommand(verify.Verify.Python, *verify.Verify.RuntimeChecks)
	require.NoError(t, err)
	assert.Equal(t, verify.Command, rendered)

	venvPython := fsroot.ProviderVenvPython("vllm")
	verify.Verify.Python = venvPython
	verify.Verify.Transient = false
	managedRoot := fsroot.ProviderVenvDir("vllm")
	compilerRoot := filepath.Join(managedRoot, "cuda")
	verify.Verify.Toolkit = install.ToolkitSelection{}
	verify.Verify.Environment = map[string]string{"CUDA_HOME": compilerRoot, "CUDA_PATH": compilerRoot, "CUDACXX": filepath.Join(compilerRoot, "bin", "nvcc"), "FLASHINFER_NVCC": filepath.Join(compilerRoot, "bin", "nvcc"), "CUDA_MANAGED_ROOT": compilerRoot}
	require.NoError(t, os.MkdirAll(filepath.Dir(venvPython), 0700))
	fixture := filepath.Join(root, "fake_modules.py")
	wrapper := "#!/bin/sh\nexec " + fsroot.ShellQuote(python) + " -I " + fsroot.ShellQuote(fixture) + " \"$@\"\n"
	require.NoError(t, os.WriteFile(venvPython, []byte(wrapper), 0700))

	for _, tc := range []struct {
		name, torch, audio, vision, missing, failure string
		pipExit                                      int
		passed                                       bool
	}{
		{name: "consistent CUDA 13", torch: "'13.0'", audio: "13000", vision: "13000", passed: true},
		{name: "consistent CUDA 12.8", torch: "'12.8'", audio: "12080", vision: "12080", passed: true},
		{name: "incident audio mismatch", torch: "'13.0'", audio: "12080", vision: "13000", failure: "CUDA build mismatch"},
		{name: "vision minor mismatch", torch: "'13.0'", audio: "13000", vision: "13010", failure: "CUDA build mismatch"},
		{name: "CPU torch", torch: "None", audio: "13000", vision: "13000", failure: "CUDA build mismatch"},
		{name: "unknown audio build", torch: "'13.0'", audio: "None", vision: "13000", failure: "CUDA build mismatch"},
		{name: "CPU vision", torch: "'13.0'", audio: "13000", vision: "-1", failure: "CUDA build mismatch"},
		{name: "missing engine", torch: "'13.0'", audio: "13000", vision: "13000", missing: "vllm", failure: "No module named 'vllm'"},
		{name: "missing engine core", torch: "'13.0'", audio: "13000", vision: "13000", missing: "vllm.v1.engine.core", failure: "No module named 'vllm'"},
		{name: "missing audio", torch: "'13.0'", audio: "13000", vision: "13000", missing: "torchaudio", failure: "No module named 'torchaudio'"},
		{name: "broken dependencies", torch: "'13.0'", audio: "13000", vision: "13000", pipExit: 1, failure: "broken dependency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Execute the generated verifier with isolated fake modules, without installing packages.
			source := fmt.Sprintf(`import sys, types, subprocess, io, os, pathlib, shutil
sys.prefix = %[6]q
shutil.which = lambda name: "/fixture/c++" if name == "c++" else os.environ["CUDACXX"] if name == "nvcc" else None
while sys.argv[1] in ["-I", "-B"]:
    del sys.argv[1]
class PipProcess:
    def __init__(self, *args, **kwargs):
        self.compiler = len(args[0]) > 1 and args[0][1] == "--version"
        self.stdout = io.BytesIO(("release " + str(%[3]s)).encode() if self.compiler else (b"broken dependency" if %[1]d else b"No broken requirements found."))
    def wait(self, timeout=None):
        return 0 if self.compiler else %[1]d
subprocess.Popen = PipProcess
import importlib
original_import = importlib.import_module
def checked_import(name, *args, **kwargs):
    if name == %[2]q or (bool(%[2]q) and name.startswith(%[2]q + ".")):
        raise ModuleNotFoundError(f"No module named '{name.split('.')[0]}'")
    if name == "flashinfer.comm.fd_exchange":
        base = os.getenv("FLASHINFER_WORKSPACE_BASE")
        assert base and pathlib.Path(base).name.startswith("zzrouter-probe-")
        target = pathlib.Path(base) / ".cache" / "flashinfer" / "fixture.log"
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("probe cache")
    return original_import(name, *args, **kwargs)
importlib.import_module = checked_import
for name in ["torch", "torchaudio", "torchvision", "vllm", "vllm.entrypoints.cli.main", "vllm.entrypoints.openai.api_server", "vllm.v1.engine.core", "flashinfer.comm.fd_exchange"]:
    sys.modules[name] = types.ModuleType(name)
torch = sys.modules["torch"]
torch.version = types.SimpleNamespace(cuda=%[3]s)
torch.cuda = types.SimpleNamespace(is_available=lambda: True, device_count=lambda: 1)
torch.ops = types.SimpleNamespace(
    _torchaudio=types.SimpleNamespace(cuda_version=lambda: %[4]s),
    torchvision=types.SimpleNamespace(_cuda_version=lambda: %[5]s))
code = sys.argv[2]
sys.argv = ["-c", *sys.argv[3:]]
scope = {"__name__": "synthetic_probe"}
exec(code, scope)
scope["loaded_cuda_runtime"] = lambda: (str(%[3]s), os.path.join(sys.prefix, "lib", "libcudart.so.13"))
results = scope["run"](__import__("json").loads(sys.argv[1]), sys.argv[2])
print("ZZROUTER_RUNTIME_CHECKS=" + __import__("json").dumps(results))
sys.exit(0 if all(item["passed"] for item in results) else 1)
`, tc.pipExit, tc.missing, tc.torch, tc.audio, tc.vision, managedRoot)
			require.NoError(t, os.WriteFile(fixture, []byte(source), 0600))
			result := (&install.Plan{Provider: "vllm", Steps: []install.Step{verify}}).VerifyAll()
			assert.Equal(t, install.RuntimeCheckContract, result.CheckContract)
			require.Len(t, result.Checks, 11)
			assert.Equal(t, tc.passed, result.AllOK)
			require.Len(t, result.Steps, 1)
			assert.Equal(t, tc.passed, result.Steps[0].Passed)
			if tc.failure != "" {
				data, err := json.Marshal(result.Checks)
				require.NoError(t, err)
				assert.Contains(t, string(data), tc.failure)
			}
		})
	}
}

func TestManagedPythonVerifyPlanResolvesEnvironmentCallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed Python runtimes require Unix")
	}
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride("") })
	for _, name := range []string{"vllm", "mlx", "mlx-vlm"} {
		t.Run(name, func(t *testing.T) {
			if name == "mlx-vlm" && runtime.GOOS != "darwin" {
				t.Skip("MLX vision runtime requires macOS")
			}
			called := ""
			dispatcher := builtins.NewDispatcher(func(provider string) map[string]string {
				called = provider
				return map[string]string{"VLLM_TEST_SAMPLER": "node-tier"}
			}, nil)
			inst, err := dispatcher.Get(name)
			require.NoError(t, err)
			allowed := false
			for _, platform := range inst.SupportedPlatforms() {
				allowed = allowed || platform == fsroot.CurrentPlatform()
			}
			if !allowed {
				t.Skip("runtime is unsupported on this host")
			}
			plan, err := inst.InstallPlan(t.Context(), "1.0.0")
			require.NoError(t, err)
			parent := name
			if name == "mlx-vlm" {
				parent = "mlx"
			}
			assert.Equal(t, parent, called)
			var found bool
			for _, step := range plan.Steps {
				if step.Verify.Type == "runtime_checks" {
					found = true
					assert.Equal(t, "node-tier", step.Verify.Environment["VLLM_TEST_SAMPLER"])
				}
			}
			assert.True(t, found)
		})
	}
}

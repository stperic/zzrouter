package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolkitFindingsArchitectureRequirement(t *testing.T) {
	checks := schema.RuntimeChecks{ToolkitMinimum: map[string]string{"12": "12.9"}, Workaround: map[string]string{"SAMPLER": "0"}}
	for _, tc := range []struct {
		capability, version string
		finding             bool
	}{
		{"12.0", "12.6", true}, {"12.0", "12.9", false}, {"12.0", "13.0", false},
		{"12.0", "", true}, {"8.9", "12.6", false},
	} {
		t.Run(tc.capability+"/"+tc.version, func(t *testing.T) {
			result := ToolkitFindings(checks, CUDAToolkit{Path: "/host/nvcc", Version: tc.version}, []string{tc.capability})
			if !tc.finding {
				assert.Empty(t, result)
				return
			}
			require.Len(t, result, 1)
			assert.Contains(t, result[0].Required, "12.9")
			assert.NotEmpty(t, result[0].Impact)
			assert.Len(t, result[0].HumanRemediation, 3)
			assert.Equal(t, checks.Workaround, result[0].APIWorkarounds)
		})
	}
}

func TestEnvironmentSeparatesLoadedRuntimeAndHostToolkit(t *testing.T) {
	libraryRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	libraryPath := filepath.Join(libraryRoot, "managed", "lib", "libcudart.so.13")
	// The synthetic /proc map uses Linux separators even on a Windows test host.
	mappedPath := filepath.ToSlash(libraryPath)
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	// Exercise the fixed probe with synthetic devices and loaded-library bindings.
	data, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	script := `import json, sys, types, io, ctypes, builtins, importlib.metadata, os
os.RTLD_LOCAL, os.RTLD_NOLOAD = 0, 4
scope = {"__name__": "probe_library"}
exec(json.loads(sys.argv[1]), scope)
torch = types.ModuleType("torch")
torch.version = types.SimpleNamespace(cuda="12.8")
torch.cuda = types.SimpleNamespace(init=lambda: None, device_count=lambda: 1, get_device_capability=lambda i: (12, 0), get_device_name=lambda i: "synthetic SM12")
sys.modules["torch"] = torch
importlib.metadata.version = lambda name: "1.2.3"
scope["toolkit"] = lambda: dict(path="/host/nvcc", version="12.6", source="PATH", reason="")
original_open = builtins.open
builtins.open = lambda name, *args, **kw: io.StringIO("0-1 r-xp 0 0:0 0 " + sys.argv[2] + "\n") if name == "/proc/self/maps" else original_open(name, *args, **kw)
class Query:
    def __call__(self, pointer):
        pointer._obj.value = 13000
        return 0
ctypes.CDLL = lambda path, **kwargs: types.SimpleNamespace(cudaRuntimeGetVersion=Query())
print(json.dumps(scope["environment"]({"checks": ["cuda_available"], "packages": ["torch"]})))
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-B", "-c", script, string(data), mappedPath).CombinedOutput()
	require.NoError(t, err, string(output))
	var result RuntimeEnvironment
	require.NoError(t, json.Unmarshal(output, &result))
	assert.Equal(t, "13.0", result.CUDARuntime.LoadedVersion)
	assert.Equal(t, "12.8", result.CUDARuntime.BuildVersion)
	assert.Equal(t, libraryPath, result.CUDARuntime.LibraryPath)
	assert.Equal(t, "12.6", result.Toolkit.Version)
	require.Len(t, result.Devices, 1)
	assert.Equal(t, "12.0", result.Devices[0].Capability)
	assert.Equal(t, "1.2.3", result.Packages["torch"].Version)
}

func TestVerifyProbeUsesLaunchEnvironmentWithoutPublishingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixture")
	}
	path := filepath.Join(t.TempDir(), "python")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf 'ZZROUTER_RUNTIME_CHECKS=[{\"name\":\"environment\",\"passed\":true,\"actual\":\"%s\",\"reason\":\"\"}]\\n' \"$VLLM_TEST_SAMPLER\"\n"), 0o700))
	checks := schema.RuntimeChecks{Checks: []string{"pip_check"}, Kernels: "unknown"}
	verify := StepVerify{Type: "runtime_checks", Python: path, RuntimeChecks: &checks, Environment: map[string]string{"VLLM_TEST_SAMPLER": "configured"}}
	result := (&Plan{Steps: []Step{{Number: 1, Verify: verify}}}).VerifyAllContext(t.Context())
	require.Len(t, result.Checks, 1)
	assert.Equal(t, "configured", result.Checks[0].Actual)
	data, err := json.Marshal(verify)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "VLLM_TEST_SAMPLER")
}

func TestRuntimeProbeUsesCanonicalEnvironmentGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixture")
	}
	t.Setenv("ZZROUTER_TEST_SERVER_SECRET", "test-secret")
	path := filepath.Join(t.TempDir(), "python")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf 'ZZROUTER_RUNTIME_CHECKS=[{\"name\":\"secret\",\"passed\":true,\"actual\":\"%s\",\"reason\":\"\"}]\\n' \"${ZZROUTER_TEST_SERVER_SECRET:-absent}\"\n"), 0o700))
	checks := schema.RuntimeChecks{Checks: []string{"pip_check"}, Kernels: "unknown"}
	result := runRuntimeChecks(t.Context(), path, checks, nil)
	require.Len(t, result, 1)
	assert.Equal(t, "absent", result[0].Actual)
	result = runRuntimeChecks(t.Context(), path, checks, map[string]string{"LD_PRELOAD": "/not/executed"})
	require.Len(t, result, 1)
	assert.False(t, result[0].Passed)
	assert.Contains(t, result[0].Reason, "LD_PRELOAD")
}

func TestMetalObservationDistinguishesUnavailableAndBrokenImport(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	data, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	script := `import json, sys, types
scope = {"__name__": "probe_library"}
exec(json.loads(sys.argv[1]), scope)
scope["toolkit"] = lambda: dict(path="", version="", source="", reason="")
sys.modules["mlx.core"] = types.SimpleNamespace(metal=types.SimpleNamespace(is_available=lambda: False))
unavailable = scope["environment"]({"checks": ["metal_available"]})
sys.modules["mlx.core"] = None
broken = scope["environment"]({"checks": ["metal_available"]})
print(json.dumps([unavailable, broken]))
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-B", "-c", script, string(data)).CombinedOutput()
	require.NoError(t, err, string(output))
	var results []RuntimeEnvironment
	require.NoError(t, json.Unmarshal(output, &results))
	require.Len(t, results, 2)
	require.NotNil(t, results[0].MetalAvailable)
	assert.False(t, *results[0].MetalAvailable)
	assert.Empty(t, results[0].MetalReason)
	assert.Nil(t, results[1].MetalAvailable)
	assert.Contains(t, results[1].MetalReason, "ModuleNotFoundError")
}

package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/process"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolkitSearchPathsUseCapturedEnvironment(t *testing.T) {
	toolkitName := "cuda13"
	recipe := config.InstallRecipe{Toolkit: &toolkitName}
	hostBin := filepath.Join(t.TempDir(), "host-bin")
	separator := string(os.PathListSeparator)
	base := []string{"PATH=" + strings.Join([]string{hostBin, "", ".", "relative", hostBin}, separator), "LD_LIBRARY_PATH=/host/cuda/lib", "CUDA_LIB_PATH=/host/cuda/lib", "OTHER=preserved"}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "later-path"))
	for _, name := range []string{"candidate", "live", "disposable"} {
		t.Run(name, func(t *testing.T) {
			venv := filepath.Join(t.TempDir(), name, "venv")
			selection := ToolkitEnvironmentAt(venv, recipe, "3.13.7")
			assert.Equal(t, filepath.Join(selection.root, "lib"), selection.Variables()["CUDA_LIB_PATH"])
			composed, err := selection.Compose(base)
			require.NoError(t, err)
			values := envValues(composed)
			paths := filepath.SplitList(values["PATH"])
			assert.Equal(t, filepath.Join(selection.root, "bin"), paths[0])
			assert.Equal(t, hostBin, paths[len(paths)-1], "retain the host C++ search path")
			assert.NotContains(t, paths, "")
			assert.NotContains(t, paths, ".")
			assert.NotContains(t, paths, "relative")
			assert.NotContains(t, paths, os.Getenv("PATH"), "do not reread ambient PATH")
			assert.Equal(t, 1, strings.Count(values["PATH"], hostBin))
			assert.Equal(t, filepath.Join(selection.root, "lib"), values["LD_LIBRARY_PATH"])
			assert.Equal(t, filepath.Join(selection.root, "lib"), values["CUDA_LIB_PATH"])
			assert.Equal(t, selection.Variables()["FLASHINFER_NVCC"], values["CUDACXX"])
			assert.Equal(t, "preserved", values["OTHER"])
		})
	}
	assert.Contains(t, base, "LD_LIBRARY_PATH=/host/cuda/lib")
	assert.Contains(t, base, "CUDA_LIB_PATH=/host/cuda/lib")
}

func envValues(entries []string) map[string]string {
	values := map[string]string{}
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func TestToolkitProbeChildReceivesTrustedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable fixture")
	}
	venv := filepath.Join(t.TempDir(), "venv")
	selection := ToolkitSelection{root: filepath.Join(venv, "toolkit"), venv: venv}
	python := filepath.Join(t.TempDir(), "python")
	require.NoError(t, os.WriteFile(python, []byte("#!/bin/sh\nprintf 'ZZROUTER_RUNTIME_CHECKS=[{\"name\":\"paths\",\"passed\":true,\"actual\":\"%s|%s|%s\",\"reason\":\"\"}]\\n' \"$PATH\" \"$LD_LIBRARY_PATH\" \"$CUDACXX\"\n"), 0700))
	checks := schema.RuntimeChecks{Checks: []string{"pip_check"}, Kernels: "unknown"}
	base := []string{"PATH=/usr/bin:/bin", "LD_LIBRARY_PATH=/host/cuda/lib"}
	result := RunRuntimeChecksSnapshot(t.Context(), python, checks, selection.Variables(), base, selection)
	require.Len(t, result, 1)
	require.True(t, result[0].Passed, result[0].Reason)
	assert.Contains(t, result[0].Actual, filepath.Join(selection.root, "bin")+":")
	assert.Contains(t, result[0].Actual, "|"+filepath.Join(selection.root, "lib")+"|")
	assert.NotContains(t, result[0].Actual, "/host/cuda")
	for _, key := range []string{"PATH", "LD_LIBRARY_PATH"} {
		_, err := runRuntimeProbeSnapshot(t.Context(), python, []byte(`{"checks":[]}`), map[string]string{key: "/host"}, "verify", base, selection)
		assert.ErrorIs(t, err, process.ErrDangerousEnvVar)
	}
}

func TestManagedToolkitProbeChecksEffectiveResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shipped managed CUDA toolkit is Linux-only")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	data, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	// Synthetic Torch and library bindings exercise the fixed probe without GPU packages.
	script := `import json, sys, os, pathlib, types, tempfile
scope = {"__name__": "probe_library"}
exec(json.loads(sys.argv[1]), scope)
with tempfile.TemporaryDirectory() as temp:
    base = pathlib.Path(temp)
    venv = base / "venv"
    root = venv / "toolkit"
    host = base / "host"
    for path in (root / "bin", root / "lib", host): path.mkdir(parents=True)
    compiler = root / "bin" / "nvcc"
    host_compiler = host / "nvcc"
    cpp = host / "c++"
    library = root / "lib" / "libcudart.so.13"
    outside = host / "libcudart.so.13"
    for path in (compiler, host_compiler, cpp, library, outside):
        path.write_text("synthetic")
        path.chmod(0o700)
    sys.prefix = str(venv)
    sys.modules["torch"] = types.SimpleNamespace(version=types.SimpleNamespace(cuda="13.0"))
    scope["command_output"] = lambda args: (0, "nvcc release 13.0")
    answers = []
    for case in ("managed", "host_path", "cudacxx", "flashinfer", "compiler_escape", "library_escape", "library_version", "no_cpp"):
        os.environ.update(CUDA_HOME=str(root), CUDA_PATH=str(root), CUDA_MANAGED_ROOT=str(root), CUDACXX=str(compiler), FLASHINFER_NVCC=str(compiler), PATH=str(root / "bin") + os.pathsep + str(host))
        scope["loaded_cuda_runtime"] = lambda: ("13.0", str(library))
        if case == "host_path": os.environ["PATH"] = str(host) + os.pathsep + str(root / "bin")
        if case == "cudacxx": os.environ["CUDACXX"] = str(host_compiler)
        if case == "flashinfer": os.environ["FLASHINFER_NVCC"] = str(host_compiler)
        if case == "compiler_escape":
            compiler.unlink()
            compiler.symlink_to(host_compiler)
        if case == "library_escape":
            library.unlink()
            library.symlink_to(outside)
        if case == "library_version": scope["loaded_cuda_runtime"] = lambda: ("12.8", str(library))
        if case == "no_cpp": os.environ["PATH"] = str(root / "bin")
        try:
            actual = scope["check"]("managed_toolkit")
            answers.append(dict(name=case, passed=True, actual=actual))
        except Exception as error:
            answers.append(dict(name=case, passed=False, actual=str(error)))
        if compiler.is_symlink():
            compiler.unlink()
            compiler.write_text("synthetic")
            compiler.chmod(0o700)
        if library.is_symlink():
            library.unlink()
            library.write_text("synthetic")
    print(json.dumps(answers))
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-B", "-c", script, string(data)).CombinedOutput()
	require.NoError(t, err, string(output))
	var results []RuntimeCheck
	require.NoError(t, json.Unmarshal(output, &results))
	require.Len(t, results, 8)
	assert.True(t, results[0].Passed, results[0].Actual)
	reasons := []string{"PATH does not resolve", "CUDACXX compiler override", "FlashInfer", "compiler escaped", "library escaped", "runtime and Torch CUDA build", "C++ compiler required"}
	for index, result := range results[1:] {
		assert.False(t, result.Passed, result.Name+": "+result.Actual)
		if result.Name == "flashinfer" {
			assert.Contains(t, result.Actual, "FLASHINFER_NVCC compiler override")
		} else {
			assert.Contains(t, result.Actual, reasons[index])
		}
		if result.Name == "library_escape" {
			assert.Contains(t, result.Actual, filepath.Join("host", "libcudart.so.13"), "name the escaped symlink target")
		}
	}
}

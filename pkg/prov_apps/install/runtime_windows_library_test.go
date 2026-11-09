package install

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestWindowsLoadedRuntimeUsesExistingModuleHandles(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python fixture unavailable")
	}
	data, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	// Synthetic Win32 bindings exercise bounds and pointer-sized handles without CUDA DLLs.
	script := `import ctypes, json, os, sys, tempfile, types
scope = {"__name__": "windows_library_fixture"}
exec(json.loads(sys.argv[1]), scope)
with tempfile.TemporaryDirectory(prefix="cuda-é-") as root:
    root = os.path.realpath(root)
    sys.prefix = root
    scope["os"] = types.SimpleNamespace(name="nt", path=os.path, environ={"CUDA_MANAGED_ROOT": root})
    class Function:
        def __init__(self, fn): self.fn = fn
        def __call__(self, *args): return self.fn(*args)
    initialized, seen, paths, versions = False, [], [], {}
    def initialize():
        global initialized
        initialized = True
    sys.modules["torch"] = types.SimpleNamespace(cuda=types.SimpleNamespace(init=initialize))
    def enumerate_modules(process, buffer, size, needed, flags):
        assert initialized and flags == 3
        needed._obj.value = len(paths) * ctypes.sizeof(ctypes.c_void_p)
        if case == "overflow": needed._obj.value = size + ctypes.sizeof(ctypes.c_void_p)
        if case == "unaligned": needed._obj.value += 1
        for i in range(len(paths)): buffer[i] = (1 << 40) + i
        return 0 if case == "enumeration_failed" else 1
    def module_path(handle, buffer, capacity):
        if case == "truncated": return capacity
        if case == "path_failed": return 0
        buffer.value = paths[handle - (1 << 40)]
        return len(buffer.value)
    kernel = types.SimpleNamespace(GetCurrentProcess=Function(lambda: -1), K32EnumProcessModulesEx=Function(enumerate_modules), GetModuleFileNameW=Function(module_path))
    def library(name, **kwargs):
        if name == "kernel32": return kernel
        assert kwargs == dict(handle=(1 << 40) + paths.index(name)), "must reuse a loaded handle"
        seen.append(name)
        def query(pointer):
            pointer._obj.value = versions.get(name, 13020)
            return 0
        return types.SimpleNamespace(cudaRuntimeGetVersion=Function(query))
    ctypes.WinDLL = library
    ctypes.get_last_error = lambda: 5
    first, second = root + "/cudart64_13.dll", root + "/wheel/cudart64_13.dll"
    answers = []
    for case in ("unicode_one", "two", "conflicting", "external", "missing", "overflow", "unaligned", "enumeration_failed", "truncated", "path_failed"):
        initialized, seen, versions = False, [], {}
        paths = [first, root + "/nvcuda.dll"]
        if case in ("two", "conflicting"): paths.append(second)
        if case == "conflicting": versions[second] = 13000
        if case == "external": paths.append(root + "-outside/cudart64_13.dll")
        if case == "missing": paths = [root + "/nvcuda.dll"]
        paths = [os.path.realpath(path) for path in paths]
        try:
            version, path = scope["loaded_cuda_runtime"]()
            answers.append(dict(case=case, passed=True, version=version, count=len(seen)))
        except Exception as error:
            answers.append(dict(case=case, passed=False, reason=str(error), count=len(seen)))
    print(json.dumps(answers))
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-B", "-c", script, string(data)).CombinedOutput()
	require.NoError(t, err, string(output))
	var results []struct {
		Case, Version, Reason string
		Passed                bool
		Count                 int
	}
	require.NoError(t, json.Unmarshal(output, &results))
	require.Len(t, results, 10)
	for n, count := range []int{1, 2} {
		require.True(t, results[n].Passed, results[n].Reason)
		require.Equal(t, "13.2", results[n].Version)
		require.Equal(t, count, results[n].Count)
	}
	for n, reason := range []string{"conflicting versions", "escaped managed runtime", "0 candidates", "observation limit", "observation limit", "enumeration failed", "truncated", "unavailable"} {
		require.False(t, results[n+2].Passed, results[n+2].Case)
		require.Contains(t, results[n+2].Reason, reason)
		if n > 0 {
			require.Zero(t, results[n+2].Count, "refusals precede CUDA DLL queries")
		}
	}
}

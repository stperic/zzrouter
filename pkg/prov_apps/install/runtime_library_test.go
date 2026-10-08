package install

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadedRuntimeInitializesAndChecksEveryMappedCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux CUDA library map fixture")
	}
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	data, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	// Synthetic CUDA bindings reproduce lazy loading and separate managed copies.
	script := `import builtins, ctypes, io, json, os, sys, tempfile, types
scope = {"__name__": "library_fixture"}
exec(json.loads(sys.argv[1]), scope)
with tempfile.TemporaryDirectory() as root:
    root = os.path.realpath(root)
    sys.prefix = root
    os.environ["CUDA_MANAGED_ROOT"] = root
    initialized = False
    paths, versions, seen = [], {}, []
    def initialize():
        global initialized
        initialized = True
    sys.modules["torch"] = types.SimpleNamespace(cuda=types.SimpleNamespace(init=initialize))
    original_open = builtins.open
    def mapped_open(path, *args, **kwargs):
        if path != "/proc/self/maps": return original_open(path, *args, **kwargs)
        assert initialized, "mapping observed before lazy CUDA initialization"
        return io.StringIO("".join("0-1 r-xp 0 0:0 0 " + path + "\n" for path in paths))
    builtins.open = mapped_open
    class Query:
        def __init__(self, path): self.path = path
        def __call__(self, pointer):
            pointer._obj.value = versions.get(self.path, 13000)
            return 0
    def loaded_library(path, **kwargs):
        assert kwargs["mode"] & os.RTLD_NOLOAD, "probe must not load a new library"
        seen.append(path)
        return types.SimpleNamespace(cudaRuntimeGetVersion=Query(path))
    ctypes.CDLL = loaded_library
    answers = []
    first, second = root + "/libcudart.so.13", root + "/wheel/libcudart.so.13"
    alias = root + "/libcudart.so.alias"
    os.symlink(first, alias)
    external = root + "z-outside/libcudart.so.13"
    escaped_alias = root + "/libcudart.so.external.alias"
    os.symlink(external, escaped_alias)
    for case in ("lazy_one", "two_managed", "alias", "external", "conflicting", "absent", "external_many", "external_alias", "conflicting_fifth"):
        initialized, seen, versions = False, [], {}
        expected, excluded = [], []
        paths = [first]
        if case == "two_managed": paths += [second]
        if case == "alias": paths += [alias, first]
        if case == "external":
            paths += [external]
            expected = [external]
        if case == "conflicting":
            paths += [second]
            versions[second] = 12080
            expected = [first, second]
        if case == "absent": paths = []
        if case == "external_many":
            outside = [external + "." + str(n) for n in range(6)]
            paths += outside
            expected, excluded = outside[:4], outside[4:]
        if case == "external_alias":
            paths += [escaped_alias]
            expected, excluded = [external], [escaped_alias]
        if case == "conflicting_fifth":
            paths += [root + "/wheel" + str(n) + "/libcudart.so.13" for n in range(1, 5)]
            versions[paths[-1]] = 12080
            expected, excluded = paths[:4], paths[4:]
        try:
            version, path = scope["loaded_cuda_runtime"]()
            answers.append(dict(case=case, passed=True, version=version, path=path, count=len(seen)))
        except Exception as error:
            answers.append(dict(case=case, passed=False, reason=str(error), count=len(seen), expected=expected, excluded=excluded))
    print(json.dumps(answers))
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-B", "-c", script, string(data)).CombinedOutput()
	require.NoError(t, err, string(output))
	var results []struct {
		Case     string   `json:"case"`
		Passed   bool     `json:"passed"`
		Version  string   `json:"version"`
		Reason   string   `json:"reason"`
		Count    int      `json:"count"`
		Expected []string `json:"expected"`
		Excluded []string `json:"excluded"`
	}
	require.NoError(t, json.Unmarshal(output, &results))
	require.Len(t, results, 9)
	for n, count := range []int{1, 2, 1} {
		assert.True(t, results[n].Passed, results[n].Reason)
		assert.Equal(t, "13.0", results[n].Version)
		assert.Equal(t, count, results[n].Count)
	}
	for n, reason := range []string{"escaped managed runtime", "conflicting versions", "0 candidates"} {
		assert.False(t, results[n+3].Passed, results[n+3].Case)
		assert.Contains(t, results[n+3].Reason, reason)
	}
	for _, n := range []int{3, 6, 7} {
		assert.False(t, results[n].Passed, results[n].Case)
		assert.Contains(t, results[n].Reason, "escaped managed runtime")
		assert.Zero(t, results[n].Count, "reject external mappings before any library query")
	}
	assert.Contains(t, results[6].Reason, "(+2 more)")
	assert.False(t, results[8].Passed, "a conflicting fifth copy must still be checked")
	assert.Equal(t, 5, results[8].Count)
	for _, n := range []int{4, 8} {
		assert.Contains(t, results[n].Reason, "conflicting versions")
		assert.Contains(t, results[n].Reason, "12.8")
		assert.Contains(t, results[n].Reason, "13.0")
	}
	for _, result := range results {
		previous := -1
		for _, path := range result.Expected {
			assert.Contains(t, result.Reason, path)
			index := strings.Index(result.Reason, path)
			assert.Greater(t, index, previous, "diagnostic paths must be deterministic")
			previous = index
		}
		for _, path := range result.Excluded {
			assert.NotContains(t, result.Reason, path, "only bounded realpaths should be reported")
		}
	}
}

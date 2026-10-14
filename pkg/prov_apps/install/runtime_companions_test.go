package install

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestCUDACompanionsUseDeclaredPackages(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	data, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	script := `import json, sys, types
scope = {"__name__": "companions_fixture"}
exec(json.loads(sys.argv[1]), scope)
requested = []
audio = 12080
torch = types.SimpleNamespace(version=types.SimpleNamespace(cuda="12.8"), ops=types.SimpleNamespace(
    _torchaudio=types.SimpleNamespace(cuda_version=lambda: audio),
    torchvision=types.SimpleNamespace(_cuda_version=lambda: 12080)))
def load(name):
    requested.append(name)
    return torch if name == "torch" else types.SimpleNamespace()
scope["importlib"] = types.SimpleNamespace(import_module=load)
scope["cuda_companions"](["torch", "torchvision"])
assert requested == ["torch", "torchvision"], requested
requested.clear()
scope["cuda_companions"](["vllm", "torch", "torchaudio", "torchvision"])
assert requested == ["torch", "torchaudio", "torchvision"], requested
audio = 12070
try:
    scope["cuda_companions"](["torch", "torchaudio", "torchvision"])
except scope["CheckFailure"]:
    pass
else:
    raise AssertionError("vLLM still requires all declared companion builds to match")
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-c", script, string(data)).CombinedOutput()
	require.NoError(t, err, string(output))
}

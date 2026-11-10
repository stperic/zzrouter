package install

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stretchr/testify/require"
)

func TestMPSProbeRequiresActualTensorExecution(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	probe, err := json.Marshal(runtimeProbe)
	require.NoError(t, err)
	script := `import json, sys, types
scope = {"__name__": "probe_library"}
exec(json.loads(sys.argv[1]), scope)
scope["toolkit"] = lambda: dict(path="", version="", source="", reason="")
results = {}
for kind in ["unavailable", "broken_import", "broken_operator", "cpu_fallback", "wrong_value", "working"]:
    calls = []
    class Tensor:
        def __init__(self):
            self.device = types.SimpleNamespace(type="cpu" if kind == "cpu_fallback" else "mps")
            self.value = 1
        def add_(self, amount):
            calls.append("add")
            if kind == "broken_operator":
                raise RuntimeError("operator failed on MPS")
            self.value += amount
            return self
        def item(self):
            calls.append("item")
            return 3 if kind == "wrong_value" else self.value
    def ones(count, *, device):
        assert count == 1 and device == "mps"
        calls.append("ones:mps")
        return Tensor()
    def synchronize():
        calls.append("synchronize")
    sys.modules["torch"] = None if kind == "broken_import" else types.SimpleNamespace(
        backends=types.SimpleNamespace(mps=types.SimpleNamespace(is_available=lambda: kind != "unavailable")),
        mps=types.SimpleNamespace(synchronize=synchronize), ones=ones)
    checks = scope["run"]({"checks": ["mps_available"]}, sys.argv[2])
    environment = scope["environment"]({"checks": ["mps_available"]})
    results[kind] = dict(checks=checks, calls=calls, environment=environment)
print(json.dumps(results))
`
	output, err := host.CommandContext(t.Context(), python, "-I", "-B", "-c", script, string(probe), t.TempDir()).CombinedOutput()
	require.NoError(t, err, string(output))
	var results map[string]struct {
		Checks []struct {
			Name   string `json:"name"`
			Passed bool   `json:"passed"`
			Reason string `json:"reason"`
		} `json:"checks"`
		Calls       []string           `json:"calls"`
		Environment RuntimeEnvironment `json:"environment"`
	}
	require.NoError(t, json.Unmarshal(output, &results))
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			require.Len(t, result.Checks, 1)
			require.Equal(t, "mps_available", result.Checks[0].Name)
			require.Equal(t, name == "working", result.Checks[0].Passed)
			require.Nil(t, result.Environment.MetalAvailable, "Torch probes must not claim MLX availability")
			if name == "working" {
				require.Equal(t, []string{"ones:mps", "add", "synchronize", "item"}, result.Calls)
				require.Empty(t, result.Checks[0].Reason)
			} else {
				require.NotEmpty(t, result.Checks[0].Reason)
			}
			if name == "broken_import" {
				require.Nil(t, result.Environment.MPSAvailable)
				require.Contains(t, result.Environment.MPSReason, "ModuleNotFoundError")
			} else {
				require.NotNil(t, result.Environment.MPSAvailable)
				require.Equal(t, name != "unavailable", *result.Environment.MPSAvailable)
				require.Empty(t, result.Environment.MPSReason)
			}
		})
	}
}

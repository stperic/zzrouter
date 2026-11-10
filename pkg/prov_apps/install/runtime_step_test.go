package install

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"

	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/host"

	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func TestHookedRuntimeChecksRunDirectlyBeforePostHook(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python fixture unavailable")
	}
	venv := filepath.Join(t.TempDir(), "venv")
	output, err := host.CommandContext(t.Context(), python, "-m", "venv", venv).CombinedOutput()
	require.NoError(t, err, string(output))
	python = filepath.Join(venv, "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(venv, "Scripts", "python.exe")
	}
	for _, mode := range []string{"success", "guard fails", "check fails", "hook-only activation"} {
		t.Run(mode, func(t *testing.T) {
			events := []string{}
			checks := schema.RuntimeChecks{Checks: []string{"pip_check", "imports"}, Imports: []string{"json"}, Kernels: "unknown"}
			refused := errors.New("guard refused")
			step := Step{
				Command: "must-not-run-as-a-shell-command " + strings.Repeat("x", 9000),
				PreExec: func(context.Context) error {
					events = append(events, "guard")
					if mode == "guard fails" {
						return refused
					}
					return nil
				},
				PostExec: func(context.Context) error { events = append(events, "post"); return nil },
				Verify: StepVerify{Type: "runtime_checks", Python: python, RuntimeChecks: &checks,
					PreVerify: func(context.Context) error { events = append(events, "checks"); return nil }},
			}
			if mode == "check fails" {
				checks.Imports = []string{"zzrouter_nonexistent_module_fixture"}
			}
			if mode == "hook-only activation" {
				step.Command = ""
				step.Verify.Python = "not-present-before-activation"
			}
			err := executeStep(t.Context(), step)
			switch mode {
			case "guard fails":
				require.ErrorIs(t, err, refused)
				require.Equal(t, []string{"guard"}, events)
			case "check fails":
				require.ErrorContains(t, err, "zzrouter_nonexistent_module_fixture")
				require.Equal(t, []string{"guard", "checks"}, events)
			case "hook-only activation":
				require.NoError(t, err)
				require.Equal(t, []string{"guard", "post"}, events, "activation must precede verification of the live path")
			default:
				require.NoError(t, err)
				require.Equal(t, []string{"guard", "checks", "post"}, events)
			}
		})
	}
}

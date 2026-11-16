package install

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/host"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stretchr/testify/require"
)

func commandPlan() Plan {
	return Plan{Platform: fsroot.Platform{OS: "linux"}, Steps: []Step{{Number: 1, Command: "printf '%s' ok"}}}
}

func TestScriptExportRefusesManagedContext(t *testing.T) {
	hook := func(context.Context) error { return nil }
	for name, change := range map[string]func(*Plan){
		"unknown platform":                   func(p *Plan) { p.Platform.OS = "" },
		"windows":                            func(p *Plan) { p.Platform.OS = "windows" },
		"empty":                              func(p *Plan) { p.Steps = nil },
		"partial":                            func(p *Plan) { p.Steps = append(p.Steps, Step{Number: 2}) },
		"preflight":                          func(p *Plan) { p.Preflight = hook },
		"rollback":                           func(p *Plan) { p.Rollback = func() error { return nil } },
		"recipe":                             func(p *Plan) { p.Recipe = &RecipeSnapshot{} },
		"disposable":                         func(p *Plan) { p.Disposable = true },
		"plan environment":                   func(p *Plan) { p.Environment = []string{"TOKEN=private-value"} },
		"step environment":                   func(p *Plan) { p.Steps[0].Env = map[string]string{"TOKEN": "private-value"} },
		"execution environment":              func(p *Plan) { p.Steps[0].ExecutionEnvironment = []string{"TOKEN=private-value"} },
		"explicit empty environment":         func(p *Plan) { p.Steps[0].ExecutionEnvironment = []string{} },
		"service":                            func(p *Plan) { p.Steps[0].ServiceAction = "restart" },
		"stdin":                              func(p *Plan) { p.Steps[0].Stdin = "private-value" },
		"user":                               func(p *Plan) { p.Steps[0].RunAs = "service" },
		"directory":                          func(p *Plan) { p.Steps[0].WorkingDir = "/managed" },
		"timeout":                            func(p *Plan) { p.Steps[0].Timeout = time.Second },
		"optional":                           func(p *Plan) { p.Steps[0].Optional = true },
		"pre-exec":                           func(p *Plan) { p.Steps[0].PreExec = hook },
		"post-exec":                          func(p *Plan) { p.Steps[0].PostExec = hook },
		"stdout":                             func(p *Plan) { p.Steps[0].StdoutLine = func(string) {} },
		"pty":                                func(p *Plan) { p.Steps[0].UsePTY = true },
		"typed verification":                 func(p *Plan) { p.Steps[0].Verify.Type = "runtime_checks" },
		"runtime checks":                     func(p *Plan) { p.Steps[0].Verify.RuntimeChecks = &schema.RuntimeChecks{} },
		"pre-verify":                         func(p *Plan) { p.Steps[0].Verify.PreVerify = hook },
		"verification environment":           func(p *Plan) { p.Steps[0].Verify.Environment = map[string]string{"TOKEN": "private-value"} },
		"verification execution environment": func(p *Plan) { p.Steps[0].Verify.ExecutionEnvironment = []string{} },
		"verification toolkit":               func(p *Plan) { p.Steps[0].Verify.Toolkit = ToolkitSelection{root: "/managed"} },
	} {
		t.Run(name, func(t *testing.T) {
			plan := commandPlan()
			change(&plan)
			plan.PrepareScriptExport()
			require.Empty(t, plan.ScriptExport.Script, "no partial export")
			require.NotEmpty(t, plan.ScriptExport.Reason)
			require.NotContains(t, plan.ScriptExport.Reason, "private-value")
			wire, err := json.Marshal(plan)
			require.NoError(t, err)
			var forwarded Plan
			require.NoError(t, json.Unmarshal(wire, &forwarded))
			require.Equal(t, plan.ScriptExport, forwarded.ScriptExport, "refusal survives loss of hidden context")
		})
	}
}

func TestScriptExportRefusesGlobalInstallEnvironment(t *testing.T) {
	fsroot.SetExecEnv(map[string]string{"HTTPS_PROXY": "private-value"})
	t.Cleanup(func() { fsroot.SetExecEnv(nil) })
	plan := commandPlan()
	plan.PrepareScriptExport()
	require.Empty(t, plan.ScriptExport.Script)
	require.NotEmpty(t, plan.ScriptExport.Reason)
	wire, err := json.Marshal(plan)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "private-value")
}

func TestScriptExportPreservesShellSemantics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX export")
	}
	plan := commandPlan()
	plan.Provider = "untrusted\nprintf injected"
	plan.Steps = []Step{
		{Number: 1, Description: "untrusted\nprintf injected", Command: "value=private; export value; cd /; printf '%s\\n' \"literal ' quote\""},
		{Number: 2, Command: "test -z \"${value:-}\" || exit 8; printf '%s\\n' \"$PWD\""},
		{Number: 3, Command: "exit 7"},
		{Number: 4, Command: "printf should-not-run"},
	}
	plan.PrepareScriptExport()
	require.Empty(t, plan.ScriptExport.Reason)
	cmd := host.CommandContext(t.Context(), "sh")
	cmd.Stdin = strings.NewReader(plan.ScriptExport.Script)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Equal(t, 7, cmd.ProcessState.ExitCode())
	// macOS resolves /var to /private/var when a shell sets PWD.
	require.Contains(t, string(output), "literal ' quote\n")
	require.Contains(t, string(output), strings.TrimPrefix(cmd.Dir, "/private"))
	require.NotContains(t, string(output), "injected")
	require.NotContains(t, string(output), "should-not-run")
}

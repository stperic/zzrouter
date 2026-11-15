package install

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// ScriptExport contains either a complete command script or a refusal reason.
// It is prepared on the executing node before JSON drops managed context.
// Absence in an older peer's response is not permission to reconstruct a script.
type ScriptExport struct {
	Script string `json:"script,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// PrepareScriptExport must run after plan construction and identity binding.
// Verification and activation remain managed operations, even for command-only
// exports. This must never be called on a plan decoded from a worker response.
func (p *Plan) PrepareScriptExport() {
	export := p.commandScript()
	p.ScriptExport = &export
}

func (p *Plan) commandScript() ScriptExport {
	if p.Platform.OS != "linux" && p.Platform.OS != "darwin" {
		return ScriptExport{Reason: "Command scripts are available only for POSIX install plans. Use managed execution on this node."}
	}
	if len(p.Steps) == 0 {
		return ScriptExport{Reason: "This plan has no commands to export."}
	}
	if p.Preflight != nil || p.Rollback != nil || p.Recipe != nil || p.Disposable || p.Environment != nil || len(fsroot.ExecEnv()) > 0 {
		return ScriptExport{Reason: "This plan requires managed installation. Use Run all or run each step."}
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -e\n\n")
	for _, step := range p.Steps {
		if strings.TrimSpace(step.Command) == "" || step.requiresManagedExecution() || step.Verify.requiresManagedVerification() {
			return ScriptExport{Reason: fmt.Sprintf("Step %d requires managed execution. Use Run all or run this step.", step.Number)}
		}
		// Each managed step has its own shell. Quote the entire command so its
		// variables and redirects are interpreted only inside that shell.
		script.WriteString("sh -c '" + strings.ReplaceAll(step.Command, "'", "'\\''") + "'\n\n")
	}
	return ScriptExport{Script: script.String()}
}

func (s Step) requiresManagedExecution() bool {
	return s.ExecutionEnvironment != nil || s.ServiceAction != "" || s.Stdin != "" || s.RunAs != "" ||
		s.WorkingDir != "" || len(s.Env) > 0 || s.Timeout != 0 || s.Optional || s.PreExec != nil ||
		s.PostExec != nil || s.StdoutLine != nil || s.UsePTY
}

func (v StepVerify) requiresManagedVerification() bool {
	return v.Type == "runtime_checks" || v.RuntimeChecks != nil || v.PreVerify != nil ||
		v.ExecutionEnvironment != nil || len(v.Environment) > 0 || v.Toolkit != (ToolkitSelection{})
}

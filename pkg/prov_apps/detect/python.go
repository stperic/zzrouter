package detect

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// ProviderPython resolves the Python interpreter used to spawn a provider.
//
// Resolution order (first hit wins):
//
//  1. The provider's managed venv python, if zzRouter installed one
//     (providers/<name>/venv/bin/python3).
//  2. sc.Runtime.Execution.Command when the provider declares
//     execution.type = "python" — honors a user-specified interpreter
//     in the YAML (absolute path or PATH-resolvable name).
//  3. "python3" as a last-resort PATH fallback.
//
// The spawn path and any future detection caller MUST go through this
// function so the same interpreter that runs the model is the one we
// check for module availability. Splitting these two lookups was the
// original "provider not registered" bug on a fully-installed host.
func ProviderPython(name string, sc *config.ServiceConfig) string {
	if venv := fsroot.ProviderVenvPython(name); venv != "" {
		if _, err := os.Stat(venv); err == nil {
			return venv
		}
	}
	if sc != nil && sc.RunScope == config.RunScopeNode {
		return fsroot.ProviderVenvPython(name)
	}
	if sc != nil && sc.Runtime != nil && sc.Runtime.Execution.Command != "" {
		return sc.Runtime.Execution.Command
	}
	return "python3"
}

// executableBits detects a runnable interpreter rather than a source file with the same name.
const executableBits = 0111

// Runnable checks the executable and source in the operator's provider directory.
func Runnable(name string, sc *config.ServiceConfig) bool {
	if sc == nil || sc.Runtime == nil {
		return false
	}
	execution := sc.Runtime.Execution
	command := ProviderCLI(name, sc)
	if execution.Type == "python" {
		command = ProviderPython(name, sc)
	}
	binary, err := os.Stat(command)
	if err != nil || binary.IsDir() || (runtime.GOOS != "windows" && binary.Mode().Perm()&executableBits == 0) {
		return false
	}
	dir := execution.WorkingDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(fsroot.ProviderDir(name), dir)
	}
	source, err := os.Stat(dir)
	if err != nil || !source.IsDir() {
		return false
	}
	if execution.Type == "python" && len(execution.Args) > 0 && filepath.Ext(execution.Args[0]) == ".py" {
		file, err := os.Stat(filepath.Join(dir, execution.Args[0]))
		if err != nil || file.IsDir() {
			return false
		}
	}
	return true
}

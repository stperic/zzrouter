package detect

import (
	"os"

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
	if sc != nil && sc.Runtime != nil && sc.Runtime.Execution.Command != "" {
		return sc.Runtime.Execution.Command
	}
	return "python3"
}

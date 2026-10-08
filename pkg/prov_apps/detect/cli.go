package detect

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// ProviderCLI resolves the cli-binary used to spawn a provider whose
// execution.type is "cli". Coordinator-shipped configs carry bare names
// (`vllm`, `llama-server`) so the same YAML works on every host;
// resolution to an absolute path happens here, at spawn time, against
// this node's own install layout.
//
// Resolution order (first hit wins):
//
//  1. Runtime.Execution.Command as written, if it contains a path
//     separator AND names a file that exists here — an operator who
//     points at their own build gets exactly what they asked for.
//  2. providers/<name>/bin/<base> — where the llama.cpp / ollama-style
//     installers drop binaries.
//  3. providers/<name>/venv/bin/<base> — where pythonvenv-style
//     installers expose console scripts (vllm, mlx_lm.*).
//  4. The command unchanged — Go's exec.LookPath resolves a bare name
//     against $PATH for system-installed providers, and a stale
//     absolute path fails with the precise path it could not find.
//
// Steps 2 and 3 match on filepath.Base, so they also catch a command
// carrying ANOTHER node's absolute path. That is not hypothetical:
// execution.command lives in cluster-shared provider config, so a path
// naming one node's filesystem reaches every node. Resolving it here
// keeps the answer node-local no matter what the shared config says,
// which is why nothing writes an absolute path back into that config.
func ProviderCLI(name string, sc *config.ServiceConfig) string {
	if sc == nil || sc.Runtime == nil || sc.Runtime.Execution.Command == "" {
		return ""
	}
	cmd := sc.Runtime.Execution.Command
	// Only a command that spells out a location is stat-ed directly. A
	// bare name must not match a file that happens to share it in the
	// working directory.
	if strings.ContainsRune(cmd, os.PathSeparator) || strings.ContainsRune(cmd, '/') {
		if _, err := os.Stat(cmd); err == nil {
			return cmd
		}
	}
	if managed := managedBinary(name, filepath.Base(cmd)); managed != "" {
		return managed
	}
	return cmd
}

// managedBinary returns the path to base inside the directories
// zzRouter's own installers write to, or "" when this provider's binary
// is not one of ours.
func managedBinary(name, base string) string {
	venv := fsroot.ProviderVenvDir(name)
	dirs := []string{fsroot.ProviderBinDir(name), filepath.Join(venv, "bin")}
	names := []string{base}
	if runtime.GOOS == "windows" {
		// Windows venvs put console scripts in Scripts\, and the
		// installed file carries an extension that provider config
		// (written once, for every platform) does not spell out.
		dirs = append(dirs, filepath.Join(venv, "Scripts"))
		if !strings.EqualFold(filepath.Ext(base), ".exe") {
			names = append(names, base+".exe")
		}
	}
	for _, dir := range dirs {
		for _, n := range names {
			candidate := filepath.Join(dir, n)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
}

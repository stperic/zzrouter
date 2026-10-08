//go:build windows

package interpreter

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/config"
)

// defaultCandidates on Windows probes versioned and unversioned python
// executables that the official python.org installer and winget lay
// down on PATH. Windows-only Python Launcher (py.exe) resolution — the
// `py -3.13` form that reads the registry to find any Python the user
// has installed regardless of PATH — is a known follow-up, tracked
// alongside registry-backed enumeration. The primary incident this
// package fixes (Homebrew rolling the `python` formula on macOS) is
// a non-issue on Windows, so stubbing Windows here doesn't leave a
// production regression — it just means Windows users still need
// their versioned interpreter on PATH.
func defaultCandidates() []string {
	return []string{
		"python3.13.exe",
		"python3.12.exe",
		"python3.11.exe",
		"python3.10.exe",
		"python3.9.exe",
		"python3.exe",
		"python.exe",
	}
}

func preferredInstallTarget(req *config.PythonRequirement) string {
	if req == nil || req.Max == "" {
		return "3.13"
	}
	m, ok := parseMinorTuple(req.Max)
	if !ok || m.minor <= 1 {
		return "3.13"
	}
	suggest := m.minor - 1
	if suggest < 9 {
		suggest = 9
	}
	return fmt.Sprintf("%d.%d", m.major, suggest)
}

func installHintForTarget(target string) string {
	return fmt.Sprintf(
		"Install Python %s from python.org or via winget:\n"+
			"  winget install Python.Python.%s\n"+
			"The Python Launcher for Windows (py.exe) will pick it up automatically.",
		target, target,
	)
}

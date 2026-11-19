//go:build !windows

package interpreter

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// defaultCandidates lists the probes tried on Unix-likes. Ordered
// newest-minor-first to reduce the chance that python3 alone wins on
// systems where only the generic alias is installed, but the final
// tiebreaker inside Select is version number, not position — the order
// here just controls which binaries we even look at.
//
// "python3" stays at the end as a fallback for minimal Docker images
// and stripped Linux distros where only the unversioned name exists.
func versionProbeArgs() []string {
	if runtime.GOOS == "darwin" {
		arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
		return []string{"-I", "-S", "-c", "import platform,sys;sys.exit(1) if platform.machine()!=sys.argv[1] else print('Python '+sys.version.split()[0])", arch}
	}
	return []string{"--version"}
}

func defaultCandidates() []string {
	names := []string{
		"python3.13",
		"python3.12",
		"python3.11",
		"python3.10",
		"python3.9",
		"python3",
	}
	if runtime.GOOS != "darwin" {
		return names
	}
	// launchd does not inherit shell PATH; prefer stable installed aliases.
	var candidates []string
	for _, name := range names[:len(names)-1] {
		version := strings.TrimPrefix(name, "python")
		for _, prefix := range []string{"/opt/homebrew", "/usr/local"} {
			candidates = append(candidates, filepath.Join(prefix, "opt", "python@"+version, "bin", name))
		}
		candidates = append(candidates, filepath.Join("/Library/Frameworks/Python.framework/Versions", version, "bin", name))
	}
	return append(candidates, names...)
}

// preferredInstallTarget returns the minor we'd suggest the user install
// when Select returns ErrNoMatch. We bias toward the HIGHEST version
// still allowed by the range so the user gets the most up-to-date
// interpreter the provider actually supports — the library authors
// haven't validated newer minors yet, but anything older is strictly
// a worse position to be in.
func preferredInstallTarget(req *config.PythonRequirement) string {
	if req == nil || req.Max == "" {
		return "3.13"
	}
	m, ok := parseMinorTuple(req.Max)
	if !ok || m.minor <= 1 {
		return "3.13"
	}
	// Max is exclusive: max=3.14 → suggest 3.13.
	suggest := m.minor - 1
	if suggest < 9 {
		suggest = 9 // floor; anything older is unsupported ground
	}
	return fmt.Sprintf("%d.%d", m.major, suggest)
}

//go:build !windows

package interpreter

import (
	"fmt"

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
func defaultCandidates() []string {
	return []string{
		"python3.13",
		"python3.12",
		"python3.11",
		"python3.10",
		"python3.9",
		"python3",
	}
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

//go:build linux

package interpreter

import "fmt"

// installHintForTarget returns the Linux-specific install suggestion.
// Distro package names for versioned Python aren't uniform — Debian
// and Ubuntu ship `pythonX.Y` as apt packages, Fedora ships them under
// the same name via dnf, and source/pyenv is the escape hatch for older
// LTS distros that don't carry the minor. We give both flavors here
// rather than detecting the distro; the user knows which they're on
// and a two-line hint is cheaper than a wrong one-line guess.
func installHintForTarget(target string) string {
	return fmt.Sprintf(
		"Install Python %s using your distro's package manager:\n"+
			"  Debian/Ubuntu: sudo apt install python%s python%s-venv\n"+
			"  Fedora/RHEL:   sudo dnf install python%s\n"+
			"Or use pyenv for distros that don't package the minor:\n"+
			"  pyenv install %s.0",
		target, target, target, target, target,
	)
}

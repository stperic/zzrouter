//go:build darwin

package interpreter

import "fmt"

// installHintForTarget returns the macOS-specific install suggestion.
// Homebrew's rolling `python` formula is exactly what got us into this
// mess — it moves the `python3` alias to the latest released minor on
// every `brew upgrade`. The hint therefore points the user at the
// *pinned* versioned keg (`python@3.13`) so the replacement interpreter
// isn't subject to the same drift the next time Homebrew advances its
// default.
func installHintForTarget(target string) string {
	return fmt.Sprintf(
		"Install Python %s via Homebrew's versioned keg:\n"+
			"  brew install python@%s\n"+
			"Then reinstall the provider. The versioned keg is pinned: "+
			"it won't drift when Homebrew's default `python` formula advances.",
		target, target,
	)
}

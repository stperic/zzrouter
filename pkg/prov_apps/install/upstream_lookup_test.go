package install

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// An upgrade with no explicit target must aim at what the node's own
// installer can fetch. Aiming at the project's newest tag on a node that
// installs through a packager hands brew a version it does not carry, and the
// run fails on the step that checks what was really installed — observed as
// "Homebrew installed 0.32.14. The ollama formula does not carry 0.32.15 yet".
//
// The two lookups differ only in this selection, so that is what is asserted:
// the reporting question ("what did the project publish") keeps the base
// source, the install question ("what can I fetch") goes through the override.
func TestInstallResolvesThroughThePlatformOverride(t *testing.T) {
	brew := &config.VersionSource{
		Type:    config.VersionSourceHomebrew,
		Formula: "ollama",
		Compare: config.CompareSemver,
	}
	base := &config.VersionSource{
		Type:      config.VersionSourceGitHubRelease,
		Repo:      "ollama/ollama",
		Compare:   config.CompareSemver,
		Platforms: map[string]*config.VersionSource{"darwin": brew},
	}

	assert.Same(t, brew, base.ForOS("darwin"),
		"a darwin node installs through the formula, not the release")
	assert.Same(t, base, base.ForOS("linux"),
		"a linux node builds a download URL from the project's own tag")

	// The lookup asks for the local platform, so on this host it must pick
	// whichever of the two governs here — never the other one.
	got := base.ForOS(runtime.GOOS)
	if runtime.GOOS == "darwin" {
		require.Same(t, brew, got)
	} else {
		require.Same(t, base, got)
	}
}

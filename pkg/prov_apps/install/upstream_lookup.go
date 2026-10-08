package install

import (
	"context"
	"fmt"
	"runtime"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
)

// ProviderVersionSource returns the version_source declared for a provider.
//
// Absence is an error rather than a silent fallback: the alternative is a
// hardcoded upstream in Go, which is the thing this key replaced. The
// providers/ templates ship the key and templates.ReconcileManagedSpine
// carries it onto existing installs at start, so a provider missing it has
// been hand-edited.
// It returns the base source, never a platform override: an installer runs
// on the node it is installing to, and asks "what tag do I fetch", which the
// project's own feed answers. The overrides exist for the reporting side,
// which asks the different question "what can this node reach" and must
// account for a packager in between. Keep them apart — resolving install
// through an override would make a node download whatever its packager
// happens to name, which is not a URL this code can build.
func ProviderVersionSource(provider string) (*config.VersionSource, error) {
	cfg, err := LoadAppsConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load provider config: %w", err)
	}

	// VersionSourceFor validates, so no caller can reach a URL template with
	// an unvalidated source — including the pinned-version path, which never
	// resolves a release but still interpolates src.Repo into a download URL.
	return cfg.VersionSourceFor(provider)
}

// LatestUpstreamRelease resolves the newest release the provider's configured
// upstream publishes.
//
// It returns the whole Release rather than one string because the two forms
// are not interchangeable: Tag is the raw upstream identifier that upstream's
// own URLs are keyed on ("b10502"), while Version has strip_prefix applied
// ("10502"). A caller picking the wrong one, or re-deriving one from the
// other, would put a second copy of strip_prefix in Go.
func LatestUpstreamRelease(ctx context.Context, provider string) (upstream.Release, error) {
	src, err := ProviderVersionSource(provider)
	if err != nil {
		return upstream.Release{}, err
	}
	return upstream.Latest(ctx, src)
}

// LatestInstallableRelease resolves the newest release this node can actually
// install, which is what an upgrade with no explicit target must aim at.
//
// It differs from LatestUpstreamRelease wherever a packager sits between the
// project and the node. ollama on macOS installs through Homebrew, and the
// formula trails the GitHub release by a day or two; aiming a blank upgrade
// at the project's tag there asks brew for a version it does not carry, and
// the run fails on the step that checks what brew actually installed. The
// platform override names the packager, so resolving through it asks the
// only question the installer can act on.
//
// The base source still answers for every platform without an override,
// which is where the tag is also the thing being downloaded.
func LatestInstallableRelease(ctx context.Context, provider string) (upstream.Release, error) {
	src, err := ProviderVersionSource(provider)
	if err != nil {
		return upstream.Release{}, err
	}
	return upstream.Latest(ctx, src.ForOS(runtime.GOOS))
}

// Package upstream resolves the newest release an installable provider's
// upstream has published, so zzRouter can report whether a node is running
// something old.
//
// It answers exactly one question: what exists upstream right now. It does
// not know what is installed on any node, and it deliberately does not claim
// that the release it names can be installed on a given host. For the pip
// providers in particular, installability depends on the target node's
// interpreter and platform, which this package never inspects.
//
// Every fetch target is derived from a config.VersionSource, whose Type is a
// closed enum owning a fixed URL template. Provider config supplies an
// identifier, never a URL.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/download"
)

// ErrNoUpstreamRelease means the feed exists but upstream has published
// nothing. Callers must tell this apart from a transport failure: neither
// yields a version, but only one is an outage. The companion case, a provider
// that declares no feed at all, is config.ErrNoVersionSource.
var ErrNoUpstreamRelease = errors.New("upstream has published no release")

// resolveTimeout bounds a single upstream lookup. Matches the install-side
// budget in pkg/prov_apps/install/download, which was raised from Go's
// defaults to absorb slow tailnet and cloud-link TLS handshakes.
const resolveTimeout = 30 * time.Second

// Resolver owns the origins and HTTP clients a lookup needs.
//
// It exists so the origins are values rather than package-level variables
// that tests mutate: a test builds its own Resolver pointed at an httptest
// server, which keeps the fetch targets unreachable from config AND keeps
// tests safe to run in parallel.
type Resolver struct {
	githubBase string
	// githubAPIBase serves the release-by-tag lookup behind AssetDigest.
	// Separate from githubBase because the asset digests live on the REST
	// API host, not the one the releases/latest redirect is read from.
	githubAPIBase string
	pypiBase      string
	homebrewBase  string
	// client follows redirects; noRedirect stops at the first hop so the
	// GitHub releases/latest Location header can be read.
	client     *http.Client
	noRedirect *http.Client
}

// NewResolver returns a Resolver pointed at the real origins.
func NewResolver() *Resolver {
	noRedirect := download.NewClient(resolveTimeout)
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Resolver{
		githubBase:    "https://github.com",
		githubAPIBase: "https://api.github.com",
		pypiBase:      "https://pypi.org",
		homebrewBase:  "https://formulae.brew.sh",
		client:        download.NewClient(resolveTimeout),
		noRedirect:    noRedirect,
	}
}

// defaultResolver backs the package-level Latest.
var defaultResolver = NewResolver()

// Release is the newest release an upstream publishes.
type Release struct {
	// Version is the identifier with VersionSource.StripPrefix removed. It
	// is what gets compared and displayed.
	Version string `json:"version"`
	// Tag is the raw upstream identifier, retained because it is what an
	// install actually requests (llama.cpp installs "b10453", not "10453").
	Tag string `json:"tag"`
	// URL is the human-facing release page.
	URL string `json:"url,omitempty"`
}

// Latest resolves the newest release published for src.
//
// Callers get an error, never a zero Release plus nil, when the lookup
// fails: an unreachable upstream must be distinguishable from an upstream
// with no releases, because the two render differently.
func Latest(ctx context.Context, src *config.VersionSource) (Release, error) {
	return defaultResolver.Latest(ctx, src)
}

// Latest resolves the newest release published for src.
func (r *Resolver) Latest(ctx context.Context, src *config.VersionSource) (Release, error) {
	if err := src.Validate(); err != nil {
		return Release{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()

	switch src.Type {
	case config.VersionSourceGitHubRelease:
		return r.latestGitHubRelease(ctx, src)
	case config.VersionSourcePyPI:
		return r.latestPyPI(ctx, src)
	case config.VersionSourceHomebrew:
		return r.latestHomebrew(ctx, src)
	default:
		// Unreachable: Validate rejects unknown types. Kept so adding an
		// enum member without a resolver fails loudly instead of silently
		// reporting "no releases".
		return Release{}, fmt.Errorf("upstream: no resolver for type %q", src.Type)
	}
}

package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ErrNoVersionSource means a provider declares no version_source, so there is
// no upstream feed to consult. It lives here rather than in the resolver
// because it describes config, not upstream state, and callers need to tell
// "nobody tracks this provider" apart from "the feed is unreachable".
var ErrNoVersionSource = errors.New("provider declares no version_source")

// VersionSourceType names the upstream a provider publishes releases to.
// It is a closed enum: an unrecognized value is a config error rather than
// a URL to try, so provider config cannot introduce a new fetch target.
type VersionSourceType string

const (
	// VersionSourceGitHubRelease resolves the tag GitHub marks as the
	// latest release for a repository.
	VersionSourceGitHubRelease VersionSourceType = "github_release"
	// VersionSourcePyPI resolves the newest non-yanked distribution
	// published to the Python Package Index.
	VersionSourcePyPI VersionSourceType = "pypi"
	// VersionSourceHomebrew resolves the stable version of a Homebrew
	// formula. It exists because a packager, not the upstream project,
	// decides what a node installing via that packager can actually get:
	// reporting the GitHub release to a macOS node that installs through
	// Homebrew promises a version the node cannot install until the
	// formula catches up.
	VersionSourceHomebrew VersionSourceType = "homebrew"
)

// VersionCompare selects how two release identifiers are ordered.
type VersionCompare string

const (
	// CompareSemver orders major.minor.patch identifiers via pkg/version.
	CompareSemver VersionCompare = "semver"
	// ComparePEP440 orders Python version identifiers. Distinct from semver
	// because PEP 440 allows an epoch, a variable number of release
	// segments, and .postN/.devN suffixes that strict major.minor.patch
	// parsing cannot represent: twelve of vllm's published versions are not
	// three-segment. Ordering those as semver drops them, and dropping the
	// newest makes a stale node read as up to date.
	ComparePEP440 VersionCompare = "pep440"
	// CompareBuildNumber orders monotonic integer builds, such as
	// llama.cpp's "b10453", after StripPrefix is applied.
	CompareBuildNumber VersionCompare = "build_number"
	// CompareOpaque reports only equal or not-equal. Any scheme nobody has
	// reasoned about gets this: reporting "differs" is correct, whereas
	// guessing a direction is confidently wrong.
	CompareOpaque VersionCompare = "opaque"
)

// maxIdentifierLen bounds repo and package. GitHub caps owners at 39 and
// repos at 100 characters, and PEP 503 names are far shorter, so this is
// generous for anything real while keeping a pathological config from
// building a megabyte-long URL.
const maxIdentifierLen = 200

// Identifier charsets. Both patterns require the first and last character to
// be alphanumeric, which is what rejects "." and ".." and therefore keeps a
// crafted identifier from walking out of the URL template's path.
var (
	githubSegmentPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*[A-Za-z0-9]$|^[A-Za-z0-9]$`)
	pypiPackagePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*[A-Za-z0-9]$|^[A-Za-z0-9]$`)
	homebrewFormulaPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*[A-Za-z0-9]$|^[A-Za-z0-9]$`)
)

// KnownPlatformOS is the closed set of GOOS values a platform override may
// key on. Closed so a typo ("macos") is a config error rather than an
// override that silently never applies.
var KnownPlatformOS = []string{"darwin", "linux", "windows"}

// VersionSource declares where a provider's upstream publishes releases so
// zzRouter can report which one is newest. It answers "what exists
// upstream", never "what is installed here" and never "what is safe to
// install here".
//
// The source is an identifier, never a URL. Each Type owns a fixed URL
// template and the identifier is charset-validated, so a provider config
// cannot aim zzRouter's outbound fetcher at an arbitrary host.
type VersionSource struct {
	Type VersionSourceType `yaml:"type" json:"type"`
	// Repo is "owner/name", required for github_release.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Package is the distribution name, required for pypi.
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	// Formula is the Homebrew formula name, required for homebrew.
	Formula string `yaml:"formula,omitempty" json:"formula,omitempty"`
	// StripPrefix is trimmed from an upstream tag before it is compared or
	// reported: "v" for ollama's v0.32.14, "b" for llama.cpp's b10453.
	StripPrefix string `yaml:"strip_prefix,omitempty" json:"strip_prefix,omitempty"`
	// Compare selects the ordering. Absent means CompareOpaque.
	//
	// There is deliberately no "include prereleases" knob. Both sources
	// return stable releases by construction: GitHub excludes prereleases
	// from the tag it marks latest, and strict major.minor.patch parsing
	// rejects PEP 440 prerelease forms such as "0.28.0rc1". A knob that
	// could only ever be honoured in one direction would be a lie.
	Compare VersionCompare `yaml:"compare,omitempty" json:"compare,omitempty"`

	// Platforms overrides the whole source for nodes running a given GOOS.
	//
	// A cluster can install one provider by different means per platform —
	// ollama comes from the GitHub release on Linux and from Homebrew on
	// macOS — and each installer has its own ceiling. Without this, one
	// answer is wrong for one of the platforms, and it is wrong in the
	// direction that offers an upgrade the node cannot perform.
	//
	// Overrides do not merge: the entry replaces the source outright, so
	// reading one tells you the whole answer for that platform. Nesting is
	// one level; an override carrying its own Platforms is a config error.
	Platforms map[string]*VersionSource `yaml:"platforms,omitempty" json:"platforms,omitempty"`
}

// ForOS returns the source governing a node running goos, which is the
// platform override when one is declared and the base source otherwise.
//
// An empty goos means the caller does not know where the node runs; it gets
// the base source, because guessing a platform would produce a confident
// comparison against the wrong ceiling.
func (s *VersionSource) ForOS(goos string) *VersionSource {
	if s == nil || goos == "" {
		return s
	}
	if override, ok := s.Platforms[goos]; ok && override != nil {
		return override
	}
	return s
}

// Comparator returns the configured ordering, defaulting to CompareOpaque so
// an under-specified source degrades to "differs" instead of a guess.
func (s *VersionSource) Comparator() VersionCompare {
	if s == nil || s.Compare == "" {
		return CompareOpaque
	}
	return s.Compare
}

// Normalize trims StripPrefix from a raw upstream tag.
func (s *VersionSource) Normalize(tag string) string {
	tag = strings.TrimSpace(tag)
	if s == nil || s.StripPrefix == "" {
		return tag
	}
	return strings.TrimPrefix(tag, s.StripPrefix)
}

// Tag is the inverse of Normalize: it returns v in the shape upstream's
// release tags take, adding the declared prefix when it is missing.
//
// Both shapes reach an install request legitimately — the versions endpoint
// reports "latest" normalized and "latest_tag" raw, and a caller reads
// whichever it noticed first. Coercing here means neither choice produces a
// 404 from a URL built as ".../download/{version}/".
func (s *VersionSource) Tag(v string) string {
	v = strings.TrimSpace(v)
	if s == nil || s.StripPrefix == "" || v == "" {
		return v
	}
	if strings.HasPrefix(v, s.StripPrefix) {
		return v
	}
	// Only a value that looks like the normalized form gets the prefix.
	// Every scheme here normalizes to something starting with a digit
	// ("10502", "0.32.14"), so anything else is a tag in some other shape
	// and prefixing it would corrupt a value the caller meant literally.
	if v[0] < '0' || v[0] > '9' {
		return v
	}
	return s.StripPrefix + v
}

// Validate checks the closed enums and the identifier charset. It is the one
// place either is checked, so callers that have validated may build URLs from
// the identifier without re-escaping.
func (s *VersionSource) Validate() error {
	if s == nil {
		return fmt.Errorf("version_source: nil")
	}

	switch s.Comparator() {
	case CompareSemver, ComparePEP440, CompareBuildNumber, CompareOpaque:
	default:
		return fmt.Errorf("version_source: unknown compare %q (want semver, pep440, build_number or opaque)", s.Compare)
	}

	switch s.Type {
	case VersionSourceGitHubRelease:
		if s.Package != "" {
			return fmt.Errorf("version_source: package is not valid for type %q", s.Type)
		}
		if len(s.Repo) > maxIdentifierLen {
			return fmt.Errorf("version_source: repo is %d characters, over the %d limit", len(s.Repo), maxIdentifierLen)
		}
		owner, name, ok := strings.Cut(s.Repo, "/")
		if !ok {
			return fmt.Errorf("version_source: repo %q must be \"owner/name\"", s.Repo)
		}
		if !githubSegmentPattern.MatchString(owner) || !githubSegmentPattern.MatchString(name) {
			return fmt.Errorf("version_source: repo %q has an unusable owner or name", s.Repo)
		}
	case VersionSourceHomebrew:
		if s.Repo != "" || s.Package != "" {
			return fmt.Errorf("version_source: repo and package are not valid for type %q", s.Type)
		}
		if len(s.Formula) > maxIdentifierLen {
			return fmt.Errorf("version_source: formula is %d characters, over the %d limit", len(s.Formula), maxIdentifierLen)
		}
		if !homebrewFormulaPattern.MatchString(s.Formula) {
			return fmt.Errorf("version_source: formula %q is not a usable Homebrew formula name", s.Formula)
		}
	case VersionSourcePyPI:
		if s.Repo != "" {
			return fmt.Errorf("version_source: repo is not valid for type %q", s.Type)
		}
		if len(s.Package) > maxIdentifierLen {
			return fmt.Errorf("version_source: package is %d characters, over the %d limit", len(s.Package), maxIdentifierLen)
		}
		if !pypiPackagePattern.MatchString(s.Package) {
			return fmt.Errorf("version_source: package %q is not a usable distribution name", s.Package)
		}
		// Everything on PyPI is a PEP 440 identifier, and the index hands
		// back an unordered list rather than naming a latest. Requiring the
		// matching comparator here is what makes it structurally impossible
		// to silently drop a .postN release and report "up to date".
		if s.Comparator() != ComparePEP440 {
			return fmt.Errorf("version_source: type %q requires compare: pep440 (got %q); "+
				"the index returns an unordered list of PEP 440 identifiers", s.Type, s.Comparator())
		}
	default:
		return fmt.Errorf("version_source: unknown type %q (want github_release, pypi or homebrew)", s.Type)
	}

	if s.Formula != "" && s.Type != VersionSourceHomebrew {
		return fmt.Errorf("version_source: formula is not valid for type %q", s.Type)
	}

	for goos, override := range s.Platforms {
		if !slices.Contains(KnownPlatformOS, goos) {
			return fmt.Errorf("version_source: platform %q is not a known OS (want %s)",
				goos, strings.Join(KnownPlatformOS, ", "))
		}
		if override == nil {
			return fmt.Errorf("version_source: platform %q declares no source", goos)
		}
		// One level only. A nested override would make "which source governs
		// this node" depend on resolution order rather than on the config.
		if len(override.Platforms) > 0 {
			return fmt.Errorf("version_source: platform %q may not declare its own platforms", goos)
		}
		if err := override.Validate(); err != nil {
			return fmt.Errorf("version_source: platform %q: %w", goos, err)
		}
	}

	return nil
}

// VersionSourceFor returns the validated upstream feed declared for a
// provider. This is the single lookup: the installers reach it through
// install.ProviderVersionSource and the HTTP layer calls it directly, so
// neither can consult an unvalidated source.
//
// Returns ErrNoVersionSource when the provider exists but declares no feed,
// which is a reportable state ("not tracked") rather than a failure.
func (c *AppsConfig) VersionSourceFor(provider string) (*VersionSource, error) {
	var src *VersionSource
	switch {
	case c.GetOnDemand(provider) != nil:
		src = c.GetOnDemand(provider).VersionSource
	case c.GetExternal(provider) != nil:
		src = c.GetExternal(provider).VersionSource
	default:
		return nil, fmt.Errorf("%w: %q", ErrProviderNotFound, provider)
	}

	if src == nil {
		return nil, fmt.Errorf("provider %q: %w", provider, ErrNoVersionSource)
	}
	if err := src.Validate(); err != nil {
		return nil, fmt.Errorf("provider %q: %w", provider, err)
	}
	return src, nil
}

package upstream

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// semverSource is a github feed: semver is for tag-shaped identifiers, and
// config.Validate no longer lets a pypi feed use it.
func semverSource() *config.VersionSource {
	return &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "ollama/ollama", StripPrefix: "v", Compare: config.CompareSemver}
}

// pypiSource is the shape every PyPI feed must now use.
func pypiSource() *config.VersionSource {
	return &config.VersionSource{Type: config.VersionSourcePyPI, Package: "vllm", Compare: config.ComparePEP440}
}

func buildNumberSource() *config.VersionSource {
	return &config.VersionSource{
		Type:        config.VersionSourceGitHubRelease,
		Repo:        "ggml-org/llama.cpp",
		StripPrefix: "b",
		Compare:     config.CompareBuildNumber,
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name    string
		src     *config.VersionSource
		current string
		latest  string
		want    Status
	}{
		{"semver upstream ahead", semverSource(), "0.19.0", "0.27.1", StatusNewer},
		{"semver equal", semverSource(), "0.27.1", "0.27.1", StatusSame},
		{"semver current ahead", semverSource(), "0.28.0", "0.27.1", StatusOlder},
		{"semver patch only", semverSource(), "0.27.0", "0.27.1", StatusNewer},
		{"semver unparseable current", semverSource(), "not-a-version", "0.27.1", StatusUnknown},
		{"semver unparseable latest", semverSource(), "0.27.1", "nightly", StatusUnknown},

		{"build upstream ahead", buildNumberSource(), "b10453", "b10502", StatusNewer},
		{"build equal", buildNumberSource(), "b10502", "b10502", StatusSame},
		{"build current ahead", buildNumberSource(), "b10600", "b10502", StatusOlder},
		{"build prefix already stripped", buildNumberSource(), "10453", "b10502", StatusNewer},
		{"build unparseable", buildNumberSource(), "bXYZ", "b10502", StatusUnknown},

		// A missing operand must never read as "up to date".
		{"missing current", semverSource(), "", "0.27.1", StatusUnknown},
		{"missing latest", semverSource(), "0.27.1", "", StatusUnknown},
		{"both missing", semverSource(), "", "", StatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Compare(tt.src, tt.current, tt.latest))
		})
	}
}

// Opaque reports equality but never a direction, because guessing an order
// for an unreasoned scheme is worse than admitting ignorance.
func TestCompareOpaqueNeverClaimsDirection(t *testing.T) {
	src := &config.VersionSource{Type: config.VersionSourceGitHubRelease, Repo: "some/repo"}
	require.Equal(t, config.CompareOpaque, src.Comparator())

	assert.Equal(t, StatusSame, Compare(src, "2024-05-01", "2024-05-01"))
	assert.Equal(t, StatusUnknown, Compare(src, "2024-05-01", "2024-06-01"))
	assert.Equal(t, StatusUnknown, Compare(src, "2024-06-01", "2024-05-01"))
}

func TestNewest(t *testing.T) {
	tests := []struct {
		name     string
		src      *config.VersionSource
		versions []string
		want     string
	}{
		{
			// PEP 700 does not promise ordering, and lexical sort puts 0.9.2
			// above 0.27.1. Taking the last element would return 0.9.2.
			name:     "unordered list is ordered by comparator",
			src:      semverSource(),
			versions: []string{"0.19.0", "0.27.1", "0.9.2", "0.26.0"},
			want:     "0.27.1",
		},
		{
			name:     "lexical trap in reverse order",
			src:      semverSource(),
			versions: []string{"0.9.2", "0.27.1"},
			want:     "0.27.1",
		},
		{
			// PEP 440 prereleases fail strict major.minor.patch parsing, so
			// they drop out without a dedicated filter.
			name:     "pep440 prerelease skipped",
			src:      semverSource(),
			versions: []string{"0.27.1", "0.28.0rc1", "0.28.0a2"},
			want:     "0.27.1",
		},
		{
			name:     "dashed prerelease skipped",
			src:      semverSource(),
			versions: []string{"0.27.1", "0.28.0-rc1"},
			want:     "0.27.1",
		},
		{
			name:     "unparseable entries skipped",
			src:      semverSource(),
			versions: []string{"garbage", "0.27.1", "1.0"},
			want:     "0.27.1",
		},
		{
			name:     "build numbers",
			src:      buildNumberSource(),
			versions: []string{"b10453", "b10502", "b9999"},
			want:     "b10502",
		},
		{"empty input", semverSource(), nil, ""},
		{"nothing orderable", semverSource(), []string{"garbage", ""}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Newest(tt.src, tt.versions))
		})
	}
}

// Rendered from a real bug: the providers list showed ollama as
// "0.21.2 -> v0.32.14", where the v reads as part of the change.
func TestDisplayVersionMatchesInstalledShape(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		release   Release
		want      string
	}{
		{
			// ollama records the normalized version, so the tag's v must go.
			"ollama drops the v", "0.21.2",
			Release{Tag: "v0.32.14", Version: "0.32.14"}, "0.32.14",
		},
		{
			// llama.cpp records the raw tag, so the b must stay.
			"llamacpp keeps the b", "b10453",
			Release{Tag: "b10502", Version: "10502"}, "b10502",
		},
		{"no prefix", "0.19.1", Release{Tag: "0.27.1", Version: "0.27.1"}, "0.27.1"},
		{"unknown installed", "", Release{Tag: "v0.32.14", Version: "0.32.14"}, "0.32.14"},
		{"missing tag", "0.21.2", Release{Version: "0.32.14"}, "0.32.14"},
		{"missing version", "b10453", Release{Tag: "b10502"}, "b10502"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DisplayVersion(tt.installed, tt.release))
		})
	}
}

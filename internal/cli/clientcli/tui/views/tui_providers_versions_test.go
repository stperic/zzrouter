package views

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

func TestVersionCell(t *testing.T) {
	tests := []struct {
		name string
		app  shared.ProviderInfo
		want string
	}{
		{
			// Both sides are raw upstream tags. The pin is stored raw
			// ("b10453") while the normalized version has strip_prefix
			// applied, so mixing forms would render "b10453 → 10502".
			name: "newer shows the upgrade path",
			app:  shared.ProviderInfo{Version: "b10453", LatestVersion: "b10502", VersionStatus: "newer"},
			want: "b10453 → b10502",
		},
		{
			name: "same needs no annotation",
			app:  shared.ProviderInfo{Version: "b10502", LatestVersion: "b10502", VersionStatus: "same"},
			want: "b10502",
		},
		{
			// An unverified check must never imply currency.
			name: "unknown renders bare",
			app:  shared.ProviderInfo{Version: "b10453", VersionStatus: "unknown"},
			want: "b10453",
		},
		{
			name: "no check yet renders bare",
			app:  shared.ProviderInfo{Version: "b10453"},
			want: "b10453",
		},
		{
			name: "ahead of upstream is not an upgrade prompt",
			app:  shared.ProviderInfo{Version: "b10600", LatestVersion: "b10502", VersionStatus: "older"},
			want: "b10600",
		},
		{
			name: "missing version",
			app:  shared.ProviderInfo{},
			want: "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, versionCell(tt.app))
		})
	}
}

// The upgrade prompt must send an explicit version. With a pin set, a blank
// upgrade resolves back to the pin, so launching one from an "update
// available" row would be a no-op or a downgrade.
func TestUpgradeTargetFor(t *testing.T) {
	assert.Equal(t, "b10502", upgradeTargetFor(shared.ProviderInfo{
		Version: "b10453", LatestVersion: "b10502", VersionStatus: "newer",
	}))
	assert.Empty(t, upgradeTargetFor(shared.ProviderInfo{
		Version: "b10502", LatestVersion: "b10502", VersionStatus: "same",
	}))
	assert.Empty(t, upgradeTargetFor(shared.ProviderInfo{
		Version: "b10453", VersionStatus: "unknown",
	}))
}

func TestVersionDetailLinesStatesWhyItIsUnknown(t *testing.T) {
	lines := versionDetailLines(&pkgClient.ProviderVersionsResponse{
		Provider:     "llamacpp",
		Pinned:       "b10453",
		PinnedStatus: "unknown",
		Reason:       "upstream version checks are disabled in settings",
	})
	require.NotEmpty(t, lines)

	joined := flatten(lines)
	assert.Contains(t, joined, "unknown")
	assert.Contains(t, joined, "disabled")
}

func TestVersionDetailLinesRendersUpstream(t *testing.T) {
	lines := versionDetailLines(&pkgClient.ProviderVersionsResponse{
		Provider:     "llamacpp",
		Pinned:       "b10453",
		PinnedStatus: "newer",
		Latest:       "10502",
		LatestTag:    "b10502",
		ReleaseURL:   "https://github.com/ggml-org/llama.cpp/releases/tag/b10502",
		CheckedAt:    "2026-08-19T19:00:00Z",
		Source:       &pkgClient.VersionSourceInfo{Type: "github_release", Repo: "ggml-org/llama.cpp"},
		Nodes: []pkgClient.NodeVersionInfo{
			{Node: "coord", Installed: "b10453", Status: "newer"},
		},
	})

	joined := flatten(lines)
	// The raw tag, so it pairs with the pin rather than mixing forms.
	assert.Contains(t, joined, "b10502")
	assert.Contains(t, joined, "ggml-org/llama.cpp")
	assert.Contains(t, joined, "coord")
	assert.Contains(t, joined, "seeds new installs")
}

func TestVersionDetailLinesNilReport(t *testing.T) {
	assert.Nil(t, versionDetailLines(nil))
}

func TestVersionForNodeFallsBackToPin(t *testing.T) {
	report := &pkgClient.ProviderVersionsResponse{
		PinnedStatus: "newer",
		Nodes:        []pkgClient.NodeVersionInfo{{Node: "worker-1", Status: "same"}},
	}
	_, status := versionForNode(report, "worker-1", "1.0.0")
	assert.Equal(t, "same", status)
	_, status = versionForNode(report, "unlisted-node", "1.0.0")
	assert.Equal(t, "newer", status)
}

func flatten(lines [][2]string) string {
	var out string
	for _, l := range lines {
		out += l[0] + " " + l[1] + "\n"
	}
	return out
}

// The two installers disagree on which form they record, so a single rule
// cannot be "always show the tag". Rendered from a real screenshot bug:
// ollama showed "0.21.2 → v0.32.14", where the v reads as part of the change.
func TestLatestForDisplayMatchesInstalledShape(t *testing.T) {
	tests := []struct {
		name                            string
		installed, latestTag, latestVer string
		want                            string
	}{
		{
			// ollama records the normalized version, so the tag's v must go.
			name:      "ollama drops the v to match a bare installed version",
			installed: "0.21.2", latestTag: "v0.32.14", latestVer: "0.32.14",
			want: "0.32.14",
		},
		{
			// llama.cpp records the raw tag, so the b must stay.
			name:      "llamacpp keeps the b to match a prefixed installed version",
			installed: "b10453", latestTag: "b10502", latestVer: "10502",
			want: "b10502",
		},
		{
			name:      "no prefix at all",
			installed: "0.19.0", latestTag: "0.27.1", latestVer: "0.27.1",
			want: "0.27.1",
		},
		{
			name:      "unknown installed falls back to the normalized form",
			installed: "", latestTag: "v0.32.14", latestVer: "0.32.14",
			want: "0.32.14",
		},
		{
			name:      "missing tag",
			installed: "0.21.2", latestTag: "", latestVer: "0.32.14",
			want: "0.32.14",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, latestForDisplay(tt.installed, tt.latestTag, tt.latestVer))
		})
	}
}

// End-to-end on the column, which is what the screenshot showed.
func TestVersionCellNeverMixesPrefixShapes(t *testing.T) {
	assert.Equal(t, "0.21.2 → 0.32.14", versionCell(shared.ProviderInfo{
		Version:       "0.21.2",
		LatestVersion: latestForDisplay("0.21.2", "v0.32.14", "0.32.14"),
		VersionStatus: "newer",
	}))
	assert.Equal(t, "b10453 → b10502", versionCell(shared.ProviderInfo{
		Version:       "b10453",
		LatestVersion: latestForDisplay("b10453", "b10502", "10502"),
		VersionStatus: "newer",
	}))
}

// A node installing through a packager is capped by that packager, not by
// what the project published. The upgrade prompt is prefilled from this
// value, so taking the headline offers a release the node cannot fetch —
// the exact failure the per-node ceiling exists to stop.
func TestVersionForNodePrefersTheNodesOwnCeiling(t *testing.T) {
	report := &pkgClient.ProviderVersionsResponse{
		Latest:       "0.32.15",
		LatestTag:    "v0.32.15",
		PinnedStatus: "unknown",
		Nodes: []pkgClient.NodeVersionInfo{
			// Behind Homebrew's own ceiling: genuinely upgradable, but only
			// as far as brew goes.
			{Node: "macbook-pro", Installed: "0.30.0", Status: "newer", InstallableLatest: "0.32.14", InstallableSource: "homebrew"},
			// On the headline source: no per-node ceiling reported.
			{Node: "worker-1", Installed: "0.21.2", Status: "newer"},
		},
	}

	latest, status := versionForNode(report, "macbook-pro", "0.30.0")
	assert.Equal(t, "0.32.14", latest, "brew's ceiling, not the GitHub release")
	assert.Equal(t, "newer", status)

	latest, status = versionForNode(report, "worker-1", "0.21.2")
	assert.Equal(t, "0.32.15", latest, "no override means the headline")
	assert.Equal(t, "newer", status)

	// A node the report says nothing about falls back to the pin comparison.
	latest, status = versionForNode(report, "unknown-node", "0.1.0")
	assert.Equal(t, "0.32.15", latest)
	assert.Equal(t, "unknown", status)
}

// The prompt prefill is the thing that must not name an unreachable version.
func TestUpgradeTargetForUsesTheNodeCeiling(t *testing.T) {
	mac := shared.ProviderInfo{
		Name: "ollama", Node: "macbook-pro", Version: "0.30.0",
		LatestVersion: "0.32.14", VersionStatus: "newer",
	}
	assert.Equal(t, "0.32.14", upgradeTargetFor(mac))

	// At its ceiling: nothing to offer, even though upstream is ahead.
	atCeiling := shared.ProviderInfo{
		Name: "ollama", Node: "macbook-pro", Version: "0.32.14",
		LatestVersion: "0.32.14", VersionStatus: "same",
	}
	assert.Empty(t, upgradeTargetFor(atCeiling))
}

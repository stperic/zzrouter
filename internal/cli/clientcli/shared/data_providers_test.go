package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ParseProviders maps fields by hand, so a field present on ProviderInfo and
// on the API is still dropped until it is listed here. That is how the
// upstream columns went missing: the provider detail pane wrote them straight
// into the row, so they appeared until any list refresh wiped them.
func TestParseProvidersKeepsUpstreamVersionFields(t *testing.T) {
	resp := map[string]any{
		"data": []any{
			map[string]any{
				"name":               "ollama",
				"node":               "macbook-pro",
				"version":            "0.21.2",
				"mode":               "external",
				"latest_version":     "v0.32.14",
				"version_status":     "newer",
				"version_checked_at": "2026-08-19T20:54:42Z",
			},
			map[string]any{
				// A provider whose upstream has not been checked yet.
				"name":    "llamacpp",
				"node":    "worker-1",
				"version": "b10453",
				"mode":    "on-demand",
			},
		},
	}

	got, err := ParseProviders(resp)
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.Equal(t, "v0.32.14", got[0].LatestVersion)
	assert.Equal(t, "newer", got[0].VersionStatus)
	assert.Equal(t, "2026-08-19T20:54:42Z", got[0].VersionCheckedAt)

	// An unchecked provider stays empty rather than inheriting anything.
	assert.Empty(t, got[1].LatestVersion)
	assert.Empty(t, got[1].VersionStatus)
}

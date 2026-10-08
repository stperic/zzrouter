package views

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// The point of the feature: a self-hosted model reports real usage and says
// plainly that it has no price, rather than showing $0.00.
func TestUsageDetailLinesSelfHosted(t *testing.T) {
	lines := usageDetailLines(&pkgClient.ModelUsageResponse{
		UptimeSecs: 15060,
		Model: &pkgClient.ModelUsage{
			Model:            "qwen2.5-7b",
			Provider:         "llamacpp",
			Requests:         1284,
			TokensIn:         892441,
			TokensOut:        311208,
			AvgTokensPerSec:  43.1,
			PeakTokensPerSec: 61.7,
			Priced:           false,
		},
	})
	require.NotEmpty(t, lines)

	joined := flatten(lines)
	assert.Contains(t, joined, "1,284", "counts are separated for readability")
	assert.Contains(t, joined, "892,441")
	assert.Contains(t, joined, "43.1 tok/s")
	assert.Contains(t, joined, "not priced")
	assert.NotContains(t, joined, "$0.0000", "an unpriced model must not render a zero cost")
	assert.Contains(t, joined, "4h 11m")
}

func TestUsageDetailLinesPriced(t *testing.T) {
	lines := usageDetailLines(&pkgClient.ModelUsageResponse{
		UptimeSecs: 3600,
		Model: &pkgClient.ModelUsage{
			Model:    "claude-sonnet",
			Provider: "openrouter",
			Requests: 212,
			CostUSD:  4.8712,
			Priced:   true,
		},
	})
	joined := flatten(lines)
	assert.Contains(t, joined, "$4.8712")
	assert.NotContains(t, joined, "not priced")
}

func TestUsageDetailLinesNil(t *testing.T) {
	assert.Nil(t, usageDetailLines(nil))
	assert.Nil(t, usageDetailLines(&pkgClient.ModelUsageResponse{}))
}

func TestFormatUsageCount(t *testing.T) {
	tests := map[int64]string{
		0: "0", 7: "7", 42: "42", 999: "999",
		1000: "1,000", 1284: "1,284", 12345: "12,345",
		892441: "892,441", 1000000: "1,000,000", 1234567890: "1,234,567,890",
	}
	for in, want := range tests {
		assert.Equal(t, want, formatUsageCount(in), "input %d", in)
	}
}

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{5, "5s"},
		{59, "59s"},
		{60, "1m"},
		{3540, "59m"},
		{3600, "1h 0m"},
		{15060, "4h 11m"},
		{86400, "1d 0h"},
		{200000, "2d 7h"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, formatUptime(tt.seconds), "input %v", tt.seconds)
	}
}

// A deployment row names one engine and scopes to it; a bare registry row
// does not, and summing every provider serving the name is the honest answer.
func TestUsageProviderFor(t *testing.T) {
	assert.Equal(t, "llamacpp", usageProviderFor(&modelRow{Provider: "llamacpp"}))
	assert.Empty(t, usageProviderFor(&modelRow{}))
	assert.Empty(t, usageProviderFor(nil))
}

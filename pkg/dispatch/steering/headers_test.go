package steering

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHeaders is a HeaderGetter backed by a plain map so the parser can
// be exercised without an HTTP harness.
type stubHeaders map[string]string

func (s stubHeaders) GetHeader(name string) string { return s[name] }

func TestParse_EmptyGivesZeroHints(t *testing.T) {
	got, err := Parse(stubHeaders{})
	require.Nil(t, err)
	assert.False(t, got.HasAny())
	assert.Empty(t, got.Applied())
}

func TestParse_ExcludeReplicas(t *testing.T) {
	got, err := Parse(stubHeaders{HeaderExcludeReplicas: "r1, r2 ,r3"})
	require.Nil(t, err)
	assert.Equal(t, []string{"r1", "r2", "r3"}, got.ExcludeReplicas)
	assert.Equal(t, []string{string(ParamExcludeReplicas)}, got.Applied())
}

func TestParse_LatencyBudgetMs(t *testing.T) {
	got, err := Parse(stubHeaders{HeaderLatencyBudgetMs: " 2000 "})
	require.Nil(t, err)
	assert.Equal(t, 2000, got.LatencyBudgetMs)
	assert.Equal(t, []string{string(ParamLatencyBudgetMs)}, got.Applied())
}

func TestParse_LatencyBudgetMs_RejectsZero(t *testing.T) {
	_, err := Parse(stubHeaders{HeaderLatencyBudgetMs: "0"})
	require.NotNil(t, err)
	assert.Equal(t, HeaderLatencyBudgetMs, err.Header)
}

func TestParse_LatencyBudgetMs_RejectsNonInteger(t *testing.T) {
	_, err := Parse(stubHeaders{HeaderLatencyBudgetMs: "2s"})
	require.NotNil(t, err)
	assert.Equal(t, HeaderLatencyBudgetMs, err.Header)
}

func TestParse_LatencyBudgetMs_RejectsHugeValues(t *testing.T) {
	_, err := Parse(stubHeaders{HeaderLatencyBudgetMs: "99999999"})
	require.NotNil(t, err)
}

func TestParse_RequireTags(t *testing.T) {
	got, err := Parse(stubHeaders{HeaderRequireTags: "gpu, fast"})
	require.Nil(t, err)
	assert.Equal(t, []string{"gpu", "fast"}, got.RequireTags)
}

func TestParse_UnsupportedHeadersAreRejected(t *testing.T) {
	for _, h := range []string{
		"X-Route-Require-Capabilities",
		"X-Route-Max-Cost-Micro",
		"X-Route-Prefer-Tags",
		"X-Route-Prefer-Model",
	} {
		_, err := Parse(stubHeaders{h: "anything"})
		require.NotNil(t, err, "header %s must be rejected", h)
		assert.True(t, errors.Is(err, ErrUnsupportedHeader),
			"%s must wrap ErrUnsupportedHeader", h)
		assert.Equal(t, h, err.Header)
	}
}

func TestParse_ExcludeReplicas_TooMany(t *testing.T) {
	names := make([]string, maxExcludeReplicas+1)
	for i := range names {
		names[i] = "r" + strings.Repeat("0", 1)
	}
	_, err := Parse(stubHeaders{HeaderExcludeReplicas: strings.Join(names, ",")})
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "max")
}

func TestParse_ExcludeReplicas_RejectsControlChars(t *testing.T) {
	_, err := Parse(stubHeaders{HeaderExcludeReplicas: "ok,bad\x00name"})
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "control character")
}

func TestParse_AllThreeTogether(t *testing.T) {
	got, err := Parse(stubHeaders{
		HeaderExcludeReplicas: "r1",
		HeaderLatencyBudgetMs: "500",
		HeaderRequireTags:     "gpu",
	})
	require.Nil(t, err)
	applied := got.Applied()
	assert.ElementsMatch(t, []string{"exclude_replicas", "latency_budget_ms", "require_tags"}, applied)
}

func TestHints_ZeroValueIsNoop(t *testing.T) {
	var h Hints
	assert.False(t, h.HasAny())
	assert.Empty(t, h.Applied())
}

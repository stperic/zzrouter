package fallback

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterDeployments_CooldownSkip(t *testing.T) {
	cooldowns := NewCooldownManager()
	defer cooldowns.Stop()
	cooldowns.SetCooldown("dep-b", DefaultCooldownDuration, ReasonRateLimit)

	candidates := []Candidate{
		{Name: "dep-a"},
		{Name: "dep-b"},
		{Name: "dep-c"},
	}

	result, skipped := FilterDeployments(candidates, cooldowns, nil, nil, nil, nil, SteeringHints{})
	require.Len(t, result, 2)
	assert.Equal(t, "dep-a", result[0].Name)
	assert.Equal(t, "dep-c", result[1].Name)
	require.Len(t, skipped, 1)
	assert.Equal(t, "dep-b", skipped[0].Candidate.Name)
	assert.Equal(t, SkipReasonCooldown, skipped[0].Reason)
}

func TestFilterDeployments_TagFiltering(t *testing.T) {
	candidates := []Candidate{
		{Name: "cloud-fast", Tags: []string{"cloud", "fast"}},
		{Name: "local-gpu", Tags: []string{"local", "high-vram"}},
		{Name: "cloud-cheap", Tags: []string{"cloud"}},
	}

	// Request tags: must be cloud + fast
	result, skipped := FilterDeployments(candidates, nil, nil, nil, []string{"cloud", "fast"}, nil, SteeringHints{})
	require.Len(t, result, 1)
	assert.Equal(t, "cloud-fast", result[0].Name)
	require.Len(t, skipped, 2)
	for _, s := range skipped {
		assert.Equal(t, SkipReasonTagMismatch, s.Reason)
	}
}

func TestFilterDeployments_TagFilteringAllMatch(t *testing.T) {
	candidates := []Candidate{
		{Name: "a", Tags: []string{"gpu", "fast"}},
		{Name: "b", Tags: []string{"gpu", "fast", "extra"}},
	}

	result, skipped := FilterDeployments(candidates, nil, nil, nil, []string{"gpu"}, nil, SteeringHints{})
	require.Len(t, result, 2)
	assert.Empty(t, skipped)
}

func TestFilterDeployments_NoTags_NoFiltering(t *testing.T) {
	candidates := []Candidate{
		{Name: "a", Tags: []string{"gpu"}},
		{Name: "b"},
	}

	result, skipped := FilterDeployments(candidates, nil, nil, nil, nil, nil, SteeringHints{})
	require.Len(t, result, 2)
	assert.Empty(t, skipped)
}

func TestFilterDeployments_EmptyDeploymentTags(t *testing.T) {
	candidates := []Candidate{
		{Name: "a"},
		{Name: "b", Tags: []string{"gpu"}},
	}

	// Request needs "gpu" — "a" has no tags so it's filtered out
	result, skipped := FilterDeployments(candidates, nil, nil, nil, []string{"gpu"}, nil, SteeringHints{})
	require.Len(t, result, 1)
	assert.Equal(t, "b", result[0].Name)
	require.Len(t, skipped, 1)
	assert.Equal(t, "a", skipped[0].Candidate.Name)
	assert.Equal(t, SkipReasonTagMismatch, skipped[0].Reason)
}

func TestFilterDeployments_NilCooldownManager(t *testing.T) {
	candidates := []Candidate{{Name: "a"}, {Name: "b"}}
	result, skipped := FilterDeployments(candidates, nil, nil, nil, nil, nil, SteeringHints{})
	require.Len(t, result, 2)
	assert.Empty(t, skipped)
}

func TestFilterDeployments_ProviderCooldownSkip(t *testing.T) {
	providerCooldowns := NewCooldownManager()
	defer providerCooldowns.Stop()
	providerCooldowns.SetCooldown("groq", QuotaExhaustedDuration, ReasonQuota)

	candidates := []Candidate{
		{Name: "groq-primary", App: "groq"},
		{Name: "groq-backup", App: "groq"},
		{Name: "ollama-local", App: "ollama"},
	}

	result, skipped := FilterDeployments(candidates, nil, providerCooldowns, nil, nil, nil, SteeringHints{})
	require.Len(t, result, 1)
	assert.Equal(t, "ollama-local", result[0].Name)
	require.Len(t, skipped, 2)
	for _, s := range skipped {
		assert.Equal(t, SkipReasonProviderCooldown, s.Reason)
	}
}

func TestMatchesTags(t *testing.T) {
	assert.True(t, matchesTags([]string{"a", "b", "c"}, []string{"a", "b"}))
	assert.True(t, matchesTags([]string{"a", "b"}, []string{"a", "b"}))
	assert.False(t, matchesTags([]string{"a"}, []string{"a", "b"}))
	assert.True(t, matchesTags([]string{"a"}, nil))
	assert.True(t, matchesTags(nil, nil))
	assert.False(t, matchesTags(nil, []string{"a"}))
}

// stubLatency implements LatencySnapshotter for filter tests.
type stubLatency map[string]struct {
	avg     time.Duration
	samples int
}

func (s stubLatency) Snapshot(name string) (time.Duration, int) {
	v := s[name]
	return v.avg, v.samples
}

func TestFilterDeployments_ExcludeReplicasHint(t *testing.T) {
	candidates := []Candidate{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	result, skipped := FilterDeployments(candidates, nil, nil, nil, nil, nil, SteeringHints{
		ExcludeReplicas: []string{"b"},
	})
	require.Len(t, result, 2)
	assert.Equal(t, "a", result[0].Name)
	assert.Equal(t, "c", result[1].Name)
	require.Len(t, skipped, 1)
	assert.Equal(t, "b", skipped[0].Candidate.Name)
	assert.Equal(t, SkipReasonExcluded, skipped[0].Reason)
}

func TestFilterDeployments_LatencyBudgetHint_SkipsExceedingReplicas(t *testing.T) {
	candidates := []Candidate{{Name: "fast"}, {Name: "slow"}}
	latency := stubLatency{
		"fast": {avg: 50 * time.Millisecond, samples: 5},
		"slow": {avg: 800 * time.Millisecond, samples: 5},
	}
	result, skipped := FilterDeployments(candidates, nil, nil, nil, nil, latency, SteeringHints{
		LatencyBudgetMs: 500,
	})
	require.Len(t, result, 1)
	assert.Equal(t, "fast", result[0].Name)
	require.Len(t, skipped, 1)
	assert.Equal(t, SkipReasonLatencyBudget, skipped[0].Reason)
}

func TestFilterDeployments_LatencyBudgetHint_PassesUnsampledReplicas(t *testing.T) {
	// Fail-open: a replica with no samples passes the budget filter.
	// Skipping it would create a permanent dead state where untried
	// replicas can never accumulate evidence of meeting the budget.
	candidates := []Candidate{{Name: "untried"}}
	latency := stubLatency{} // no data
	result, skipped := FilterDeployments(candidates, nil, nil, nil, nil, latency, SteeringHints{
		LatencyBudgetMs: 100,
	})
	assert.Len(t, result, 1)
	assert.Empty(t, skipped)
}

func TestFilterDeployments_HintsComposeWithEngineFilters(t *testing.T) {
	cooldowns := NewCooldownManager()
	defer cooldowns.Stop()
	cooldowns.SetCooldown("a", DefaultCooldownDuration, ReasonRateLimit)

	candidates := []Candidate{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	result, skipped := FilterDeployments(candidates, cooldowns, nil, nil, nil, nil, SteeringHints{
		ExcludeReplicas: []string{"b"},
	})
	require.Len(t, result, 1)
	assert.Equal(t, "c", result[0].Name)
	// a → cooldown (engine-side filter), b → excluded (steering)
	require.Len(t, skipped, 2)
	byName := map[string]SkipReason{}
	for _, s := range skipped {
		byName[s.Candidate.Name] = s.Reason
	}
	assert.Equal(t, SkipReasonCooldown, byName["a"])
	assert.Equal(t, SkipReasonExcluded, byName["b"])
}

func TestFilterDeployments_ZeroHintsIsNoop(t *testing.T) {
	candidates := []Candidate{{Name: "a"}, {Name: "b"}}
	result, skipped := FilterDeployments(candidates, nil, nil, nil, nil, nil, SteeringHints{})
	assert.Len(t, result, 2)
	assert.Empty(t, skipped)
}

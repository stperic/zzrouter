package inferencelog

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func entryAt(model, provider, node string, in, out int64, cost float64, source string) LogEntry {
	return LogEntry{
		Model:        model,
		App:          provider,
		Node:         node,
		Status:       "success",
		TokensIn:     in,
		TokensOut:    out,
		LatencyMs:    1000,
		TokensPerSec: float64(out),
		Cost:         cost,
		CostSource:   source,
		Timestamp:    time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	}
}

// The whole reason this type exists: the entry ring evicts, so a total
// derived from it would under-report. Totals must survive eviction.
func TestTotalsSurviveRingEviction(t *testing.T) {
	const capacity = 10
	store := NewStore(capacity, 0)

	const requests = 250
	for i := range requests {
		e := entryAt("qwen2.5-7b", "llamacpp", "coord", 100, 50, 0, "")
		e.ID = fmt.Sprintf("req-%d", i)
		store.Add(e, nil)
	}

	// The ring holds only the last few entries...
	assert.Equal(t, capacity, store.Len())

	// ...but the totals still account for every request.
	usage, ok := store.Totals().ForModel("llamacpp", "qwen2.5-7b")
	require.True(t, ok)
	assert.Equal(t, int64(requests), usage.Requests, "totals must not be limited by ring capacity")
	assert.Equal(t, int64(requests*100), usage.TokensIn)
	assert.Equal(t, int64(requests*50), usage.TokensOut)
}

// A self-hosted model has no pricing, and reporting it as $0.00 reads as
// broken rather than as free. Priced is what lets a caller tell them apart.
func TestTotalsDistinguishUnpricedFromFree(t *testing.T) {
	totals := NewTotals()

	totals.Record(entryAt("qwen2.5-7b", "llamacpp", "coord", 100, 50, 0, ""))
	local, ok := totals.ForModel("llamacpp", "qwen2.5-7b")
	require.True(t, ok)
	assert.False(t, local.Priced, "an unpriced local model must not claim a cost")
	assert.Zero(t, local.CostUSD)
	assert.Equal(t, int64(1), local.Requests, "usage is still tracked without pricing")

	totals.Record(entryAt("claude-sonnet", "openrouter", "coord", 1000, 500, 0.0125, "provider"))
	cloud, ok := totals.ForModel("openrouter", "claude-sonnet")
	require.True(t, ok)
	assert.True(t, cloud.Priced)
	assert.InDelta(t, 0.0125, cloud.CostUSD, 1e-9)
}

// A zero cost carrying a real source is a genuine zero and must be counted as
// priced, unlike a zero with no source at all.
func TestTotalsCountZeroCostWithSourceAsPriced(t *testing.T) {
	totals := NewTotals()
	totals.Record(entryAt("tiny-embed", "openai", "coord", 4, 0, 0, "zzrouter"))

	usage, ok := totals.ForModel("openai", "tiny-embed")
	require.True(t, ok)
	assert.True(t, usage.Priced, "a sub-microdollar cost is still a real cost")
	assert.Zero(t, usage.CostUSD)
}

func TestTotalsAccumulate(t *testing.T) {
	totals := NewTotals()
	for range 3 {
		totals.Record(entryAt("claude-sonnet", "openrouter", "coord", 1000, 500, 0.01, "provider"))
	}

	usage, ok := totals.ForModel("openrouter", "claude-sonnet")
	require.True(t, ok)
	assert.Equal(t, int64(3), usage.Requests)
	assert.Equal(t, int64(3000), usage.TokensIn)
	assert.Equal(t, int64(1500), usage.TokensOut)
	assert.InDelta(t, 0.03, usage.CostUSD, 1e-9)
	// 1500 output tokens over 3s of generation.
	assert.InDelta(t, 500, usage.AvgTokensPerSec, 0.001)
	assert.InDelta(t, 1000, usage.AvgLatencyMs, 0.001)
	assert.Equal(t, []string{"coord"}, usage.Nodes)
}

// The same model name under two providers has different economics, so the
// buckets stay separate, and a provider-less query sums them.
func TestTotalsSeparateProvidersButMergeOnRequest(t *testing.T) {
	totals := NewTotals()
	totals.Record(entryAt("qwen2.5-7b", "llamacpp", "coord", 100, 50, 0, ""))
	totals.Record(entryAt("qwen2.5-7b", "vllm", "worker-1", 200, 80, 0, ""))

	local, ok := totals.ForModel("llamacpp", "qwen2.5-7b")
	require.True(t, ok)
	assert.Equal(t, int64(1), local.Requests)
	assert.Equal(t, int64(100), local.TokensIn)

	merged, ok := totals.ForModel("", "qwen2.5-7b")
	require.True(t, ok)
	assert.Equal(t, int64(2), merged.Requests)
	assert.Equal(t, int64(300), merged.TokensIn)
	assert.Equal(t, int64(130), merged.TokensOut)
	assert.Equal(t, "llamacpp,vllm", merged.Provider)
	assert.Equal(t, []string{"coord", "worker-1"}, merged.Nodes)
}

// Merging must not let an unpriced deployment silently dilute a priced one.
func TestTotalsMergeKeepsPricedCostIntact(t *testing.T) {
	totals := NewTotals()
	totals.Record(entryAt("shared", "openrouter", "a", 10, 10, 0.5, "provider"))
	totals.Record(entryAt("shared", "llamacpp", "b", 10, 10, 0, ""))

	merged, ok := totals.ForModel("", "shared")
	require.True(t, ok)
	assert.True(t, merged.Priced)
	assert.InDelta(t, 0.5, merged.CostUSD, 1e-9, "only priced parts contribute cost")
	assert.Equal(t, int64(2), merged.Requests, "usage still counts every request")
}

func TestTotalsCountErrors(t *testing.T) {
	totals := NewTotals()
	ok1 := entryAt("m", "p", "n", 1, 1, 0, "")
	bad := entryAt("m", "p", "n", 1, 0, 0, "")
	bad.Status = "error"
	totals.Record(ok1)
	totals.Record(bad)

	usage, found := totals.ForModel("p", "m")
	require.True(t, found)
	assert.Equal(t, int64(2), usage.Requests)
	assert.Equal(t, int64(1), usage.Errors)
}

func TestTotalsIgnoreEntriesWithoutModel(t *testing.T) {
	totals := NewTotals()
	totals.Record(entryAt("", "llamacpp", "coord", 10, 10, 0, ""))
	assert.Empty(t, totals.All(), "an unattributable entry must not create a bucket")
}

func TestTotalsAllSortedByRequests(t *testing.T) {
	totals := NewTotals()
	totals.Record(entryAt("quiet", "p", "n", 1, 1, 0, ""))
	for range 5 {
		totals.Record(entryAt("busy", "p", "n", 1, 1, 0, ""))
	}

	all := totals.All()
	require.Len(t, all, 2)
	assert.Equal(t, "busy", all[0].Model, "busiest first")
	assert.Equal(t, "quiet", all[1].Model)
}

func TestTotalsForUnknownModel(t *testing.T) {
	totals := NewTotals()
	_, ok := totals.ForModel("p", "nope")
	assert.False(t, ok)
	_, ok = totals.ForModel("", "nope")
	assert.False(t, ok)
}

func TestTotalsSinceIsSetAtCreation(t *testing.T) {
	before := time.Now()
	totals := NewTotals()
	assert.False(t, totals.Since().Before(before.Add(-time.Second)))
}

package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCache_LookupByName_HitMiss uses updateModelCache (package-private)
// to prepopulate state without needing a working refresh pipeline —
// the point is to verify the index semantics, not the population path.
func TestCache_LookupByName_HitMiss(t *testing.T) {
	c := newWorkerCache(t)
	c.updateModelCache(samplesFor())

	got, ok := c.LookupByName("llama3")
	require.True(t, ok)
	assert.Equal(t, "ollama", got.Provider)
	assert.Equal(t, "gpu-1", got.Node)

	_, ok = c.LookupByName("nonexistent")
	assert.False(t, ok)
}

// TestCache_HasModel verifies the non-blocking probe API used by
// fast-path routing (distinguishes "in index right now" from "maybe
// after a refresh").
func TestCache_HasModel(t *testing.T) {
	c := newWorkerCache(t)
	assert.False(t, c.HasModel("llama3"), "empty cache: miss")

	c.updateModelCache(samplesFor())
	assert.True(t, c.HasModel("llama3"))
	assert.False(t, c.HasModel("nope"))
}

// TestCache_HasModelExact_NoSuffixFallback pins the auto_deploy gate's
// exact-only contract. LookupModel falls back to indexBySuffix on
// exact miss, which would let a deploy gate skip when an unrelated
// suffix-matching entry exists. HasModelExact MUST hit indexByName
// only — the gate is asking "is THIS canonical model in the pool?".
func TestCache_HasModelExact_NoSuffixFallback(t *testing.T) {
	ctx := context.Background()
	c := newWorkerCache(t)
	c.updateModelCache(samplesFor())

	// Canonical names hit.
	assert.True(t, c.HasModelExact(ctx, "llama3"), "exact canonical name hits")
	assert.True(t, c.HasModelExact(ctx, "qwen/qwen2.5-7b"), "exact slash-prefixed name hits")

	// Suffix-only forms MUST NOT match HasModelExact even though
	// LookupModel would resolve them via indexBySuffix.
	if _, err := c.LookupModel(ctx, "qwen2.5-7b"); err != nil {
		t.Fatalf("precondition: LookupModel resolves suffix-only name; got err=%v", err)
	}
	assert.False(t, c.HasModelExact(ctx, "qwen2.5-7b"),
		"suffix-only name MUST NOT match HasModelExact")

	// Sanity: nonexistent name miss.
	assert.False(t, c.HasModelExact(ctx, "no-such-model"))
}

// TestCache_SourceIDAlias pins the /v1/* alias contract: a model
// scanned from a HuggingFace repo dir (filesystem source) carries
// both the file-stem Name and the repo-path SourceID. Both must
// resolve via LookupModel + HasModelExact so callers can address
// the model by either alias. Live regression: deploying
// Qwen/Qwen2.5-0.5B-Instruct-GGUF#Q4_K_M used to surface only the
// file-stem alias in /v1/chat — clients that POST'd the repo name
// got model_not_found.
func TestCache_SourceIDAlias(t *testing.T) {
	ctx := context.Background()
	c := newWorkerCache(t)

	c.updateModelCache([]*CachedModel{
		{
			Name:       "qwen2.5-0.5b-instruct-q4_k_m",
			SourceID:   "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
			Provider:   "llamacpp",
			Node:       "worker-1",
			SourceRepo: "huggingface",
		},
	})

	// Both aliases hit the exact index.
	assert.True(t, c.HasModelExact(ctx, "qwen2.5-0.5b-instruct-q4_k_m"),
		"file-stem name hits exact index")
	assert.True(t, c.HasModelExact(ctx, "Qwen/Qwen2.5-0.5B-Instruct-GGUF"),
		"SourceID alias hits exact index — closes /v1/chat 'launch-name model_not_found' gap")

	// LookupModel resolves both to the same record.
	byName, err := c.LookupModel(ctx, "qwen2.5-0.5b-instruct-q4_k_m")
	if err != nil {
		t.Fatalf("LookupModel by Name: %v", err)
	}
	bySource, err := c.LookupModel(ctx, "Qwen/Qwen2.5-0.5B-Instruct-GGUF")
	if err != nil {
		t.Fatalf("LookupModel by SourceID: %v", err)
	}
	assert.Same(t, byName, bySource, "both aliases must resolve to the same CachedModel")
}

// TestCache_Invalidate_ClearsState verifies THE central invalidation
// contract: after Invalidate, indexes are empty and valid==false so
// the next read triggers a refresh.
func TestCache_Invalidate_ClearsState(t *testing.T) {
	c := newWorkerCache(t)
	c.updateModelCache(samplesFor())

	require.True(t, c.HasModel("llama3"), "precondition: populated")
	stats := c.GetCacheStats()
	require.Equal(t, 3, stats["size"])
	require.Equal(t, true, stats["valid"])

	c.Invalidate()

	assert.False(t, c.HasModel("llama3"), "post-Invalidate: index empty")
	stats = c.GetCacheStats()
	assert.Equal(t, 0, stats["size"])
	assert.Equal(t, 0, stats["indexed_by_name"])
	assert.Equal(t, 0, stats["suffix_entries"])
	assert.Equal(t, false, stats["valid"])
}

// TestCache_ReloadConfig_Invalidates verifies the configReloader
// adapter: apps-config mutations must invalidate the cache because
// provider-assignment outcomes may shift.
func TestCache_ReloadConfig_Invalidates(t *testing.T) {
	c := newWorkerCache(t)
	c.updateModelCache(samplesFor())
	require.True(t, c.HasModel("llama3"))

	c.ReloadConfig(nil) // cfg arg unused per its godoc

	assert.False(t, c.HasModel("llama3"), "ReloadConfig must cascade to Invalidate")
}

// TestCache_GetCacheStats_Fields documents the observability contract
// — tools and dashboards rely on these keys.
func TestCache_GetCacheStats_Fields(t *testing.T) {
	c := newWorkerCache(t)

	// Pre-population stats.
	stats := c.GetCacheStats()
	for _, key := range []string{"size", "indexed_by_name", "suffix_entries", "last_updated", "age_seconds", "valid"} {
		_, ok := stats[key]
		assert.Truef(t, ok, "stats must include key %q", key)
	}
	assert.Equal(t, 0, stats["size"])
	assert.Equal(t, false, stats["valid"])

	c.updateModelCache(samplesFor())
	stats = c.GetCacheStats()
	assert.Equal(t, 3, stats["size"])
	assert.Equal(t, true, stats["valid"])

	// age_seconds must be non-zero (and small) after a populate.
	age, ok := stats["age_seconds"].(float64)
	require.True(t, ok, "age_seconds is float64 seconds")
	assert.GreaterOrEqual(t, age, 0.0)
	assert.Less(t, age, 5.0, "age should be near-zero for a just-populated cache")

	lastUpdated, ok := stats["last_updated"].(time.Time)
	require.True(t, ok)
	assert.False(t, lastUpdated.IsZero())
}

// TestCache_GetAllModels_NilWhenInvalid documents the "nil = not
// populated" contract consumed by autoroute.Manager — a nil slice
// tells the caller "no reconciliation yet."
func TestCache_GetAllModels_NilWhenInvalid(t *testing.T) {
	c := newWorkerCache(t)
	assert.Nil(t, c.GetAllModels(), "pre-populate: nil")

	c.updateModelCache(samplesFor())
	assert.Len(t, c.GetAllModels(), 3, "post-populate: slice")

	c.Invalidate()
	assert.Nil(t, c.GetAllModels(), "post-Invalidate: nil again")
}

// TestCache_SuffixIndex verifies that qualified model names (with
// slashes or colons) are also indexed by their short form — the
// coordinator-path suffix fallback that LookupModel uses.
func TestCache_SuffixIndex(t *testing.T) {
	c := newWorkerCache(t)
	c.updateModelCache(samplesFor())

	// "qwen/qwen2.5-7b" should be findable by suffix "qwen2.5-7b".
	// The exact suffix depends on FormatModelName; we just assert the
	// index is populated when the short form differs from the full name.
	stats := c.GetCacheStats()
	suffixEntries, ok := stats["suffix_entries"].(int)
	assert.True(t, ok)
	assert.Greater(t, suffixEntries, 0, "qualified names produce suffix entries")
}

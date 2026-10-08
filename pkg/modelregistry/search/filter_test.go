package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseSortDirection(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"desc", -1},
		{"DESC", -1},
		{"descending", -1},
		{"down", -1},
		{"asc", 1},
		{"ASC", 1},
		{"ascending", 1},
		{"up", 1},
		{"", -1},        // default
		{"garbage", -1}, // default
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ParseSortDirection(tt.in), "ParseSortDirection(%q)", tt.in)
	}
}

func TestFilterModelsByTags_Empty(t *testing.T) {
	models := []ModelInfo{{ID: "a", Tags: []string{"x"}}}
	got := FilterModelsByTags(models, nil, "AND")
	assert.Equal(t, models, got, "empty filter must return input unchanged")
}

func TestFilterModelsByTags_AndLogic(t *testing.T) {
	models := []ModelInfo{
		{ID: "has-both", Tags: []string{"text", "pytorch"}},
		{ID: "text-only", Tags: []string{"text"}},
		{ID: "neither", Tags: []string{"unrelated"}},
	}
	got := FilterModelsByTags(models, []string{"text", "pytorch"}, "AND")
	assert.Len(t, got, 1)
	assert.Equal(t, "has-both", got[0].ID, "AND requires all filter tags present")
}

func TestFilterModelsByTags_OrLogic(t *testing.T) {
	models := []ModelInfo{
		{ID: "has-both", Tags: []string{"text", "pytorch"}},
		{ID: "text-only", Tags: []string{"text"}},
		{ID: "pytorch-only", Tags: []string{"pytorch"}},
		{ID: "neither", Tags: []string{"unrelated"}},
	}
	got := FilterModelsByTags(models, []string{"text", "pytorch"}, "OR")
	assert.Len(t, got, 3, "OR matches any of the filter tags")
}

func TestFilterModelsByTags_CaseInsensitive(t *testing.T) {
	models := []ModelInfo{{ID: "mixed", Tags: []string{"TEXT", "PyTorch"}}}
	got := FilterModelsByTags(models, []string{"text", "pytorch"}, "AND")
	assert.Len(t, got, 1, "tag matching is case-insensitive on both sides")
}

func TestFilterModelsByTags_UnknownLogicDefaultsToAnd(t *testing.T) {
	models := []ModelInfo{
		{ID: "has-both", Tags: []string{"text", "pytorch"}},
		{ID: "text-only", Tags: []string{"text"}},
	}
	got := FilterModelsByTags(models, []string{"text", "pytorch"}, "XYZZY")
	assert.Len(t, got, 1, "unknown logic must default to AND")
	assert.Equal(t, "has-both", got[0].ID)
}

func TestMergeMetadataWithPriority_TargetWins(t *testing.T) {
	target := map[string]any{"context_length": 8192}
	source := map[string]any{"context_length": 4096, "mode": "chat"}
	MergeMetadataWithPriority(target, source)
	assert.Equal(t, 8192, target["context_length"], "target value must win")
	assert.Equal(t, "chat", target["mode"], "source fills gaps")
}

func TestMergeMetadataWithPriority_EquivalenceSuppression(t *testing.T) {
	// Target has "context_window"; source has "context_length". Equivalent
	// — source must not override since the provider already supplied the
	// same semantic value under a different name.
	target := map[string]any{"context_window": 32768}
	source := map[string]any{"context_length": 4096}
	MergeMetadataWithPriority(target, source)
	_, hasContextLength := target["context_length"]
	assert.False(t, hasContextLength, "equivalent key must suppress source")
	assert.Equal(t, 32768, target["context_window"])
}

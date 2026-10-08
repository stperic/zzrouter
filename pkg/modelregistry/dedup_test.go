package modelregistry

import (
	"testing"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/require"
)

// TestDeduplicateByFullPath_PreservesMultipleOllamaModels pins a
// regression spotted during cluster live-verify: when the worker's
// Ollama daemon reports 2 models, only 1 surfaced in coord's
// /v1/models. If the dedup pass collapses two distinct Ollama entries,
// this test will catch it.
//
// Both Ollama entries have FullPath="" (the daemon owns the files);
// the dedup pathKey falls back to Name. As long as the names differ,
// both must survive.
func TestDeduplicateByFullPath_PreservesMultipleOllamaModels(t *testing.T) {
	models := []*metadata.ModelMetadata{
		{Name: "qwen2.5:0.5b", FullPath: "", SourceRepo: metadata.SourceOllama},
		{Name: "smollm:135m", FullPath: "", SourceRepo: metadata.SourceOllama},
		{Name: "Qwen/Qwen2.5-0.5B", FullPath: "/var/lib/zzrouter/models/Qwen-Qwen2.5-0.5B", SourceRepo: metadata.SourceHuggingFace},
		{Name: "qwen2.5-0.5b-instruct-q4_k_m", FullPath: "/var/lib/zzrouter/models/qwen2.5-0.5b-instruct-q4_k_m.gguf"},
		{Name: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", FullPath: "/var/lib/zzrouter/models/Qwen-Qwen2.5-0.5B-Instruct-GGUF.gguf"},
	}
	got := deduplicateByFullPath(models)
	require.Len(t, got, 5, "all 5 distinct models must survive dedup; got=%d", len(got))
}

// TestDeduplicateByFullPath_DropsTrueDuplicates pins the intended
// behavior: same FullPath collapses to one entry.
func TestDeduplicateByFullPath_DropsTrueDuplicates(t *testing.T) {
	models := []*metadata.ModelMetadata{
		{Name: "a", FullPath: "/x/a.gguf"},
		{Name: "a", FullPath: "/x/a.gguf"}, // identical, must drop
		{Name: "b", FullPath: "/x/b.gguf"},
	}
	got := deduplicateByFullPath(models)
	require.Len(t, got, 2)
}

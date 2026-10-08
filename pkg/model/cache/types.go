package cache

import "time"

// CachedModel represents a model in the unified cache.
// Used for both routing (fast lookup) and listing (full metadata).
//
// Read-only post-return. Cache.LookupModel / Cache.ListModels /
// Cache.GetAllModels return pointers to internal state for hot-path
// efficiency (no copy); callers must not mutate the returned values
// or the slice they sit in. The cache replaces its underlying slice
// atomically on every Invalidate → RefreshCacheSync cycle, so
// references held by readers remain safe to read after an
// invalidation but will reflect the pre-invalidation snapshot.
type CachedModel struct {
	Name       string         `json:"name"`
	Model      string         `json:"model"`                 // Duplicate for Ollama compatibility
	SourceID   string         `json:"source_id,omitempty"`   // Ollama model name for filtering
	SourceRepo string         `json:"source_repo,omitempty"` // ollama, huggingface, etc.
	Node       string         `json:"node"`
	IsCloud    bool           `json:"is_cloud,omitempty"`
	Provider   string         `json:"assigned_app"`
	Format     string         `json:"format"`
	Size       int64          `json:"size"`
	Modified   time.Time      `json:"modified_at"`
	Digest     string         `json:"digest,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	// VariantOf is the base a variant runs: the entry is a provider
	// config's variant, listed wherever its base's weights are.
	VariantOf string         `json:"variant_of,omitempty"`
	Extra     map[string]any `json:"-"` // Additional fields
}

package search

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// CloudProviderMeta holds display metadata for a cloud search provider.
// This is the single source of truth — server API responses and TUI defaults
// are derived from these fields so adding a provider doesn't require touching
// multiple switch statements.
type CloudProviderMeta struct {
	DisplayName  string   // Human-readable name (e.g. "OpenRouter")
	Description  string   // Short description for API responses
	BaseURL      string   // Provider website (e.g. "https://openrouter.ai")
	ModelURLFmt  string   // fmt template for model page URL; %s = model ID
	SortOptions  []string // Sort keys exposed to client
	ClientSorts  []string // Sorts handled client-side (no re-fetch)
	ClientFilter bool     // true = client-side name filtering
	HasTags      bool     // true = supports tag-based filtering
	// Provider icons are managed centrally in pkg/ui/emoji.go via GetProviderEmoji.
}

// CloudSearchFunc is the function signature for searching a cloud provider's models.
// It returns results as []any so the caller can serialize them directly into JSON.
// Each element should be a struct or map with JSON tags.
type CloudSearchFunc func(ctx context.Context, query string, limit int) ([]any, error)

// cloudProviderEntry bundles metadata and the search function for a registered provider.
type cloudProviderEntry struct {
	Meta       CloudProviderMeta
	SearchFunc CloudSearchFunc
}

var (
	cloudRegistry   = make(map[Provider]cloudProviderEntry)
	cloudRegistryMu sync.RWMutex
)

// RegisterCloudProvider registers a cloud search provider with its metadata
// and search function. Call this from an init() in each provider's file.
// Also registers the provider's emoji in the UI package.
func RegisterCloudProvider(name Provider, meta CloudProviderMeta, fn CloudSearchFunc) {
	cloudRegistryMu.Lock()
	defer cloudRegistryMu.Unlock()
	cloudRegistry[name] = cloudProviderEntry{Meta: meta, SearchFunc: fn}
}

// GetCloudProvider returns the entry for a registered cloud provider.
func GetCloudProvider(name Provider) (cloudProviderEntry, bool) {
	cloudRegistryMu.RLock()
	defer cloudRegistryMu.RUnlock()
	entry, ok := cloudRegistry[name]
	return entry, ok
}

// CloudProviders returns all registered cloud provider names in stable sorted order.
func CloudProviders() []Provider {
	cloudRegistryMu.RLock()
	defer cloudRegistryMu.RUnlock()
	providers := make([]Provider, 0, len(cloudRegistry))
	for name := range cloudRegistry {
		providers = append(providers, name)
	}
	slices.Sort(providers)
	return providers
}

// SearchCloudProvider searches a registered cloud provider by name.
func SearchCloudProvider(ctx context.Context, name Provider, query string, limit int) ([]any, error) {
	entry, ok := GetCloudProvider(name)
	if !ok {
		return nil, fmt.Errorf("unknown cloud search provider: %s", name)
	}
	return entry.SearchFunc(ctx, query, limit)
}

// IsCloudSearchProvider returns true if the given provider is a registered cloud search provider.
func IsCloudSearchProvider(name Provider) bool {
	cloudRegistryMu.RLock()
	defer cloudRegistryMu.RUnlock()
	_, ok := cloudRegistry[name]
	return ok
}

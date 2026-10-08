package search

import (
	"fmt"
	"sync"
)

// modelCardCache is a bounded in-memory cache of fetched ModelCards.
// Keyed by "<provider>:<modelID>". Full eviction on overflow bounds memory
// without per-entry bookkeeping — the cache carries non-authoritative data
// (descriptions, tags, README excerpts), so a cold-fetch after eviction is
// acceptable.
var (
	modelCardCache = make(map[string]*ModelCard)
	cacheMutex     sync.RWMutex
)

const modelCardCacheMaxSize = 500

func cacheKey(provider Provider, modelID string) string {
	return fmt.Sprintf("%s:%s", provider.String(), modelID)
}

func getCachedModelCard(provider Provider, modelID string) (*ModelCard, bool) {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	card, exists := modelCardCache[cacheKey(provider, modelID)]
	return card, exists
}

// setCachedModelCard stores a card. When the cache reaches
// modelCardCacheMaxSize, everything is evicted before inserting so memory
// stays bounded.
func setCachedModelCard(provider Provider, modelID string, card *ModelCard) {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()

	if len(modelCardCache) >= modelCardCacheMaxSize {
		modelCardCache = make(map[string]*ModelCard)
	}
	modelCardCache[cacheKey(provider, modelID)] = card
}

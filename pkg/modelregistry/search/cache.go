package search

import (
	"net/url"
	sortpkg "sort"
	"strconv"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// DefaultSearchCacheTTL is the default time-to-live for cached search results.
const DefaultSearchCacheTTL = 6 * time.Hour

// MaxSearchCacheEntries is the maximum number of cached entries before eviction.
const MaxSearchCacheEntries = 250

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

// SearchCache is a TTLCache (see internal/server/README.md for the
// Store / Tracker / Cache convention): caches results from external
// search providers whose state cannot be observed from in-process
// events, so TTL is the only defensible freshness signal.
//
// Default TTL is 6h with LRU eviction at MaxSearchCacheEntries to
// bound memory. Thread-safe for concurrent use; a background
// goroutine (started in NewSearchCache) periodically evicts
// expired entries, stopped by Stop.
type SearchCache struct {
	mu       sync.RWMutex
	entries  map[string]cacheEntry
	ttl      time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
	// wg tracks the cleanupLoop goroutine so Stop blocks until it
	// exits. Callers that Stop and then destroy the cache must see
	// a quiesced loop.
	wg sync.WaitGroup
}

// NewSearchCache creates a new search cache with the given TTL.
// Call Start() to launch the background cleanup goroutine.
func NewSearchCache(ttl time.Duration) *SearchCache {
	return &SearchCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
}

// Start launches the background cleanup goroutine.
func (c *SearchCache) Start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.cleanupLoop()
	}()
}

// Get retrieves a cached value. Returns nil if not found or expired.
func (c *SearchCache) Get(key string) any {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || utils.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.value
}

// Set stores a value in the cache with the configured TTL.
func (c *SearchCache) Set(key string, value any) {
	c.mu.Lock()
	// Evict oldest entries if at capacity
	if len(c.entries) >= MaxSearchCacheEntries {
		c.evictOldest()
	}
	c.entries[key] = cacheEntry{
		value:     value,
		expiresAt: utils.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// Stop shuts down the background cleanup goroutine and blocks until
// it exits. Idempotent.
func (c *SearchCache) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.wg.Wait()
}

// SearchCacheKey builds a deterministic cache key from search parameters.
func SearchCacheKey(params SearchParams, tagLogic string, enrich bool) string {
	sortedTags := append([]string(nil), params.Filter...)
	sortpkg.Strings(sortedTags)
	return url.Values{
		"provider": {string(params.Provider)}, "q": {params.Search}, "author": {params.Author},
		"sort": {params.Sort}, "direction": {strconv.Itoa(params.Direction)}, "tags": sortedTags,
		"logic": {tagLogic}, "full": {strconv.FormatBool(params.Full)}, "enrich": {strconv.FormatBool(enrich)},
		"limit": {strconv.Itoa(params.Pagination.Limit)}, "offset": {strconv.Itoa(params.Pagination.Offset)}, "cursor": {params.Pagination.Cursor},
	}.Encode()
}

func (c *SearchCache) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SearchCache) cleanup() {
	now := utils.Now()
	c.mu.Lock()
	for k, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// evictOldest removes the entry closest to expiration. Must be called with lock held.
func (c *SearchCache) evictOldest() {
	var oldestKey string
	var oldestTime time.Time

	for k, entry := range c.entries {
		if oldestKey == "" || entry.expiresAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = entry.expiresAt
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

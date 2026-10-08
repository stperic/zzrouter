package upstream

import (
	"context"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// DefaultTTL is how long a successful lookup is reused. Upstream release
// cadence is measured in hours at best, so a long TTL costs nothing in
// freshness and keeps a cluster of several providers well clear of any
// rate limit.
const DefaultTTL = 12 * time.Hour

// DefaultNegativeTTL is how long a failed lookup is reused. Deliberately much
// shorter than DefaultTTL: caching an outage for twelve hours would hide a
// real update behind a transient network blip, and the failure is cheap to
// retry. Failures are cached at all so a persistently unreachable upstream
// cannot be retried on every list request.
const DefaultNegativeTTL = 15 * time.Minute

// Result is a cached lookup outcome.
type Result struct {
	Release Release
	// Err is the lookup failure, if any. A Result always carries one or the
	// other, never neither.
	Err error
	// CheckedAt is when the lookup ran. Callers surface it so an operator can
	// tell a fresh "up to date" from a stale one.
	CheckedAt time.Time
}

// Cache memoises upstream lookups with a TTL.
//
// It never blocks a caller on more than one in-flight request per provider:
// concurrent misses collapse through singleflight, so a burst of list
// requests against a cold cache makes one outbound call, not one per request.
type Cache struct {
	resolver    *Resolver
	ttl         time.Duration
	negativeTTL time.Duration
	clock       func() time.Time

	group   singleflight.Group
	mu      sync.RWMutex
	entries map[string]Result
}

// NewCache returns a Cache over the real upstream origins. A zero ttl or
// negativeTTL takes the package default.
func NewCache(ttl, negativeTTL time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if negativeTTL <= 0 {
		negativeTTL = DefaultNegativeTTL
	}
	return &Cache{
		resolver:    NewResolver(),
		ttl:         ttl,
		negativeTTL: negativeTTL,
		clock:       utils.Now,
		entries:     make(map[string]Result),
	}
}

// Lookup returns the cached result for a key, refreshing it when the entry
// is missing or stale.
//
// key comes from CacheKey: it is the provider, plus the platform when the
// provider resolves to a different source per platform. Keying on the
// provider alone would let a macOS node's Homebrew ceiling overwrite a Linux
// node's GitHub one, and whichever was fetched last would answer for both.
//
// refresh=false still fetches on a miss — it only means "reuse a fresh entry
// if there is one". Peek is the read that never touches the network, and is
// what a render path must use.
func (c *Cache) Lookup(ctx context.Context, key string, src *config.VersionSource, refresh bool) Result {
	if !refresh {
		if got, ok := c.Peek(key); ok {
			return got
		}
	}

	// singleflight keys the same way, so a cold-cache burst produces one
	// outbound request rather than one per caller.
	v, _, _ := c.group.Do(key, func() (any, error) {
		release, err := c.resolver.Latest(ctx, src)
		got := Result{Release: release, Err: err, CheckedAt: c.clock()}

		// Do not memoise the caller's own cancellation. Lookup runs on a
		// request context, so a client that hangs up mid-fetch would
		// otherwise write a failure that Peek serves for the whole negative
		// TTL — suppressing the row and blocking the background warm for
		// fifteen minutes over something upstream never did.
		if ctx.Err() == nil {
			c.mu.Lock()
			c.entries[key] = got
			c.mu.Unlock()
		}

		return got, nil
	})
	res, _ := v.(Result)
	return res
}

// Peek returns a cached result without any network call, reporting false when
// there is nothing fresh. List endpoints use this so rendering a provider list
// can never wait on an upstream.
func (c *Cache) Peek(key string) (Result, bool) {
	c.mu.RLock()
	got, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return Result{}, false
	}
	if c.expired(got) {
		return Result{}, false
	}
	return got, true
}

// expired applies the shorter negative TTL to failed lookups.
func (c *Cache) expired(r Result) bool {
	ttl := c.ttl
	if r.Err != nil {
		ttl = c.negativeTTL
	}
	return c.clock().Sub(r.CheckedAt) >= ttl
}

// Invalidate drops every entry for a provider, so the next Lookup re-fetches.
//
// Nothing in the server calls this today, and an install or upgrade does not
// need it: those change what a node has installed, not what upstream has
// published, and the comparison is recomputed from the fresh installed
// version against the same cached ceiling. It exists for a caller that
// learns the ceiling itself moved, and for `refresh=true` to have a
// non-network sibling.
//
// It clears all of a provider's per-platform keys, not just the bare one:
// such a caller knows a provider changed, not which platform's ceiling it
// should re-ask about.
func (c *Cache) Invalidate(provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, provider)
	for key := range c.entries {
		if name, _, ok := strings.Cut(key, cacheKeySep); ok && name == provider {
			delete(c.entries, key)
		}
	}
}

// cacheKeySep joins a provider to the platform whose source was used. It is
// a character neither a provider name nor a GOOS contains.
const cacheKeySep = "\x00"

// CacheKey returns the entry key for a provider under the source governing
// goos.
//
// A provider with no per-platform override keys on its name alone, so the
// single answer is shared by every node, which is both correct and one
// fetch. Only a provider that genuinely resolves differently per platform
// pays for a second entry.
func CacheKey(provider, goos string, src *config.VersionSource) string {
	if goos == "" || src == nil || len(src.Platforms) == 0 {
		return provider
	}
	// Agree with ForOS, which treats a present-but-nil entry as no override.
	// Disagreeing would file the base source's result under two keys.
	if override, ok := src.Platforms[goos]; !ok || override == nil {
		// This platform uses the base source, so it shares the base entry.
		return provider
	}
	return provider + cacheKeySep + goos
}

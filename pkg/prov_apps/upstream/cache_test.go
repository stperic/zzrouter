package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCache wires a Cache to a stub upstream with a controllable clock.
func newTestCache(t *testing.T, handler http.HandlerFunc) (*Cache, *time.Time) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	c := NewCache(time.Hour, time.Minute)
	c.resolver = stubResolver(srv.URL)
	c.clock = func() time.Time { return now }
	return c, &now
}

func tagHandler(hits *atomic.Int64, tag string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	}
}

func TestCacheServesFromMemoryUntilTTL(t *testing.T) {
	var hits atomic.Int64
	c, now := newTestCache(t, tagHandler(&hits, "b10502"))
	src := buildNumberSource()

	first := c.Lookup(context.Background(), "llamacpp", src, false)
	require.NoError(t, first.Err)
	assert.Equal(t, "b10502", first.Release.Tag)
	assert.Equal(t, int64(1), hits.Load())

	// Within the TTL, repeated lookups must not touch the network.
	for i := 0; i < 5; i++ {
		got := c.Lookup(context.Background(), "llamacpp", src, false)
		require.NoError(t, got.Err)
		assert.Equal(t, "b10502", got.Release.Tag)
	}
	assert.Equal(t, int64(1), hits.Load(), "cached reads must not re-fetch")

	*now = now.Add(time.Hour + time.Second)
	c.Lookup(context.Background(), "llamacpp", src, false)
	assert.Equal(t, int64(2), hits.Load(), "a stale entry must re-fetch")
}

func TestCacheRefreshBypassesTTL(t *testing.T) {
	var hits atomic.Int64
	c, _ := newTestCache(t, tagHandler(&hits, "b10502"))

	c.Lookup(context.Background(), "llamacpp", buildNumberSource(), false)
	c.Lookup(context.Background(), "llamacpp", buildNumberSource(), true)
	assert.Equal(t, int64(2), hits.Load())
}

// A transient outage must not be remembered for the full success TTL, or one
// network blip hides a real update for half a day.
func TestCacheUsesShorterTTLForFailures(t *testing.T) {
	var hits atomic.Int64
	c, now := newTestCache(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	src := buildNumberSource()

	first := c.Lookup(context.Background(), "llamacpp", src, false)
	require.Error(t, first.Err)
	assert.Equal(t, int64(1), hits.Load())

	// Still inside the negative TTL: reuse rather than hammer a down upstream.
	c.Lookup(context.Background(), "llamacpp", src, false)
	assert.Equal(t, int64(1), hits.Load())

	// Past the negative TTL but well inside the success TTL.
	*now = now.Add(2 * time.Minute)
	c.Lookup(context.Background(), "llamacpp", src, false)
	assert.Equal(t, int64(2), hits.Load(), "failures expire on the short TTL")
}

func TestCachePeekNeverFetches(t *testing.T) {
	var hits atomic.Int64
	c, _ := newTestCache(t, tagHandler(&hits, "b10502"))

	_, ok := c.Peek("llamacpp")
	assert.False(t, ok, "cold cache must report a miss")
	assert.Equal(t, int64(0), hits.Load(), "Peek must never touch the network")

	c.Lookup(context.Background(), "llamacpp", buildNumberSource(), false)
	got, ok := c.Peek("llamacpp")
	require.True(t, ok)
	assert.Equal(t, "b10502", got.Release.Tag)
	assert.Equal(t, int64(1), hits.Load())
}

func TestCacheInvalidate(t *testing.T) {
	var hits atomic.Int64
	c, _ := newTestCache(t, tagHandler(&hits, "b10502"))

	c.Lookup(context.Background(), "llamacpp", buildNumberSource(), false)
	c.Invalidate("llamacpp")
	_, ok := c.Peek("llamacpp")
	assert.False(t, ok)
}

// A burst against a cold cache must collapse into one outbound request, not
// one per caller.
func TestCacheCollapsesConcurrentMisses(t *testing.T) {
	var hits atomic.Int64
	release := make(chan struct{})
	c, _ := newTestCache(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release // hold the request open so all callers pile up
		w.Header().Set("Location", "/ggml-org/llama.cpp/releases/tag/b10502")
		w.WriteHeader(http.StatusFound)
	})

	const callers = 12
	var wg sync.WaitGroup
	results := make([]Result, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.Lookup(context.Background(), "llamacpp", buildNumberSource(), false)
		}()
	}

	// Let the callers reach the resolver before answering.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), hits.Load(), "%d concurrent misses must make one request", callers)
	for _, got := range results {
		require.NoError(t, got.Err)
		assert.Equal(t, "b10502", got.Release.Tag)
	}
}

// A failed lookup must be reported as a failure, never as a stale success and
// never as "same".
func TestCacheKeepsFailuresDistinguishable(t *testing.T) {
	c, _ := newTestCache(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/ggml-org/llama.cpp/releases")
		w.WriteHeader(http.StatusFound)
	})

	got := c.Lookup(context.Background(), "llamacpp", buildNumberSource(), false)
	require.Error(t, got.Err)
	assert.True(t, errors.Is(got.Err, ErrNoUpstreamRelease))
	assert.Empty(t, got.Release.Version)
	assert.Equal(t, StatusUnknown, Compare(buildNumberSource(), "b10453", got.Release.Version))
}

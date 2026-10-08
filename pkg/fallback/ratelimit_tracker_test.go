package fallback

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRateLimitHeaders_OpenAI(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Ratelimit-Limit-Requests", "200")
	headers.Set("X-Ratelimit-Remaining-Requests", "18")
	headers.Set("X-Ratelimit-Limit-Tokens", "40000")
	headers.Set("X-Ratelimit-Remaining-Tokens", "28450")
	headers.Set("X-Ratelimit-Reset-Requests", "12s")
	headers.Set("X-Ratelimit-Reset-Tokens", "1m30s")

	snap := ParseRateLimitHeaders(headers)
	require.NotNil(t, snap)

	assert.Equal(t, 200, snap.LimitRequests)
	assert.Equal(t, 18, snap.RemainingRequests)
	assert.Equal(t, 40000, snap.LimitTokens)
	assert.Equal(t, 28450, snap.RemainingTokens)
	assert.Equal(t, 12*time.Second, snap.ResetRequests)
	assert.Equal(t, 90*time.Second, snap.ResetTokens)
	assert.False(t, snap.ObservedAt.IsZero())
}

func TestParseRateLimitHeaders_ShortForm(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Ratelimit-Limit", "20")
	headers.Set("X-Ratelimit-Remaining", "5")
	headers.Set("X-Ratelimit-Reset", "30")

	snap := ParseRateLimitHeaders(headers)
	require.NotNil(t, snap)

	assert.Equal(t, 20, snap.LimitRequests)
	assert.Equal(t, 5, snap.RemainingRequests)
	assert.Equal(t, 30*time.Second, snap.ResetRequests)
}

func TestParseRateLimitHeaders_RetryAfter(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "15")

	snap := ParseRateLimitHeaders(headers)
	require.NotNil(t, snap)

	assert.Equal(t, 15*time.Second, snap.ResetRequests)
}

func TestParseRateLimitHeaders_NoHeaders(t *testing.T) {
	headers := http.Header{}
	snap := ParseRateLimitHeaders(headers)
	assert.Nil(t, snap)
}

func TestParseRateLimitHeaders_ZeroRemaining(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Ratelimit-Limit-Requests", "20")
	headers.Set("X-Ratelimit-Remaining-Requests", "0")
	headers.Set("X-Ratelimit-Reset-Requests", "42s")

	snap := ParseRateLimitHeaders(headers)
	require.NotNil(t, snap)

	assert.Equal(t, 0, snap.RemainingRequests)
	assert.Equal(t, 20, snap.LimitRequests)
	assert.Equal(t, 42*time.Second, snap.ResetRequests)
}

func TestRateLimitTracker_UpdateAndGet(t *testing.T) {
	tracker := NewRateLimitTracker()

	snap := RateLimitSnapshot{
		LimitRequests:     200,
		RemainingRequests: 18,
		ObservedAt:        time.Now(),
	}

	tracker.Update("groq", snap)

	got, ok := tracker.Get("groq")
	require.True(t, ok)
	assert.Equal(t, 200, got.LimitRequests)
	assert.Equal(t, 18, got.RemainingRequests)

	_, ok = tracker.Get("openai")
	assert.False(t, ok)
}

func TestRateLimitTracker_Snapshot(t *testing.T) {
	tracker := NewRateLimitTracker()

	tracker.Update("groq", RateLimitSnapshot{LimitRequests: 20, ObservedAt: time.Now()})
	tracker.Update("openai", RateLimitSnapshot{LimitRequests: 500, ObservedAt: time.Now()})

	snap := tracker.Snapshot()
	assert.Len(t, snap, 2)
	assert.Equal(t, 20, snap["groq"].LimitRequests)
	assert.Equal(t, 500, snap["openai"].LimitRequests)
}

package fallback

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestShouldRetry_RetriesExhausted(t *testing.T) {
	ok, _ := ShouldRetry(1, 1, 503, nil)
	assert.False(t, ok, "should not retry when attempt >= maxRetries")
}

func TestShouldRetry_NonRetriable(t *testing.T) {
	ok, _ := ShouldRetry(0, 2, 400, nil)
	assert.False(t, ok, "should not retry non-retriable status")
}

func TestShouldRetry_RetriableStatus(t *testing.T) {
	for _, code := range []int{429, 503, 504} {
		ok, backoff := ShouldRetry(0, 2, code, nil)
		assert.True(t, ok, "should retry %d", code)
		assert.Greater(t, backoff, time.Duration(0), "backoff should be positive for %d", code)
	}
}

func TestShouldRetry_BackoffGrows(t *testing.T) {
	_, b0 := ShouldRetry(0, 5, 503, nil)
	_, b1 := ShouldRetry(1, 5, 503, nil)

	// b1 should be roughly 2x b0 (within jitter)
	assert.Greater(t, b1, b0, "backoff should grow with attempt number")
}

func TestShouldRetry_BackoffCapped(t *testing.T) {
	_, b := ShouldRetry(10, 20, 503, nil)
	// Should be capped at ~4s (plus jitter)
	assert.LessOrEqual(t, b, 5*time.Second, "backoff should be capped")
}

func TestShouldRetry_ZeroMaxRetries(t *testing.T) {
	ok, _ := ShouldRetry(0, 0, 503, nil)
	assert.False(t, ok, "should not retry when maxRetries is 0")
}

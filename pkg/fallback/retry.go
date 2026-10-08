package fallback

import (
	"math"
	"math/rand/v2"
	"time"
)

const (
	// DefaultMaxRetries is the default number of retries per deployment before fallback.
	DefaultMaxRetries = 1

	// defaultBaseBackoff is the initial backoff duration.
	defaultBaseBackoff = 1 * time.Second

	// defaultMaxBackoff caps the exponential growth.
	defaultMaxBackoff = 4 * time.Second

	// jitterFraction adds randomness to avoid thundering herd.
	jitterFraction = 0.1
)

// ShouldRetry returns whether the request should be retried on the same deployment,
// and the backoff duration to wait before retrying.
// attempt is 0-indexed (0 = first retry after initial failure).
func ShouldRetry(attempt int, maxRetries int, statusCode int, err error) (bool, time.Duration) {
	if attempt >= maxRetries {
		return false, 0
	}

	if !IsRetriable(statusCode, err) {
		return false, 0
	}

	backoff := calcBackoff(attempt)
	return true, backoff
}

// calcBackoff computes exponential backoff with jitter.
// backoff = min(baseBackoff * 2^attempt, maxBackoff) ± 10% jitter
func calcBackoff(attempt int) time.Duration {
	backoff := float64(defaultBaseBackoff) * math.Pow(2, float64(attempt))
	if backoff > float64(defaultMaxBackoff) {
		backoff = float64(defaultMaxBackoff)
	}

	// Add ±10% jitter
	jitter := backoff * jitterFraction * (2*rand.Float64() - 1)
	backoff += jitter

	return time.Duration(backoff)
}

package fallback

import (
	"errors"
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

// RetryLimit is how many times this candidate is retried before the next one; unset means DefaultMaxRetries.
func (c Candidate) RetryLimit() int {
	if c.MaxRetries == nil {
		return DefaultMaxRetries
	}
	return *c.MaxRetries
}

// RetryDecision determines whether and where the chain may send again.
type RetryDecision uint8

const (
	// StopRetry ends the chain with this attempt's result.
	StopRetry RetryDecision = iota
	// RetryReplica retries the same deployment after backoff.
	RetryReplica
	// NextReplica moves on after exhausting this deployment's retries.
	NextReplica
)

// ShouldRetry owns replay safety and the retry budget for both replica choices.
func ShouldRetry(method string, err error, status, attempt, maxRetries int) (RetryDecision, time.Duration) {
	wroteRequest := true
	var sendErr *RequestError
	if errors.As(err, &sendErr) {
		wroteRequest = sendErr.WroteRequest
	}
	if !ReplaySafe(method, err, status, wroteRequest) {
		return StopRetry, 0
	}
	if attempt >= maxRetries {
		return NextReplica, 0
	}
	return RetryReplica, calcBackoff(attempt)
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

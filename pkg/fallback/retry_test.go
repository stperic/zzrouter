package fallback

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestShouldRetry_RetriesExhausted(t *testing.T) {
	ok, _ := ShouldRetry("POST", nil, 503, 1, 1)
	assert.Equal(t, NextReplica, ok, "exhausted safe retries move to the next replica")
}

func TestShouldRetry_NonRetriable(t *testing.T) {
	ok, _ := ShouldRetry("POST", nil, 400, 0, 2)
	assert.Equal(t, StopRetry, ok, "unsafe retries end the chain")
}

func TestShouldRetry_RetriableStatus(t *testing.T) {
	for _, code := range []int{429, 402, 503} {
		ok, backoff := ShouldRetry("POST", nil, code, 0, 2)
		assert.Equal(t, RetryReplica, ok, "should retry %d", code)
		assert.Greater(t, backoff, time.Duration(0), "backoff should be positive for %d", code)
	}
}

func TestShouldRetry_BackoffGrows(t *testing.T) {
	_, b0 := ShouldRetry("POST", nil, 503, 0, 5)
	_, b1 := ShouldRetry("POST", nil, 503, 1, 5)

	// b1 should be roughly 2x b0 (within jitter)
	assert.Greater(t, b1, b0, "backoff should grow with attempt number")
}

func TestShouldRetry_BackoffCapped(t *testing.T) {
	_, b := ShouldRetry("POST", nil, 503, 10, 20)
	// Should be capped at ~4s (plus jitter)
	assert.LessOrEqual(t, b, 5*time.Second, "backoff should be capped")
}

func TestShouldRetry_ZeroMaxRetries(t *testing.T) {
	ok, _ := ShouldRetry("POST", nil, 503, 0, 0)
	assert.Equal(t, NextReplica, ok, "zero same-replica retries still permits fallback")
}

func TestRetryLimit(t *testing.T) {
	three := 3
	zero := 0
	tests := []struct {
		name string
		max  *int
		want int
	}{
		{name: "unset uses default", max: nil, want: DefaultMaxRetries},
		{name: "explicit zero means no retry", max: &zero, want: 0},
		{name: "explicit value", max: &three, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Candidate{MaxRetries: tt.max}.RetryLimit())
		})
	}
}

func TestShouldRetry_WriteEvidence(t *testing.T) {
	for _, written := range []bool{false, true} {
		for _, attempt := range []int{0, 1} {
			t.Run(fmt.Sprintf("written=%t/attempt=%d", written, attempt), func(t *testing.T) {
				err := fmt.Errorf("wrapped: %w", &RequestError{Err: syscall.ECONNRESET, WroteRequest: written})
				decision, backoff := ShouldRetry("POST", err, 0, attempt, 1)
				if written {
					assert.Equal(t, StopRetry, decision)
					assert.Zero(t, backoff)
				} else if attempt == 0 {
					assert.Equal(t, RetryReplica, decision)
					assert.Positive(t, backoff)
				} else {
					assert.Equal(t, NextReplica, decision)
					assert.Zero(t, backoff)
				}
			})
		}
	}
	decision, _ := ShouldRetry("POST", errors.New("missing write evidence"), 0, 0, 1)
	assert.Equal(t, StopRetry, decision)
}

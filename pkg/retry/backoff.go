// Package retry provides a jittered-exponential-backoff policy and an
// errors.As-based retry classifier, shared by the cluster mesh, pairing
// client, and worker renewal loop. One policy object → one delay formula
// → jitter is free at every call site.
package retry

import (
	"math"
	"math/rand/v2"
	"time"
)

const (
	// defaultMultiplier applies when Backoff.Multiplier is zero.
	// Doubling per attempt is the standard exp-backoff growth factor.
	defaultMultiplier = 2.0

	// maxBackoffAttempt caps the exponent passed into math.Pow. Beyond
	// attempt 62, math.Pow(2, attempt)*Initial overflows the int64
	// time.Duration range and wraps to negative — defeating the Max
	// clamp. 62 keeps us safely inside int64 while allowing 62 doublings
	// which dwarf any plausible Max in a real retry loop.
	maxBackoffAttempt = 62

	// durationCeiling is the largest positive time.Duration, returned
	// when Max is unset and the exponent has saturated — callers still
	// sleep rather than silently skip the wait.
	durationCeiling = time.Duration(1<<63 - 1)
)

// Backoff computes the wait time between retry attempts using
// exponential backoff with optional random jitter. The zero value is
// valid but produces a constant zero delay — callers that want real
// backoff must set Initial (and usually Max + Jitter).
type Backoff struct {
	// Initial is the base delay for attempt 0 (the first retry after
	// the operation's first failure).
	Initial time.Duration
	// Max caps the exponential growth. If zero, growth is unbounded.
	Max time.Duration
	// Multiplier grows the delay each attempt. Defaults to 2.0 when 0.
	Multiplier float64
	// Jitter adds random perturbation as ±fraction of the computed
	// delay (e.g. 0.2 means ±20%). Zero means no jitter.
	Jitter float64
}

// Delay returns the wait for a given zero-indexed attempt number
// (attempt=0 is the first retry, i.e. wait after the first failure).
func (b Backoff) Delay(attempt int) time.Duration {
	if b.Initial <= 0 {
		return 0
	}
	mult := b.Multiplier
	if mult <= 0 {
		mult = defaultMultiplier
	}
	if attempt > maxBackoffAttempt {
		attempt = maxBackoffAttempt
	}
	delay := time.Duration(float64(b.Initial) * math.Pow(mult, float64(attempt)))
	if delay < 0 || (b.Max > 0 && delay > b.Max) {
		delay = b.Max
	}
	if b.Max <= 0 && delay < 0 {
		delay = durationCeiling
	}
	if b.Jitter > 0 {
		jitterRange := time.Duration(float64(delay) * b.Jitter)
		if jitterRange > 0 {
			delay += time.Duration(rand.Int64N(int64(jitterRange*2))) - jitterRange
		}
		// Floor so a large negative jitter can't drive us below half
		// the base initial (prevents "delay=0" collapse on attempt 0).
		if floor := b.Initial / 2; delay < floor {
			delay = floor
		}
	}
	return delay
}

package quota

import (
	"context"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	rateLimiterCleanupInterval = 5 * time.Minute
	rateLimiterEntryTTL        = 10 * time.Minute
	tpmWindowDuration          = 60 * time.Second
)

// RateLimiter enforces per-scope RPM and TPM limits.
// RPM uses token buckets, TPM uses tumbling windows.
// Keyed by ThrottleScope (Kind + EntityID + Model).
//
// Lifecycle: Start(ctx) launches the cleanup goroutine; Stop(ctx)
// cancels it. Restart-safe: Start→Stop→Start cycles cleanly.
type RateLimiter struct {
	rpmMu      sync.Mutex
	rpmBuckets map[ThrottleScope]*tokenBucket

	tpmMu       sync.Mutex
	tpmCounters map[ThrottleScope]*tpmWindow

	lifecycleMu sync.Mutex
	lifeCtx     context.Context
	lifeCancel  context.CancelFunc
	// wg tracks the cleanupLoop goroutine so Stop blocks until it
	// exits. The loop runs cleanup under rpmMu/tpmMu; Stop must not
	// return while those locks could still be taken by the loop.
	wg sync.WaitGroup
}

type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

type tpmWindow struct {
	tokens      int64
	windowStart time.Time
}

// NewRateLimiter creates a per-scope rate limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		rpmBuckets:  make(map[ThrottleScope]*tokenBucket),
		tpmCounters: make(map[ThrottleScope]*tpmWindow),
	}
}

// Start begins the cleanup goroutine. Returns immediately;
// the goroutine exits when ctx is cancelled or Stop is called.
// Calling Start twice on a running limiter is a no-op.
// Restart-safe: may be called again after Stop.
func (rl *RateLimiter) Start(ctx context.Context) {
	rl.lifecycleMu.Lock()
	if rl.lifeCancel != nil { // already started
		rl.lifecycleMu.Unlock()
		return
	}
	rl.lifeCtx, rl.lifeCancel = context.WithCancel(ctx)
	rl.lifecycleMu.Unlock()

	rl.wg.Add(1)
	go func() {
		defer rl.wg.Done()
		rl.cleanupLoop()
	}()
}

// Stop terminates the cleanup goroutine. Safe to call multiple
// times and safe to call when Start was never invoked.
// Restart-safe: a subsequent Start will re-launch the goroutine.
func (rl *RateLimiter) Stop(_ context.Context) {
	// Match SpendTracker.Stop: do NOT nil lifeCtx — cleanup loop
	// reads it in a select and a racy nil-deref would crash teardown.
	// cancel() alone is sufficient; the loop observes Done() and
	// returns. A subsequent Start overwrites lifeCtx under the lock.
	rl.lifecycleMu.Lock()
	cancel := rl.lifeCancel
	rl.lifeCancel = nil
	rl.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
	}
	rl.wg.Wait()
}

// AllowRPM checks if a request is allowed under the scope's RPM limit.
func (rl *RateLimiter) AllowRPM(scope ThrottleScope, limit int) bool {
	if limit <= 0 {
		return true
	}

	rl.rpmMu.Lock()
	defer rl.rpmMu.Unlock()

	bucket, exists := rl.rpmBuckets[scope]
	if !exists {
		rate := float64(limit) / 60.0
		bucket = &tokenBucket{
			tokens:     float64(limit),
			maxTokens:  float64(limit),
			refillRate: rate,
			lastRefill: utils.Now(),
		}
		rl.rpmBuckets[scope] = bucket
	}

	// Refill
	now := utils.Now()
	elapsed := now.Sub(bucket.lastRefill).Seconds()
	bucket.tokens += elapsed * bucket.refillRate
	if bucket.tokens > bucket.maxTokens {
		bucket.tokens = bucket.maxTokens
	}
	bucket.lastRefill = now

	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// AllowTPM checks if the scope's current-minute token count is under the TPM limit.
func (rl *RateLimiter) AllowTPM(scope ThrottleScope, limit int) bool {
	if limit <= 0 {
		return true
	}

	rl.tpmMu.Lock()
	defer rl.tpmMu.Unlock()

	window := rl.tpmCounters[scope]
	if window == nil {
		// Materialize the window on first check so repeated calls take the
		// normal age-and-compare path instead of the nil shortcut. Semantics
		// are unchanged (tokens=0 still passes `0 < limit`), but the window
		// clock starts ticking immediately rather than only after the first
		// RecordTokens.
		window = &tpmWindow{windowStart: utils.Now()}
		rl.tpmCounters[scope] = window
	}

	if time.Since(window.windowStart) >= tpmWindowDuration {
		window.tokens = 0
		window.windowStart = utils.Now()
		return true
	}

	return window.tokens < int64(limit)
}

// RecordTokens adds tokens to the scope's TPM window (called post-response).
func (rl *RateLimiter) RecordTokens(scope ThrottleScope, tokens int64) {
	if tokens <= 0 {
		return
	}

	rl.tpmMu.Lock()
	defer rl.tpmMu.Unlock()

	window := rl.tpmCounters[scope]
	if window == nil {
		window = &tpmWindow{windowStart: utils.Now()}
		rl.tpmCounters[scope] = window
	}

	if time.Since(window.windowStart) >= tpmWindowDuration {
		window.tokens = 0
		window.windowStart = utils.Now()
	}

	window.tokens += tokens
}

// RateLimitSnapshot is a point-in-time view of a scope's rate-limit state.
type RateLimitSnapshot struct {
	RPMLimit        int
	RPMRemaining    int
	RPMResetSeconds float64

	TPMLimit        int
	TPMRemaining    int
	TPMResetSeconds float64
}

// Snapshot reports the scope's current rate-limit state without mutating it.
func (rl *RateLimiter) Snapshot(scope ThrottleScope, rpmLimit, tpmLimit int) RateLimitSnapshot {
	snap := RateLimitSnapshot{RPMLimit: rpmLimit, TPMLimit: tpmLimit}

	if rpmLimit > 0 {
		rl.rpmMu.Lock()
		bucket := rl.rpmBuckets[scope]
		if bucket != nil {
			elapsed := time.Since(bucket.lastRefill).Seconds()
			rate := float64(rpmLimit) / 60.0
			projected := bucket.tokens + elapsed*rate
			if projected > bucket.maxTokens {
				projected = bucket.maxTokens
			}
			snap.RPMRemaining = int(projected)
			missing := bucket.maxTokens - projected
			if missing > 0 && rate > 0 {
				snap.RPMResetSeconds = missing / rate
			}
		} else {
			snap.RPMRemaining = rpmLimit
		}
		rl.rpmMu.Unlock()
	}

	if tpmLimit > 0 {
		rl.tpmMu.Lock()
		window := rl.tpmCounters[scope]
		switch {
		case window == nil:
			snap.TPMRemaining = tpmLimit
			snap.TPMResetSeconds = tpmWindowDuration.Seconds()
		case time.Since(window.windowStart) >= tpmWindowDuration:
			snap.TPMRemaining = tpmLimit
			snap.TPMResetSeconds = tpmWindowDuration.Seconds()
		default:
			remaining := int64(tpmLimit) - window.tokens
			if remaining < 0 {
				remaining = 0
			}
			snap.TPMRemaining = int(remaining)
			snap.TPMResetSeconds = (tpmWindowDuration - time.Since(window.windowStart)).Seconds()
		}
		rl.tpmMu.Unlock()
	}

	return snap
}

// cleanupLoop evicts stale entries periodically.
// Exits when the lifecycle context is cancelled.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rateLimiterCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.evictStale()
		case <-rl.lifeCtx.Done():
			return
		}
	}
}

func (rl *RateLimiter) evictStale() {
	cutoff := utils.Now().Add(-rateLimiterEntryTTL)

	rl.rpmMu.Lock()
	for scope, bucket := range rl.rpmBuckets {
		if bucket.lastRefill.Before(cutoff) {
			delete(rl.rpmBuckets, scope)
		}
	}
	rl.rpmMu.Unlock()

	rl.tpmMu.Lock()
	for scope, window := range rl.tpmCounters {
		if window.windowStart.Before(cutoff) {
			delete(rl.tpmCounters, scope)
		}
	}
	rl.tpmMu.Unlock()
}

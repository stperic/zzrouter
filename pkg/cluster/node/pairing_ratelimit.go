package clusternode

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Rate limits for the unauthenticated pairing-request endpoint. Values
// are settled in the design doc: 5 req/min per source IP, 20 req/min
// global ceiling. 80-bit pairing code entropy makes brute force
// infeasible at these rates.
const (
	pairingPerIPBurst         = 5.0
	pairingPerIPRefillPerSec  = 5.0 / 60.0
	pairingGlobalBurst        = 20.0
	pairingGlobalRefillPerSec = 20.0 / 60.0

	// Per-IP bucket TTL: drop entries whose lastRefill is older than
	// this so a steady trickle of unique source IPs can't grow the map
	// without bound.
	pairingPerIPIdleTTL = 10 * time.Minute
)

// tokenBucket is a minimal rate limiter. Used by pairingRateLimiter
// as both the global bucket and the per-IP buckets. Kept here (not
// inlined into pairingRateLimiter) so a future consumer — e.g. a
// rate-limited admin endpoint — can reuse the type without pulling
// in the HTTP glue.
//
// Uses a wall-clock refill against a `now` function so tests can
// drive it deterministically. A backwards clock step (NTP adjustment,
// VM thaw) produces a negative elapsed which the `elapsed > 0` guard
// ignores; the bucket stops refilling until wall time catches up but
// never goes negative. Go's monotonic clock (carried on utils.Now()
// values) makes this effectively production-safe.
type tokenBucket struct {
	mu           sync.Mutex
	tokens       float64
	capacity     float64
	refillPerSec float64
	lastRefill   time.Time
	now          func() time.Time
}

func newTokenBucket(capacity, refillPerSec float64) *tokenBucket {
	return &tokenBucket{
		tokens:       capacity,
		capacity:     capacity,
		refillPerSec: refillPerSec,
		lastRefill:   utils.Now(),
		now:          utils.Now,
	}
}

// Allow returns true if a token is available and consumes it; false
// otherwise. Safe for concurrent callers.
func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillPerSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastRefill = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// pairingRateLimiter couples a global bucket with a per-IP bucket map.
// Decision: allow iff both buckets admit the request. Per-IP and
// global are separate defenses — the global bucket stops a distributed
// flood from pinning the map, the per-IP bucket stops one attacker
// from exhausting the global bucket.
type pairingRateLimiter struct {
	global *tokenBucket

	mu     sync.Mutex
	perIP  map[string]*tokenBucket
	lastGC time.Time
	now    func() time.Time
	newBkt func() *tokenBucket
}

func newPairingRateLimiter() *pairingRateLimiter {
	return &pairingRateLimiter{
		global: newTokenBucket(pairingGlobalBurst, pairingGlobalRefillPerSec),
		perIP:  make(map[string]*tokenBucket),
		lastGC: utils.Now(),
		now:    utils.Now,
		newBkt: func() *tokenBucket {
			return newTokenBucket(pairingPerIPBurst, pairingPerIPRefillPerSec)
		},
	}
}

// Allow returns true iff both the per-IP bucket for ip and the global
// bucket admit. On false, the caller returns 429.
//
// GC for idle per-IP buckets is opportunistic — piggybacks on every
// Allow call so a quiet period doesn't strand entries forever. The cost
// is O(n) over the map when the gc condition hits (once per minute at
// most); cheap for the per-IP scale this endpoint sees.
func (r *pairingRateLimiter) Allow(ip string) bool {
	r.mu.Lock()
	bucket, ok := r.perIP[ip]
	if !ok {
		bucket = r.newBkt()
		r.perIP[ip] = bucket
	}
	if r.now().Sub(r.lastGC) > time.Minute {
		r.gcLocked()
		r.lastGC = r.now()
	}
	r.mu.Unlock()

	if !bucket.Allow() {
		return false
	}
	if !r.global.Allow() {
		return false
	}
	return true
}

// gcLocked drops per-IP buckets whose lastRefill is older than
// pairingPerIPIdleTTL — they're refilled to capacity at that point, so
// evicting and re-creating on the next hit is equivalent to keeping
// them around. Caller holds r.mu.
func (r *pairingRateLimiter) gcLocked() {
	cutoff := r.now().Add(-pairingPerIPIdleTTL)
	for ip, b := range r.perIP {
		b.mu.Lock()
		idle := b.lastRefill.Before(cutoff)
		b.mu.Unlock()
		if idle {
			delete(r.perIP, ip)
		}
	}
}

// pairingRateLimitMiddleware returns a gin middleware that enforces
// pairing-request rate limits keyed on the client's source IP.
//
// IP extraction: gin's ClientIP honors trusted-proxy config on the
// engine, so a cluster-port listener running behind a reverse proxy
// (unusual — the cluster listener is meant to be directly reachable
// from workers) still attributes to the real client when properly
// configured. For the common case the coord's cluster port is direct,
// ClientIP returns RemoteAddr's host.
func pairingRateLimitMiddleware(rl *pairingRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if ip == "" {
			// Fall back to RemoteAddr's host if ClientIP can't resolve.
			// "can't resolve" here means the engine has no trusted
			// proxy config AND RemoteAddr is malformed — shouldn't
			// happen in practice, but fail closed rather than admit
			// unattributed traffic.
			host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
			if err != nil || host == "" {
				c.AbortWithStatusJSON(http.StatusTooManyRequests,
					gin.H{"error": "cannot attribute source ip"})
				return
			}
			ip = host
		}
		if !rl.Allow(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}

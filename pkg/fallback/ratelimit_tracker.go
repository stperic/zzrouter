package fallback

import (
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// RateLimitSnapshot captures the rate limit state from a single provider response.
type RateLimitSnapshot struct {
	// Request-based limits
	LimitRequests     int `json:"limit_requests,omitempty"`
	RemainingRequests int `json:"remaining_requests,omitempty"`

	// Token-based limits
	LimitTokens     int `json:"limit_tokens,omitempty"`
	RemainingTokens int `json:"remaining_tokens,omitempty"`

	// Reset durations (parsed from headers)
	ResetRequests time.Duration `json:"-"`
	ResetTokens   time.Duration `json:"-"`

	// For JSON serialization
	ResetRequestsSeconds float64 `json:"reset_requests_seconds,omitempty"`
	ResetTokensSeconds   float64 `json:"reset_tokens_seconds,omitempty"`

	// When this snapshot was captured
	ObservedAt time.Time `json:"observed_at"`
}

// RateLimitTracker stores the most recent rate limit snapshot per provider.
// Thread-safe for concurrent reads and writes.
type RateLimitTracker struct {
	mu        sync.RWMutex
	snapshots map[string]RateLimitSnapshot // provider name → latest snapshot
}

// NewRateLimitTracker creates a new rate limit tracker.
func NewRateLimitTracker() *RateLimitTracker {
	return &RateLimitTracker{
		snapshots: make(map[string]RateLimitSnapshot),
	}
}

// Update records a rate limit snapshot for a provider.
func (t *RateLimitTracker) Update(provider string, snap RateLimitSnapshot) {
	t.mu.Lock()
	t.snapshots[provider] = snap
	t.mu.Unlock()
}

// Get returns the latest snapshot for a provider.
// Returns zero value and false if no snapshot exists.
func (t *RateLimitTracker) Get(provider string) (RateLimitSnapshot, bool) {
	t.mu.RLock()
	snap, ok := t.snapshots[provider]
	t.mu.RUnlock()
	return snap, ok
}

// Snapshot returns a copy of all tracked providers.
func (t *RateLimitTracker) Snapshot() map[string]RateLimitSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]RateLimitSnapshot, len(t.snapshots))
	maps.Copy(result, t.snapshots)
	return result
}

// ParseRateLimitHeaders extracts rate limit information from HTTP response headers.
// Returns nil if no rate limit headers are present.
//
// Supports the de facto standard used by OpenAI, Groq, Anthropic, and OpenRouter:
//   - x-ratelimit-limit-requests / x-ratelimit-remaining-requests / x-ratelimit-reset-requests
//   - x-ratelimit-limit-tokens / x-ratelimit-remaining-tokens / x-ratelimit-reset-tokens
//
// Also supports the shorter form used by some providers:
//   - x-ratelimit-limit / x-ratelimit-remaining / x-ratelimit-reset
func ParseRateLimitHeaders(headers http.Header) *RateLimitSnapshot {
	snap := RateLimitSnapshot{ObservedAt: utils.Now()}
	found := false

	// OpenAI/Groq/Anthropic style: separate request and token headers
	if v := headerInt(headers, "X-Ratelimit-Limit-Requests"); v > 0 {
		snap.LimitRequests = v
		found = true
	}
	if v := headerInt(headers, "X-Ratelimit-Remaining-Requests"); v >= 0 && headers.Get("X-Ratelimit-Remaining-Requests") != "" {
		snap.RemainingRequests = v
		found = true
	}
	if v := headerInt(headers, "X-Ratelimit-Limit-Tokens"); v > 0 {
		snap.LimitTokens = v
		found = true
	}
	if v := headerInt(headers, "X-Ratelimit-Remaining-Tokens"); v >= 0 && headers.Get("X-Ratelimit-Remaining-Tokens") != "" {
		snap.RemainingTokens = v
		found = true
	}

	// Reset durations (e.g., "12s", "1m30s", "6m0s")
	if d := headerDuration(headers, "X-Ratelimit-Reset-Requests"); d > 0 {
		snap.ResetRequests = d
		snap.ResetRequestsSeconds = d.Seconds()
		found = true
	}
	if d := headerDuration(headers, "X-Ratelimit-Reset-Tokens"); d > 0 {
		snap.ResetTokens = d
		snap.ResetTokensSeconds = d.Seconds()
		found = true
	}

	// Shorter form fallback (OpenRouter style: x-ratelimit-limit, x-ratelimit-remaining)
	if !found {
		if v := headerInt(headers, "X-Ratelimit-Limit"); v > 0 {
			snap.LimitRequests = v
			found = true
		}
		if v := headerInt(headers, "X-Ratelimit-Remaining"); v >= 0 && headers.Get("X-Ratelimit-Remaining") != "" {
			snap.RemainingRequests = v
			found = true
		}
		if d := headerDuration(headers, "X-Ratelimit-Reset"); d > 0 {
			snap.ResetRequests = d
			snap.ResetRequestsSeconds = d.Seconds()
			found = true
		}
	}

	// Retry-After header (used on 429 responses)
	if d := headerDuration(headers, "Retry-After"); d > 0 {
		// Retry-After applies to requests if we don't have a more specific reset
		if snap.ResetRequests == 0 {
			snap.ResetRequests = d
			snap.ResetRequestsSeconds = d.Seconds()
		}
		found = true
	}

	if !found {
		return nil
	}
	return &snap
}

// headerInt parses an integer from a header value. Returns 0 on failure.
func headerInt(headers http.Header, key string) int {
	v := headers.Get(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

// headerDuration parses a duration from a header value.
// Supports Go duration format ("12s", "1m30s") and plain seconds ("12").
// Also handles OpenRouter's millisecond epoch format by detecting large values.
func headerDuration(headers http.Header, key string) time.Duration {
	v := strings.TrimSpace(headers.Get(key))
	if v == "" {
		return 0
	}

	// Try Go duration format first (e.g., "12s", "1m30s", "6m0s")
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}

	// Try plain seconds (e.g., "12", "30")
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		if n > 1e9 {
			// Looks like a millisecond epoch (OpenRouter) — compute duration until then
			resetAt := time.UnixMilli(int64(n))
			if d := time.Until(resetAt); d > 0 {
				return d
			}
			return 0
		}
		return time.Duration(n * float64(time.Second))
	}

	return 0
}

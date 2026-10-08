package fallback

import (
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// DefaultCooldownDuration is the cooldown period for transient errors (429, 503, 504).
	DefaultCooldownDuration = 60 * time.Second

	// QuotaExhaustedDuration is the cooldown period for quota/billing errors (402).
	QuotaExhaustedDuration = 5 * time.Minute
)

// Cooldown reasons — indicate why a deployment/provider entered cooldown.
const (
	ReasonRateLimit   = "rate_limit"  // 429 Too Many Requests
	ReasonQuota       = "quota"       // 402 Payment Required / quota exhausted
	ReasonUnavailable = "unavailable" // 503/504 service unavailable or timeout
	ReasonTransport   = "transport"   // Connection refused, DNS, timeout, etc.
)

// CooldownForStatus returns the appropriate cooldown duration for an HTTP status code.
// 402 (quota exhausted) gets a longer cooldown; all other retriable codes get the default.
func CooldownForStatus(statusCode int) time.Duration {
	if statusCode == 402 {
		return QuotaExhaustedDuration
	}
	return DefaultCooldownDuration
}

// ReasonForStatus returns the cooldown reason for an HTTP status code.
func ReasonForStatus(statusCode int) string {
	switch statusCode {
	case 429:
		return ReasonRateLimit
	case 402:
		return ReasonQuota
	case 503, 504:
		return ReasonUnavailable
	default:
		return ReasonUnavailable
	}
}

// CooldownEntry holds a cooldown's expiry and reason.
type CooldownEntry struct {
	ExpiresAt time.Time
	Reason    string
}

// CooldownManager tracks per-deployment cooldown periods.
// When a deployment fails with a retriable error, it enters cooldown
// and is skipped for subsequent requests until the period expires.
type CooldownManager struct {
	mu       sync.RWMutex
	entries  map[string]CooldownEntry
	stopCh   chan struct{}
	stopOnce sync.Once
	// wg tracks the cleanupLoop goroutine so Stop blocks until it
	// exits — prevents a Stop-then-reuse race and lets goleak pass.
	wg sync.WaitGroup
	// emit is invoked outside the manager mutex on cooldown_started /
	// cooldown_ended transitions. Optional — nil = no publish.
	emit func(eventType, replica, reason string)
}

// SetEventEmitter wires a callback that fires on every transition
// from out-of-cooldown to in-cooldown (eventType = "cooldown_started")
// and from in-cooldown to out-of-cooldown ("cooldown_ended"). Callback
// runs *outside* the manager's mutex so a slow subscriber can't
// backpressure mutators on the inference hot path. Optional — leave
// unset to disable publishing.
//
// String-typed parameters here avoid a back-import from pkg/fallback
// to pkg/observability/route_events (which would create a cycle —
// pkg/observability/route_events stays a leaf). The dispatch chain
// owns the route_events.Event construction.
func (cm *CooldownManager) SetEventEmitter(emit func(eventType, replica, reason string)) {
	cm.mu.Lock()
	cm.emit = emit
	cm.mu.Unlock()
}

// NewCooldownManager creates a new cooldown manager.
// Call Start() to launch the background cleanup goroutine.
func NewCooldownManager() *CooldownManager {
	return &CooldownManager{
		entries: make(map[string]CooldownEntry),
		stopCh:  make(chan struct{}),
	}
}

// Start launches the background cleanup goroutine.
func (cm *CooldownManager) Start() {
	cm.wg.Add(1)
	go func() {
		defer cm.wg.Done()
		cm.cleanupLoop()
	}()
}

// SetCooldown puts a deployment into cooldown for the given duration with a reason.
// Emits cooldown_started when this transitions a replica from
// out-of-cooldown into cooldown (no prior active entry). A back-to-
// back SetCooldown on the same replica doesn't re-fire — agents
// observing the bus see one event per cooldown episode.
func (cm *CooldownManager) SetCooldown(name string, duration time.Duration, reason string) {
	now := utils.Now()
	cm.mu.Lock()
	prev, hadPrev := cm.entries[name]
	wasActive := hadPrev && now.Before(prev.ExpiresAt)
	cm.entries[name] = CooldownEntry{
		ExpiresAt: now.Add(duration),
		Reason:    reason,
	}
	emit := cm.emit
	cm.mu.Unlock()
	if emit != nil && !wasActive {
		emit("cooldown_started", name, reason)
	}
}

// SetCooldownIfLonger sets the cooldown only if the new duration extends beyond the current expiry.
// Avoids TOCTOU by holding the write lock for both read and write.
// Emits cooldown_started on a transition from out-of-cooldown to in-cooldown,
// matching SetCooldown — the dispatch chain's 429-retry path goes through here.
func (cm *CooldownManager) SetCooldownIfLonger(name string, duration time.Duration, reason string) {
	now := utils.Now()
	newExpiry := now.Add(duration)
	cm.mu.Lock()
	prev, hadPrev := cm.entries[name]
	wasActive := hadPrev && now.Before(prev.ExpiresAt)
	wrote := false
	if !hadPrev || newExpiry.After(prev.ExpiresAt) {
		cm.entries[name] = CooldownEntry{ExpiresAt: newExpiry, Reason: reason}
		wrote = true
	}
	emit := cm.emit
	cm.mu.Unlock()
	if emit != nil && wrote && !wasActive {
		emit("cooldown_started", name, reason)
	}
}

// InCooldown returns true if the entry is currently in cooldown.
func (cm *CooldownManager) InCooldown(name string) bool {
	cm.mu.RLock()
	entry, ok := cm.entries[name]
	cm.mu.RUnlock()

	if !ok {
		return false
	}
	return utils.Now().Before(entry.ExpiresAt)
}

// ClearCooldown removes an entry from cooldown (e.g., after a successful request).
// Emits cooldown_ended if there was an active entry to clear.
func (cm *CooldownManager) ClearCooldown(name string) {
	now := utils.Now()
	cm.mu.Lock()
	prev, hadPrev := cm.entries[name]
	wasActive := hadPrev && now.Before(prev.ExpiresAt)
	delete(cm.entries, name)
	emit := cm.emit
	cm.mu.Unlock()
	if emit != nil && wasActive {
		emit("cooldown_ended", name, prev.Reason)
	}
}

// CooldownStatus returns the remaining cooldown duration.
// Returns 0 if not in cooldown.
func (cm *CooldownManager) CooldownStatus(name string) time.Duration {
	cm.mu.RLock()
	entry, ok := cm.entries[name]
	cm.mu.RUnlock()

	if !ok {
		return 0
	}

	remaining := time.Until(entry.ExpiresAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// GetEntry returns the full cooldown entry (remaining duration + reason).
// Returns zero values if not in cooldown.
func (cm *CooldownManager) GetEntry(name string) (time.Duration, string) {
	cm.mu.RLock()
	entry, ok := cm.entries[name]
	cm.mu.RUnlock()

	if !ok {
		return 0, ""
	}

	remaining := time.Until(entry.ExpiresAt)
	if remaining <= 0 {
		return 0, ""
	}
	return remaining, entry.Reason
}

// Snapshot returns a copy of all active (non-expired) cooldown entries.
// Safe for concurrent use. Used by status endpoints.
func (cm *CooldownManager) Snapshot() map[string]CooldownEntry {
	now := utils.Now()
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	result := make(map[string]CooldownEntry, len(cm.entries))
	for name, entry := range cm.entries {
		if remaining := entry.ExpiresAt.Sub(now); remaining > 0 {
			result[name] = entry
		}
	}
	return result
}

// Stop shuts down the background cleanup goroutine and blocks until it
// exits. Safe to call multiple times.
func (cm *CooldownManager) Stop() {
	cm.stopOnce.Do(func() { close(cm.stopCh) })
	cm.wg.Wait()
}

// cleanupLoop periodically removes expired cooldown entries.
func (cm *CooldownManager) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cm.stopCh:
			return
		case <-ticker.C:
			cm.cleanup()
		}
	}
}

func (cm *CooldownManager) cleanup() {
	now := utils.Now()
	type expired struct{ name, reason string }
	var ended []expired
	cm.mu.Lock()
	for name, entry := range cm.entries {
		if now.After(entry.ExpiresAt) {
			ended = append(ended, expired{name, entry.Reason})
			delete(cm.entries, name)
		}
	}
	emit := cm.emit
	cm.mu.Unlock()
	if emit == nil {
		return
	}
	for _, e := range ended {
		emit("cooldown_ended", e.name, e.reason)
	}
}

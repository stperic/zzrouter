package group

import (
	"context"
	"log/slog"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// DefaultReaperInterval is the steady-state tick rate for the
// ephemeral-route reaper. One minute matches the plan's stated
// granularity: TTLs are minimum 60s and informational, so resolution
// finer than the minimum doesn't help agents.
const DefaultReaperInterval = time.Minute

// StartReaper launches the ephemeral-route cleanup goroutine. Returns
// a done channel that closes once the goroutine has fully exited.
// Cancel ctx to stop; the caller MUST wait on done before treating the
// store as quiesced — synchronous shutdown so a final tick can't race
// teardown.
//
// Invariants:
//   - groups with zero ExpiresAt are skipped (autoroute-generated
//     routes and perpetual user-created routes never reach the
//     expire path).
//   - the enumerate-expired pass takes RLock briefly, then releases
//     before calling expireGroup per name. expireGroup re-acquires
//     the write lock; sync.RWMutex is non-reentrant, so this is the
//     only safe pattern.
//   - TTL is best-effort: a PATCH that extends ExpiresAt concurrent
//     with the reaper's snapshot may lose the race in either
//     direction. Agents that need hard guarantees should re-issue.
//   - expireGroup publishes route_expired (not route_deleted) so
//     audit dashboards can separate server-initiated cleanup from
//     user-initiated deletes.
func (s *GroupStore) StartReaper(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		interval = DefaultReaperInterval
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapOnce()
			}
		}
	}()
	return done
}

// reapOnce walks the store once, enumerates expired routes under
// RLock, then deletes them outside the lock. Separated from the
// goroutine body so tests can drive the cleanup deterministically
// without waiting on a tick.
func (s *GroupStore) reapOnce() {
	now := utils.Now()
	s.mu.RLock()
	expired := make([]string, 0)
	for name, g := range s.groups {
		if g.ExpiresAt.IsZero() {
			continue
		}
		if now.After(g.ExpiresAt) {
			expired = append(expired, name)
		}
	}
	s.mu.RUnlock()
	if len(expired) == 0 {
		return
	}
	for _, name := range expired {
		if s.expireGroup(name) {
			slog.Info("[GroupStore] ephemeral route expired", "route", name)
		}
	}
}

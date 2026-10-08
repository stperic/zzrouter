package control

import (
	"log/slog"
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/utils"
)

// popPendingReservation removes and returns the reservation batch with
// id for keyID. Called by RecordSpendByKey during post-response
// settlement and by CancelPending on handler errors. Returns nil if
// the entry has already been settled or reaped.
func (c *Core) popPendingReservation(keyID string, id ReservationID) []quota.ReservationRecord {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	keyed := c.pendingReservations[keyID]
	entry, ok := keyed[id]
	if !ok {
		return nil
	}
	delete(keyed, id)
	if len(keyed) == 0 {
		delete(c.pendingReservations, keyID)
	}
	return entry.reservations
}

// pendingReservationReaper runs until pendingReaperStop is closed. On
// each tick it scans the stash map for entries older than
// pendingReservationTTL and cancels them via the enforcer, reclaiming
// the reserved budget. Without this, a handler that errors after
// Enforce — or a configuration where the inference log bridge is
// disabled — would leak stashes into the map forever.
//
// Trade-off accepted: if a request takes longer than
// pendingReservationTTL between Enforce() and its eventual
// RecordCompletion, the reaper cancels the reservation first. When
// the log hook then fires late, popPendingReservation returns nil so
// no settle runs — the request's token counts are still recorded for
// TPM, but its actual cost is NOT added to the key's SpendMicro
// ledger. The TTL covers realistic cold-load worst case (see
// pendingReservationTTL doc); requests that exceed it are rare and
// usually a sign of a stuck model launch, where missing the ledger
// row is the lesser evil compared to leaking budget reservations
// indefinitely. Cross-settlement is impossible by construction
// (each stash has its own ID, popped by ID not FIFO).
func (c *Core) pendingReservationReaper() {
	ticker := time.NewTicker(pendingReservationReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.pendingReaperStop:
			return
		case <-ticker.C:
			c.reapStalePendingReservations(utils.Now())
		}
	}
}

// reapStalePendingReservations walks the pending map and cancels any
// entry older than pendingReservationTTL. Split from the reaper loop
// so tests can drive it with an injected "now" and assert behaviour
// deterministically.
//
// Return value is the number of stashes reaped, for tests and logging.
func (c *Core) reapStalePendingReservations(now time.Time) int {
	cutoff := now.Add(-pendingReservationTTL)

	// Collect stale entries under the map lock, release, then cancel
	// with the enforcer's tracker (which has its own lock). Doing
	// cancels inside the map lock would hold both locks at once and
	// risk inversion against any future code path that acquires them
	// in the opposite order.
	var stale [][]quota.ReservationRecord

	c.pendingMu.Lock()
	for keyID, keyed := range c.pendingReservations {
		for id, entry := range keyed {
			if entry.createdAt.Before(cutoff) {
				stale = append(stale, entry.reservations)
				delete(keyed, id)
			}
		}
		if len(keyed) == 0 {
			delete(c.pendingReservations, keyID)
		}
	}
	c.pendingMu.Unlock()

	if len(stale) == 0 {
		return 0
	}
	for _, batch := range stale {
		c.enforcer.CancelReservations(batch)
	}
	slog.Warn("Reaped stale pending budget reservations",
		"count", len(stale),
		"ttl_seconds", pendingReservationTTL.Seconds())
	return len(stale)
}

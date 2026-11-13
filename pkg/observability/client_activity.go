package observability

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

const clientActivityLimit = 1024

// ClientActivityRetention is the idle observation window, not a connection timeout.
const ClientActivityRetention = 24 * time.Hour

// ClientIdentity identifies an authenticated caller without retaining credentials.
// Several applications may share this identity; it is not a device or session ID.
type ClientIdentity struct {
	KeyID          string `json:"key_id"`
	Name           string `json:"name"`
	Source         string `json:"source"`
	Protocol       string `json:"protocol"`
	Authentication string `json:"authentication"`
}

type clientActivityKey struct {
	id, source, protocol, authentication string
}

// ClientObservation describes HTTP activity, independently of current authorization.
type ClientObservation struct {
	ClientIdentity
	ActiveRequests int       `json:"active_requests"`
	RequestCount   uint64    `json:"request_count"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
}

// ClientActivity is a bounded, process-local observation store with no background work.
type ClientActivity struct {
	mu        sync.Mutex
	entries   map[clientActivityKey]*ClientObservation
	since     time.Time
	truncated bool
}

// NewClientActivity starts an empty observation window.
func NewClientActivity(at time.Time) *ClientActivity {
	return &ClientActivity{entries: make(map[clientActivityKey]*ClientObservation), since: at}
}

// Start records an admitted request and returns its idempotent completion callback.
func (a *ClientActivity) Start(identity ClientIdentity, at time.Time) func(time.Time) {
	a.mu.Lock()
	a.expire(at)
	key := clientActivityKey{identity.KeyID, identity.Source, identity.Protocol, identity.Authentication}
	entry := a.entries[key]
	if entry == nil && len(a.entries) >= clientActivityLimit {
		var oldest *ClientObservation
		var victim clientActivityKey
		for candidate, row := range a.entries {
			if row.ActiveRequests == 0 && (oldest == nil || row.LastSeenAt.Before(oldest.LastSeenAt)) {
				oldest, victim = row, candidate
			}
		}
		a.truncated = true
		if oldest == nil {
			a.mu.Unlock()
			return func(time.Time) {}
		}
		delete(a.entries, victim)
	}
	if entry == nil {
		entry = &ClientObservation{ClientIdentity: identity, FirstSeenAt: at}
		a.entries[key] = entry
	}
	if !at.Before(entry.LastSeenAt) {
		entry.Name = identity.Name
		entry.LastSeenAt = at
	}
	if at.Before(entry.FirstSeenAt) {
		entry.FirstSeenAt = at
	}
	entry.ActiveRequests++
	entry.RequestCount++
	a.mu.Unlock()
	var once sync.Once
	return func(finished time.Time) {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			entry.ActiveRequests--
			if !finished.Before(entry.LastSeenAt) {
				entry.LastSeenAt = finished
			}
		})
	}
}

// Snapshot returns copies, newest first. Truncated means the capacity omitted observations.
func (a *ClientActivity) Snapshot(at time.Time) (rows []ClientObservation, since time.Time, truncated bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expire(at)
	rows = make([]ClientObservation, 0, len(a.entries))
	for _, row := range a.entries {
		rows = append(rows, *row)
	}
	slices.SortFunc(rows, func(a, b ClientObservation) int {
		if order := cmp.Compare(b.ActiveRequests, a.ActiveRequests); order != 0 {
			return order
		}
		if order := b.LastSeenAt.Compare(a.LastSeenAt); order != 0 {
			return order
		}
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.KeyID, b.KeyID), cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.Authentication, b.Authentication))
	})
	return rows, a.since, a.truncated
}

func (a *ClientActivity) expire(at time.Time) {
	for key, row := range a.entries {
		if row.ActiveRequests == 0 && at.Sub(row.LastSeenAt) > ClientActivityRetention {
			delete(a.entries, key)
		}
	}
}

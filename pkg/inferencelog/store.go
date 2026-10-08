package inferencelog

import (
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultMaxEntries is the default ring buffer capacity.
const DefaultMaxEntries = 1000

// Store is a thread-safe, fixed-size ring buffer for inference log entries.
// Real-time streaming is served via the jobs firehose
// (pkg/jobs KindInferenceLog) — the bridge in internal/server mirrors each
// Add onto that handle, so Store itself owns history only.
type Store struct {
	mu      sync.RWMutex
	entries []LogEntry
	head    int // next write position
	count   int // number of valid entries (≤ cap)

	// payloads retains full request bodies for a shorter window than
	// the metadata ring, so bodies age out while their entries survive.
	payloads *payloadRing

	// totals accumulates per-model usage for the process lifetime. It hangs
	// off Add rather than off Query because the ring evicts: summing the
	// ring would answer "over the last N requests" and quietly under-report
	// once it wraps.
	totals *Totals

	closed atomic.Bool
}

// NewStore creates a new inference log store retaining maxEntries of
// metadata and full request payloads for the most recent maxPayloads of
// those. A maxPayloads of 0 disables payload retention.
func NewStore(maxEntries, maxPayloads int) *Store {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxPayloads > maxEntries {
		maxPayloads = maxEntries
	}
	return &Store{
		entries:  make([]LogEntry, maxEntries),
		payloads: newPayloadRing(maxPayloads),
		totals:   NewTotals(),
	}
}

// Add writes an entry to the ring buffer and reports whether payload was
// retained in the shorter payload window. Pass a nil payload to store
// metadata only.
func (s *Store) Add(entry LogEntry, payload *Payload) bool {
	if s.closed.Load() {
		return false
	}

	s.mu.Lock()
	// The two rings advance on different events — only payload-carrying
	// adds move the payload ring — so an evicted entry must take its
	// payload with it or the payload outlives the entry it describes.
	if s.count == len(s.entries) {
		s.payloads.remove(s.entries[s.head].ID)
	}
	if payload != nil {
		s.payloads.add(entry.ID, *payload)
	}
	entry.PayloadAvailable = s.payloads.hasBody(entry.ID)
	s.entries[s.head] = entry
	s.head = (s.head + 1) % len(s.entries)
	if s.count < len(s.entries) {
		s.count++
	}
	retained := entry.PayloadAvailable
	s.mu.Unlock()

	// Outside the ring lock: Totals has its own, and holding both would
	// order two locks for no reason.
	s.totals.Record(entry)

	return retained
}

// Totals returns the per-model usage accumulated since this store was
// created, which is process start.
func (s *Store) Totals() *Totals { return s.totals }

// Query returns entries matching the filter, newest first.
func (s *Store) Query(filter QueryFilter) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.count == 0 {
		return nil
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	var result []LogEntry
	skipped := 0

	// Iterate from newest to oldest
	for i := 0; i < s.count; i++ {
		idx := (s.head - 1 - i + len(s.entries)) % len(s.entries)
		entry := s.entries[idx]

		if !matchesFilter(entry, filter) {
			continue
		}

		if skipped < filter.Offset {
			skipped++
			continue
		}

		entry.PayloadAvailable = s.payloads.hasBody(entry.ID)
		result = append(result, entry)
		if len(result) >= limit {
			break
		}
	}

	return result
}

// Get returns a single entry by ID, or false if not found.
func (s *Store) Get(id string) (LogEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := 0; i < s.count; i++ {
		idx := (s.head - 1 - i + len(s.entries)) % len(s.entries)
		if s.entries[idx].ID == id {
			entry := s.entries[idx]
			entry.PayloadAvailable = s.payloads.hasBody(id)
			return entry, true
		}
	}
	return LogEntry{}, false
}

// Payload returns the retained request bodies for an entry, or false if
// the entry never had one or has aged out of the payload window. The
// returned bodies alias the store's copies — treat them as read-only.
func (s *Store) Payload(id string) (Payload, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.payloads.get(id)
}

// PayloadLen returns the number of retained payloads.
func (s *Store) PayloadLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.payloads.len()
}

// Len returns the number of entries currently in the buffer.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count
}

// Stop marks the store as closed, rejects further Add calls, and releases
// every retained body — payloads live in memory precisely so prompt
// content does not outlive the process.
func (s *Store) Stop() {
	if s == nil {
		return
	}
	s.closed.Store(true)
	s.mu.Lock()
	s.payloads.drop()
	s.mu.Unlock()
}

// matchesFilter returns true if the entry matches all non-zero filter fields.
func matchesFilter(entry LogEntry, filter QueryFilter) bool {
	if filter.Model != "" && !strings.EqualFold(entry.Model, filter.Model) {
		return false
	}
	if filter.Status != "" && entry.Status != filter.Status {
		return false
	}
	if filter.KeyID != "" && entry.KeyID != filter.KeyID {
		return false
	}
	if filter.TeamID != "" && entry.TeamID != filter.TeamID {
		return false
	}
	if filter.GroupName != "" && entry.GroupName != filter.GroupName {
		return false
	}
	if !filter.Since.IsZero() && entry.Timestamp.Before(filter.Since) {
		return false
	}
	return true
}

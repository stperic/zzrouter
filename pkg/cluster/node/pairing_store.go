package clusternode

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// Sentinel errors. Callers match with errors.Is.
var (
	// ErrPairingFingerprintConflict is returned by Record when a pending
	// entry already exists for the same fingerprint under a DIFFERENT
	// code. Protects against a worker flapping between codes creating
	// multiple pending entries. Surfaces as 409 at the HTTP layer.
	ErrPairingFingerprintConflict = errors.New("clusternode: pairing fingerprint already pending under a different code")

	// ErrPairingCodeUnknown means the admin Accept hit a code that has
	// no pending entry. Either never recorded, already consumed, or
	// TTL-evicted. Surfaces as 404 at the HTTP layer.
	ErrPairingCodeUnknown = errors.New("clusternode: pairing code not found")

	// ErrPairingCodeExpired means the entry existed but its TTL lapsed
	// before Accept arrived. Surfaces as 410 at the HTTP layer.
	ErrPairingCodeExpired = errors.New("clusternode: pairing code expired")

	// ErrPairingAlreadyAccepted guards against a second Accept for the
	// same code. First-writer-wins atomic burn; losers see this.
	ErrPairingAlreadyAccepted = errors.New("clusternode: pairing code already consumed")

	// ErrPairingStoreStopped is returned from WaitFor when Stop cancels
	// in-flight waiters.
	ErrPairingStoreStopped = errors.New("clusternode: pairing store stopped")
)

// pairingStatus is the state of one pending request. Lowercase
// unexported; external callers branch on the error from Accept/WaitFor,
// not on the status value directly.
type pairingStatus int

const (
	statusPending pairingStatus = iota
	statusApproved
	statusExpired
)

// defaultPairingTTL mirrors the 15-min window the design doc locks for
// both sides. Exposed as a constructor argument so tests can drive a
// compressed timeline without hitting real clocks.
const defaultPairingTTL = 15 * time.Minute

// PairingRequest is one pending worker pairing attempt. Fields are
// unexported on purpose — the store owns the mutation; callers read via
// the exported value types (PairingResult, PendingSummary). The store's
// own methods take *PairingRequest inputs for ergonomics (handler
// builds a fresh request, hands it off) but do not expose live pointers
// on read paths.
type PairingRequest struct {
	Code        string
	Fingerprint string
	NodeName    string
	CSRPEM      string
	SANs        []string
	// RemoteAddr is the worker's IP as observed by the coord on the
	// incoming /cluster/pairing-request. Captured by the handler and
	// surfaced back to the admin accept path so the coord can register
	// the newly-paired worker as a mesh endpoint without asking the
	// operator for an address out-of-band.
	RemoteAddr string

	// recordedAt is stamped by Record, not by the caller — the store is
	// the authority for "when did we see this".
	recordedAt time.Time

	// Populated by Accept.
	status     pairingStatus
	signedCert []byte
	caCert     []byte
	coordURL   string

	// done is closed exactly once by the writer that transitions the
	// entry to statusApproved or statusExpired. Readers in WaitFor
	// select on <-done and then re-read status under the store lock
	// to decide approved vs expired. Unbuffered; no values are sent.
	done chan struct{}
}

// PairingResult is the payload the worker-side long-poll returns on
// approval. Flat value type — no live pointer into the store.
type PairingResult struct {
	SignedCertPEM  []byte
	CACertPEM      []byte
	CoordinatorURL string
	NodeName       string
	Fingerprint    string
}

// PendingSummary is the shape the admin `cluster pending` CLI renders.
// Code is deliberately NOT included — the operator reads it off the
// worker, not the coordinator listing.
type PendingSummary struct {
	NodeName    string
	Fingerprint string
	RecordedAt  time.Time
	Age         time.Duration
}

// PairingStore is the coordinator-side pending-request store. One per
// coordinator Node. Safe for concurrent use.
//
// Design invariants:
//
//   - `byCode` and `byFingerprint` are kept consistent under a single
//     mutex — a concurrent Record + Accept cannot observe a half-indexed
//     entry.
//   - Code lookup in Accept uses constant-time compare against each
//     candidate. O(n) with n = pending entries; n is bounded by the
//     per-IP rate limit and the 15-min TTL, so this is fine at the
//     scale we care about (fleet bringup, not steady-state).
//   - `done` is closed exactly once per entry, by whichever writer
//     transitions it out of statusPending. Subsequent wakeups on the
//     closed channel are idempotent — Go's close-of-closed panics, but
//     the store guarantees a single close via the status guard.
//   - The GC goroutine takes the same mutex; TTL eviction is atomic
//     with Record/Accept so a waiter can't observe a racing
//     expire-then-accept inversion.
type PairingStore struct {
	mu            sync.Mutex
	byCode        map[string]*PairingRequest
	byFingerprint map[string]*PairingRequest

	ttl    time.Duration
	now    func() time.Time
	stopCh chan struct{}
	stopWg sync.WaitGroup
	// stopOnce so multiple Stop() calls are safe — the GC goroutine
	// closing stopCh on its own would race an external Stop.
	stopOnce sync.Once
	stopped  bool // guarded by mu
}

// NewPairingStore constructs a store with the given per-entry TTL. Zero
// or negative ttl defaults to the 15-min design value. Starts the
// background GC goroutine immediately; call Stop to reclaim it.
func NewPairingStore(ttl time.Duration) *PairingStore {
	return newPairingStoreWithClock(ttl, utils.Now)
}

// newPairingStoreWithClock is the test constructor — lets us drive TTL
// eviction without time.Sleep. Production uses utils.Now via
// NewPairingStore.
func newPairingStoreWithClock(ttl time.Duration, now func() time.Time) *PairingStore {
	if ttl <= 0 {
		ttl = defaultPairingTTL
	}
	s := &PairingStore{
		byCode:        make(map[string]*PairingRequest),
		byFingerprint: make(map[string]*PairingRequest),
		ttl:           ttl,
		now:           now,
		stopCh:        make(chan struct{}),
	}
	// GC cadence is ttl/4 with a floor — small enough that a test
	// using a 50ms TTL still makes forward progress within a
	// reasonable wait, large enough that production's 15-min TTL
	// doesn't wake every few seconds.
	gcInterval := ttl / 4
	if gcInterval < 10*time.Millisecond {
		gcInterval = 10 * time.Millisecond
	}
	s.stopWg.Add(1)
	go s.gcLoop(gcInterval)
	return s
}

// Record inserts a new pending entry or returns the existing one if the
// same (fingerprint, code) pair is already pending. Dedupes by
// fingerprint: a second call with the same fingerprint but a different
// code returns ErrPairingFingerprintConflict — the worker is expected
// to send stable (code, fingerprint) pairs within a pairing window.
//
// The input PairingRequest is shallow-copied into store-owned state;
// the caller's struct is NOT retained. recordedAt, status, done, and
// the approved-only fields are set by the store.
func (s *PairingStore) Record(req *PairingRequest) (*PairingRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", ErrInvalidConfig)
	}
	if req.Code == "" || req.Fingerprint == "" {
		return nil, fmt.Errorf("%w: code and fingerprint required", ErrInvalidConfig)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return nil, ErrPairingStoreStopped
	}

	// Evict the target entry if it's TTL-expired before we branch on
	// index state — otherwise a stale entry under the same fingerprint
	// would spuriously reject a legitimate re-entry attempt.
	s.evictExpiredLocked()

	if existing, ok := s.byFingerprint[req.Fingerprint]; ok {
		// Same fingerprint already pending.
		if constantTimeStringEqual(existing.Code, req.Code) {
			// Idempotent re-submit — return the existing entry so
			// the caller's WaitFor observes the same channel/state.
			return existing, nil
		}
		return nil, ErrPairingFingerprintConflict
	}
	if _, ok := s.byCode[req.Code]; ok {
		// Code collision under a different fingerprint. Vanishingly
		// unlikely at 80-bit entropy, but treat it as a conflict
		// rather than overwrite the prior entry.
		return nil, ErrPairingFingerprintConflict
	}

	entry := &PairingRequest{
		Code:        req.Code,
		Fingerprint: req.Fingerprint,
		NodeName:    req.NodeName,
		CSRPEM:      req.CSRPEM,
		SANs:        append([]string(nil), req.SANs...),
		RemoteAddr:  req.RemoteAddr,
		recordedAt:  s.now(),
		status:      statusPending,
		done:        make(chan struct{}),
	}
	s.byCode[entry.Code] = entry
	s.byFingerprint[entry.Fingerprint] = entry
	return entry, nil
}

// Accept atomically burns the code, marks the entry approved, and wakes
// any WaitFor goroutine. Returns a copy of the approved request so the
// admin handler can render node_name / short_form in the 200 response
// without holding a live store pointer.
//
// First-writer-wins: a second Accept for the same code returns
// ErrPairingAlreadyAccepted. An Accept for an unknown code returns
// ErrPairingCodeUnknown; for an expired code, ErrPairingCodeExpired.
func (s *PairingStore) Accept(code string, signedCert, caCert []byte, coordURL string) (*PairingRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return nil, ErrPairingStoreStopped
	}

	entry := s.lookupByCodeLocked(code)
	if entry == nil {
		return nil, ErrPairingCodeUnknown
	}
	if s.isExpiredLocked(entry) {
		s.expireLocked(entry)
		return nil, ErrPairingCodeExpired
	}
	if entry.status != statusPending {
		return nil, ErrPairingAlreadyAccepted
	}

	entry.status = statusApproved
	entry.signedCert = append([]byte(nil), signedCert...)
	entry.caCert = append([]byte(nil), caCert...)
	entry.coordURL = coordURL
	close(entry.done)

	// Return a shallow copy so the caller doesn't retain a pointer
	// into the store. Store still owns the original; GC-on-delivery
	// happens in WaitFor once the worker poll delivers the approved
	// response.
	return copyRequest(entry), nil
}

// WaitFor long-polls for a status transition on the entry identified
// by code. Blocks until one of:
//
//   - Entry transitions to approved: returns the PairingResult and
//     deletes the entry (delivery consumes it).
//   - Entry expires: returns ErrPairingCodeExpired.
//   - Store Stop fires: returns ErrPairingStoreStopped.
//   - ctx.Done fires: returns ctx.Err (typical path for a 30s server-
//     side hold that the worker will then reconnect on).
//
// On ctx cancellation the entry is NOT deleted — the worker is expected
// to reconnect and WaitFor again on the same code.
func (s *PairingStore) WaitFor(ctx context.Context, code string) (*PairingResult, error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, ErrPairingStoreStopped
	}
	entry := s.lookupByCodeLocked(code)
	if entry == nil {
		s.mu.Unlock()
		return nil, ErrPairingCodeUnknown
	}
	// Fast path — already approved when we arrived (Accept ran before
	// we did). Deliver immediately.
	if entry.status == statusApproved {
		result := resultOf(entry)
		s.deleteLocked(entry)
		s.mu.Unlock()
		return result, nil
	}
	if s.isExpiredLocked(entry) {
		s.expireLocked(entry)
		s.mu.Unlock()
		return nil, ErrPairingCodeExpired
	}
	done := entry.done
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stopCh:
		return nil, ErrPairingStoreStopped
	case <-done:
		// Re-acquire the lock and re-read — entry could be approved
		// or expired depending on who closed done.
		s.mu.Lock()
		defer s.mu.Unlock()
		// Re-locate by code; entry pointer is still valid even if it
		// was deleted from the index (another goroutine's parallel
		// Accept+WaitFor delivery wouldn't change the pointer).
		switch entry.status {
		case statusApproved:
			result := resultOf(entry)
			s.deleteLocked(entry)
			return result, nil
		case statusExpired:
			return nil, ErrPairingCodeExpired
		default:
			// Shouldn't happen — done is closed only on status
			// transition. Treat as expired to fail closed.
			return nil, ErrPairingCodeExpired
		}
	}
}

// inspect returns a read-only copy of the pending entry for the given
// code, or nil if no such entry exists / the entry is no longer
// pending. Used by the admin-accept path to pull the stashed CSR out
// so it can be signed BEFORE the atomic Accept commit. Does NOT
// consume the code and does NOT wake waiters.
func (s *PairingStore) inspect(code string) *PairingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
	}
	s.evictExpiredLocked()
	entry := s.lookupByCodeLocked(code)
	if entry == nil || entry.status != statusPending {
		return nil
	}
	return copyRequest(entry)
}

// Pending returns a snapshot of all currently-pending entries for the
// admin `cluster pending` CLI. Code is redacted.
func (s *PairingStore) Pending() []PendingSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	out := make([]PendingSummary, 0, len(s.byCode))
	now := s.now()
	for _, e := range s.byCode {
		if e.status != statusPending {
			continue
		}
		out = append(out, PendingSummary{
			NodeName:    e.NodeName,
			Fingerprint: e.Fingerprint,
			RecordedAt:  e.recordedAt,
			Age:         now.Sub(e.recordedAt),
		})
	}
	// Stable ordering so the CLI renders deterministically and tests
	// can assert exact output.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out
}

// Stop terminates the GC goroutine and wakes all in-flight WaitFor
// calls with ErrPairingStoreStopped. Safe to call multiple times; only
// the first call has effect. Blocks until the GC goroutine exits.
func (s *PairingStore) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.stopCh)
	})
	s.stopWg.Wait()
}

// ---------------------------------------------------------------------
// Internal helpers. All assume caller holds s.mu unless noted.
// ---------------------------------------------------------------------

// lookupByCodeLocked walks byCode with a constant-time compare so an
// attacker probing wrong codes can't learn an active code via response
// timing. Returns the matched entry or nil.
func (s *PairingStore) lookupByCodeLocked(candidate string) *PairingRequest {
	var hit *PairingRequest
	for k, v := range s.byCode {
		if constantTimeStringEqual(k, candidate) {
			hit = v
			// Don't break — walking the full map keeps timing
			// independent of where the match sits.
		}
	}
	return hit
}

// isExpiredLocked reports whether entry's TTL has lapsed.
func (s *PairingStore) isExpiredLocked(entry *PairingRequest) bool {
	return s.now().Sub(entry.recordedAt) > s.ttl
}

// expireLocked transitions entry to statusExpired, removes it from
// both indices, and closes done exactly once.
func (s *PairingStore) expireLocked(entry *PairingRequest) {
	if entry.status == statusPending {
		entry.status = statusExpired
		close(entry.done)
	}
	s.deleteLocked(entry)
}

// deleteLocked removes entry from both indices. Idempotent — already-
// deleted entries are a no-op.
func (s *PairingStore) deleteLocked(entry *PairingRequest) {
	if cur, ok := s.byCode[entry.Code]; ok && cur == entry {
		delete(s.byCode, entry.Code)
	}
	if cur, ok := s.byFingerprint[entry.Fingerprint]; ok && cur == entry {
		delete(s.byFingerprint, entry.Fingerprint)
	}
}

// evictExpiredLocked walks byCode and expires any lapsed pending
// entries. Called on every Record/Accept/Pending path so expiry is
// observed promptly even if the GC goroutine hasn't ticked yet.
func (s *PairingStore) evictExpiredLocked() {
	for _, e := range s.byCode {
		if e.status == statusPending && s.isExpiredLocked(e) {
			s.expireLocked(e)
		}
	}
}

// gcLoop wakes at interval and evicts TTL-expired entries. Unblocked
// waiters on newly-expired entries receive ErrPairingCodeExpired via
// their done channel.
func (s *PairingStore) gcLoop(interval time.Duration) {
	defer s.stopWg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.mu.Lock()
			s.evictExpiredLocked()
			s.mu.Unlock()
		}
	}
}

// constantTimeStringEqual compares two strings in constant time when
// lengths match. Different lengths take the fast-false path (length is
// not considered secret — code length is fixed by the protocol).
func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// resultOf extracts a PairingResult from an approved entry.
func resultOf(e *PairingRequest) *PairingResult {
	return &PairingResult{
		SignedCertPEM:  append([]byte(nil), e.signedCert...),
		CACertPEM:      append([]byte(nil), e.caCert...),
		CoordinatorURL: e.coordURL,
		NodeName:       e.NodeName,
		Fingerprint:    e.Fingerprint,
	}
}

// copyRequest returns a shallow copy of entry with its slice fields
// duplicated so callers can't mutate store state.
func copyRequest(e *PairingRequest) *PairingRequest {
	return &PairingRequest{
		Code:        e.Code,
		Fingerprint: e.Fingerprint,
		NodeName:    e.NodeName,
		CSRPEM:      e.CSRPEM,
		SANs:        append([]string(nil), e.SANs...),
		RemoteAddr:  e.RemoteAddr,
		recordedAt:  e.recordedAt,
		status:      e.status,
		signedCert:  append([]byte(nil), e.signedCert...),
		caCert:      append([]byte(nil), e.caCert...),
		coordURL:    e.coordURL,
	}
}

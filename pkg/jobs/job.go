package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// job is the server-side state of a running (or recently-terminated)
// operation. It implements Handle for producer use.
//
// Locking discipline: j.mu guards every mutable field AND is held
// across every fan-out to subscribers. Subscribers use their own s.mu
// internally (for deliver vs close serialization) but never acquire
// j.mu. Lock order is j.mu > s.mu — Unsubscribe's j.detach path
// acquires j.mu first, then calls sub.close which acquires s.mu. No
// path acquires j.mu while holding s.mu.
type job struct {
	// Immutable after construction.
	id            string
	kind          Kind
	epoch         string
	node          string
	createdBy     string
	policy        RingPolicy
	inactivityTTL time.Duration
	subBuffer     int
	clk           clock.Clock
	ctx           context.Context
	cancel        context.CancelFunc

	// Guarded by mu.
	mu             sync.Mutex
	phase          Phase
	seq            uint64 // next seq to emit
	meta           Meta
	ring           []Event // nil for Firehose
	firstSeq       uint64  // seq of ring[0] (when ring non-empty)
	subs           []*Subscriber
	lastActivityAt time.Time
	terminatedAt   time.Time
	lastEvent      Event // most recent, for Get snapshots
	evicted        bool  // true after Registry.Stop → evict
}

// ID reports the kind-prefixed job ID.
func (j *job) ID() string { return j.id }

// Epoch reports the per-job nonce.
func (j *job) Epoch() string { return j.epoch }

// Context returns the job's lifecycle context. Cancelled on Registry.Stop,
// Cancel, or after the job's caller-parent ctx cancels.
func (j *job) Context() context.Context { return j.ctx }

// Progress emits a running-phase event. No-op after terminal. Percent
// is clamped to [0,100] silently.
func (j *job) Progress(percent int, step string, bytes Bytes) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	var bp *Bytes
	if bytes != (Bytes{}) {
		b := bytes
		bp = &b
	}
	j.emit(PhaseRunning, percent, step, bp, "")
}

// Meta merges m into the sidecar state. Empty on nil. Does not emit an
// event — next emitted event carries the merged meta.
func (j *job) Meta(m Meta) {
	if len(m) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.phase.IsTerminal() {
		return
	}
	if j.meta == nil {
		j.meta = make(Meta, len(m))
	}
	for k, v := range m {
		j.meta[k] = v
	}
	j.lastActivityAt = j.clk.Now()
}

// Done emits the terminal done event and seals the job.
func (j *job) Done() {
	j.emit(PhaseDone, 100, "", nil, "")
}

// Fail emits the terminal failed event. A nil err is treated as empty.
func (j *job) Fail(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	j.emit(PhaseFailed, 0, "", nil, msg)
}

// failSilent is used by the janitor to reap inactive jobs.
var errInactive = errors.New("job reaped: inactivity timeout exceeded")

func (j *job) failSilent() {
	j.Fail(errInactive)
}

// emitInitial emits the pending event exactly once, right after
// construction. No subscribers can be attached yet.
func (j *job) emitInitial() {
	j.mu.Lock()
	defer j.mu.Unlock()
	ev := Event{
		JobID: j.id,
		Node:  j.node,
		Kind:  j.kind,
		Epoch: j.epoch,
		Seq:   j.seq,
		At:    j.clk.Now(),
		Phase: PhasePending,
		Meta:  cloneMeta(j.meta),
	}
	j.seq++
	j.lastEvent = ev
	j.appendRing(ev)
	// No subscribers yet; nothing to deliver.
}

// emit is the shared producer path for running / done / failed events.
// Late calls after terminal are silently dropped. Fan-out to
// subscribers happens under j.mu so Subscribe's replay-then-register
// sequence cannot interleave with a live event.
func (j *job) emit(phase Phase, percent int, step string, bytes *Bytes, errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.phase.IsTerminal() {
		return
	}
	warning, _ := j.meta["warning"].(string)
	now := j.clk.Now()
	j.phase = phase
	j.lastActivityAt = now
	if phase.IsTerminal() {
		j.terminatedAt = now
	}
	ev := Event{
		JobID:   j.id,
		Node:    j.node,
		Kind:    j.kind,
		Epoch:   j.epoch,
		Seq:     j.seq,
		At:      now,
		Phase:   phase,
		Percent: percent,
		Step:    step,
		Bytes:   bytes,
		Meta:    cloneMeta(j.meta),
		Err:     errMsg,
		Warning: warning,
	}
	j.seq++
	j.lastEvent = ev
	j.appendRing(ev)

	for _, s := range j.subs {
		s.deliver(ev)
	}

	if phase.IsTerminal() {
		j.cancel()
		for _, s := range j.subs {
			s.close()
		}
		j.subs = nil
	}
}

// appendRing inserts ev into the ring. Caller holds mu. No-op for
// Firehose kinds (ring is nil).
func (j *job) appendRing(ev Event) {
	if j.ring == nil {
		return
	}
	if len(j.ring) < j.policy.Size {
		if len(j.ring) == 0 {
			j.firstSeq = ev.Seq
		}
		j.ring = append(j.ring, ev)
		return
	}
	// Ring full: drop oldest, append new. firstSeq advances.
	copy(j.ring, j.ring[1:])
	j.ring[len(j.ring)-1] = ev
	j.firstSeq++
}

// snapshot returns the most-recently-emitted event. Used by Get.
func (j *job) snapshot() Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastEvent
}

// phaseAndTimestamps is the janitor's read-only accessor.
func (j *job) phaseAndTimestamps() (Phase, time.Time, time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.phase, j.lastActivityAt, j.terminatedAt
}

// evict is called by Registry.Stop to tear down a job immediately. Does
// not emit a terminal event — the registry is going away, so subscribers
// should reconnect rather than treat this as a job outcome.
func (j *job) evict() {
	j.cancel()
	j.mu.Lock()
	j.evicted = true
	toClose := j.subs
	j.subs = nil
	j.mu.Unlock()
	for _, s := range toClose {
		s.close()
	}
}

// subscribe attaches a new Subscriber. Handles replay, Firehose live-
// only, terminal-within-TTL, and post-eviction stubs. Runs replay +
// registration atomically under j.mu so no live event can slip between
// the two.
func (j *job) subscribe(opts SubscribeOptions) (*Subscriber, error) {
	if opts.Epoch != "" && opts.Epoch != j.epoch {
		return nil, ErrEpochMismatch
	}
	if j.policy.Mode == RingFirehose && opts.From != nil && *opts.From > 0 {
		return nil, ErrReplayUnsupported
	}

	var sub *Subscriber
	detach := func() { j.detach(sub) }
	sub = newSubscriber(j.subBuffer, detach)

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.evicted {
		// Registry was stopped before this Subscribe took the lock.
		// Return an already-closed subscriber so the caller's reader
		// observes immediate end-of-stream (consistent with the
		// "registry teardown" failure mode in the plan doc).
		sub.close()
		return sub, nil
	}

	replay, dropped := j.collectReplay(opts)
	if dropped != nil {
		sub.deliver(*dropped)
	}
	for _, ev := range replay {
		sub.deliver(ev)
	}

	if j.phase.IsTerminal() {
		// Terminal-within-TTL: replay already contained the terminal
		// event (it's the most recent ring entry). Close the channel
		// so the reader observes end-of-stream after draining.
		sub.close()
		return sub, nil
	}

	j.subs = append(j.subs, sub)
	return sub, nil
}

// collectReplay builds the ordered replay slice under the job lock.
// Returns (events, optional dropped marker). The marker is populated
// when the caller's From seq predates the ring.
func (j *job) collectReplay(opts SubscribeOptions) ([]Event, *Event) {
	if j.policy.Mode == RingFirehose || len(j.ring) == 0 {
		return nil, nil
	}

	from := uint64(0)
	if opts.From != nil {
		from = *opts.From
	}

	var dropped *Event
	if from > 0 && from < j.firstSeq {
		marker := Event{
			JobID: j.id,
			Node:  j.node,
			Kind:  j.kind,
			Epoch: j.epoch,
			Seq:   j.firstSeq,
			At:    j.clk.Now(),
			Phase: j.phase,
			Dropped: &DroppedMarker{
				Since:   from,
				Current: j.firstSeq,
			},
		}
		dropped = &marker
		from = j.firstSeq
	}

	out := make([]Event, 0, len(j.ring))
	for _, ev := range j.ring {
		if ev.Seq >= from {
			out = append(out, ev)
		}
	}
	return out, dropped
}

// detach removes sub from the job's subscriber list and closes it.
// Called via Subscriber.Unsubscribe. Acquires j.mu (which never nests
// under s.mu), so there is no deadlock with deliver.
func (j *job) detach(sub *Subscriber) {
	j.mu.Lock()
	for i, s := range j.subs {
		if s == sub {
			j.subs = append(j.subs[:i], j.subs[i+1:]...)
			break
		}
	}
	j.mu.Unlock()
	sub.close()
}

package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// Default janitor tick interval. Chosen so a 5-min CompletedTTL is
// evicted within ~10% of its window; inactivity gating does not need
// sub-second precision (kind timeouts are minutes-scale).
const defaultJanitorInterval = 30 * time.Second

// Default CompletedTTL picked from the plan doc (plan_jobs.md
// §Lifecycle): human-in-the-loop TUI reconnect after brief network
// loss needs well over 60s. 5min covers handoffs and SSH reconnects.
const defaultCompletedTTL = 5 * time.Minute

// Default subscriber channel buffer. Small enough to surface
// backpressure quickly (drop-oldest counts grow, operator notices);
// large enough to absorb normal network jitter.
const defaultSubscriberBuffer = 16

// Config bundles Registry construction inputs.
type Config struct {
	// NodeName stamps every event's Node field. Required.
	NodeName string

	// Clock is the time source. Required — pass clock.System() in
	// production, clocktest.NewFakeClock(..) in tests.
	Clock clock.Clock

	// CompletedTTL is the window a terminal job stays in the registry
	// before eviction. Late subscribers within the window replay the
	// ring and see the final event; after eviction, Get returns
	// ErrNotFound. Zero → defaultCompletedTTL.
	CompletedTTL time.Duration

	// JanitorInterval is the tick period for the TTL + inactivity
	// sweep goroutine. Zero → defaultJanitorInterval.
	JanitorInterval time.Duration

	// SubscriberBuffer is the per-subscriber channel slot count. Zero
	// → defaultSubscriberBuffer.
	SubscriberBuffer int

	// KindPolicy overrides the default ring policy for specific kinds.
	// Missing entries fall back to Kind.DefaultRingPolicy.
	KindPolicy map[Kind]RingPolicy

	// KindInactivity overrides the default inactivity reap window per
	// kind. Missing entries fall back to Kind.DefaultInactivityTimeout.
	KindInactivity map[Kind]time.Duration
}

// Registry is the per-node job registry. Construct with NewRegistry;
// call Start (implicit via first use) and Stop to manage lifecycle.
// All methods are safe for concurrent use.
type Registry struct {
	cfg Config
	clk clock.Clock

	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	jobs map[string]*job
	done bool

	wg sync.WaitGroup
}

// NewRegistry constructs a Registry and starts the janitor. Returns an
// error if required config is missing.
func NewRegistry(cfg Config) (*Registry, error) {
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("jobs: NodeName is required")
	}
	if cfg.Clock == nil {
		return nil, fmt.Errorf("jobs: Clock is required")
	}
	if cfg.CompletedTTL <= 0 {
		cfg.CompletedTTL = defaultCompletedTTL
	}
	if cfg.JanitorInterval <= 0 {
		cfg.JanitorInterval = defaultJanitorInterval
	}
	if cfg.SubscriberBuffer <= 0 {
		cfg.SubscriberBuffer = defaultSubscriberBuffer
	}
	// Copy caller-provided maps so post-construction mutation on the
	// caller's side cannot race the registry's reads.
	if cfg.KindPolicy != nil {
		copied := make(map[Kind]RingPolicy, len(cfg.KindPolicy))
		for k, v := range cfg.KindPolicy {
			copied[k] = v
		}
		cfg.KindPolicy = copied
	}
	if cfg.KindInactivity != nil {
		copied := make(map[Kind]time.Duration, len(cfg.KindInactivity))
		for k, v := range cfg.KindInactivity {
			copied[k] = v
		}
		cfg.KindInactivity = copied
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &Registry{
		cfg:    cfg,
		clk:    cfg.Clock,
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(map[string]*job),
	}
	r.wg.Add(1)
	go r.janitorLoop()
	return r, nil
}

// Stop halts the janitor, cancels every job's context, evicts all jobs,
// and closes all subscriber channels. Safe to call multiple times.
func (r *Registry) Stop() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	jobs := make([]*job, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.jobs = make(map[string]*job)
	r.mu.Unlock()

	r.cancel()
	for _, j := range jobs {
		j.evict()
	}
	r.wg.Wait()
}

// policyFor returns the resolved ring policy for a kind (config override
// or kind default).
func (r *Registry) policyFor(k Kind) RingPolicy {
	if p, ok := r.cfg.KindPolicy[k]; ok {
		return p
	}
	return k.DefaultRingPolicy()
}

// inactivityFor returns the resolved inactivity timeout.
func (r *Registry) inactivityFor(k Kind) time.Duration {
	if d, ok := r.cfg.KindInactivity[k]; ok {
		return d
	}
	return k.DefaultInactivityTimeout()
}

// Start opens a new job. The returned Handle is the producer-side
// surface; store it and call Progress/Done/Fail from the work
// goroutine. ctx is the parent for the job's lifecycle context;
// cancelling ctx signals the producer via Handle.Context().
//
// Meta is the initial sidecar payload; subsequent Handle.Meta calls
// merge into it. createdBy identifies the principal that initiated the
// job (admin key ID, virtual-key ID, etc.); captured now as a
// forward-compat hook for per-principal authz on subscribe.
//
// Use StartDetached for any handler that returns 202 + job_id; only
// pass a caller ctx when the caller will block on completion. Any
// request-scoped ctx (HTTP, RPC, anything that cancels on response
// flush or stream close) will cancel the job microseconds after Start
// returns and the goroutine starts. Bug class fixed three times:
// KindRun fa91f367, KindSync fa91f367, KindInstall × 4 paths 167dd7df.
func (r *Registry) Start(ctx context.Context, kind Kind, createdBy string, meta Meta) (Handle, error) {
	return r.start(ctx, kind, createdBy, kind.idPrefix()+randomHex(8), meta)
}

// Resume restores a domain owner's persisted job ID with a fresh stream epoch.
func (r *Registry) Resume(ctx context.Context, kind Kind, createdBy, id string, meta Meta) (Handle, error) {
	prefix := kind.idPrefix()
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+16 {
		return nil, fmt.Errorf("invalid persisted job ID")
	}
	if _, err := hex.DecodeString(id[len(prefix):]); err != nil {
		return nil, fmt.Errorf("invalid persisted job ID: %w", err)
	}
	return r.start(ctx, kind, createdBy, id, meta)
}

func (r *Registry) start(ctx context.Context, kind Kind, createdBy, id string, meta Meta) (Handle, error) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return nil, ErrRegistryStopped
	}
	if _, exists := r.jobs[id]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("job ID already exists")
	}
	epoch := randomHex(8)
	policy := r.policyFor(kind)

	// Derive the job's ctx from BOTH the registry's lifecycle ctx and
	// the caller-provided ctx: either cancelling cancels the job.
	mergedCtx, cancel := mergeCancel(r.ctx, ctx)

	j := &job{
		id:             id,
		kind:           kind,
		epoch:          epoch,
		node:           r.cfg.NodeName,
		createdBy:      createdBy,
		policy:         policy,
		inactivityTTL:  r.inactivityFor(kind),
		subBuffer:      r.cfg.SubscriberBuffer,
		clk:            r.clk,
		ctx:            mergedCtx,
		cancel:         cancel,
		meta:           cloneMeta(meta),
		phase:          PhasePending,
		lastActivityAt: r.clk.Now(),
	}
	if policy.Mode != RingFirehose && policy.Size > 0 {
		j.ring = make([]Event, 0, policy.Size)
	}

	r.jobs[id] = j
	r.mu.Unlock()

	// Emit the pending event — seq 0, established before any subscriber
	// can observe so the ring invariant "index 0 is always seq 0" holds.
	j.emitInitial()
	return j, nil
}

// StartDetached opens a job whose lifecycle is bounded only by the
// Registry's own ctx — Registry.Stop and explicit Cancel still cancel
// Handle.Context(); nothing else can. Use for any HTTP handler that
// returns 202 + job_id and expects the work to outlive the request.
//
// Equivalent to Start(context.Background(), ...) but explicit at the
// call site, so a future reader doesn't have to recognize "background"
// as the safe choice from the caller-ctx footgun.
func (r *Registry) StartDetached(kind Kind, createdBy string, meta Meta) (Handle, error) {
	return r.Start(context.Background(), kind, createdBy, meta)
}

// Get returns a snapshot of the job's most recent event, or ErrNotFound.
func (r *Registry) Get(id string) (Event, error) {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return Event{}, ErrNotFound
	}
	return j.snapshot(), nil
}

// List returns snapshots of every job in the registry. Filter by
// providing a non-empty Kind; zero-value means "any."
func (r *Registry) List(kind Kind) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, 0, len(r.jobs))
	for _, j := range r.jobs {
		if kind != "" && j.kind != kind {
			continue
		}
		out = append(out, j.snapshot())
	}
	return out
}

// SubscribeOptions controls Subscribe behavior.
type SubscribeOptions struct {
	// From, when non-nil, is the seq to replay from (inclusive). Nil
	// means "live-only" — no historical replay. Unsupported on
	// Firehose kinds when *From > 0.
	From *uint64

	// Epoch, if non-empty, must match the job's current epoch or
	// Subscribe returns ErrEpochMismatch. Clients use this to detect
	// worker-restart-under-same-ID.
	Epoch string
}

// Subscribe attaches a live listener to the job. Returns a Subscriber
// whose Events channel closes when the job terminates + drains, when
// Unsubscribe is called, or when the registry stops. The subscriber
// receives (a) any replayed events from the ring, (b) a Dropped marker
// if From is older than the ring holds, (c) live events going forward.
func (r *Registry) Subscribe(id string, opts SubscribeOptions) (*Subscriber, error) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return nil, ErrRegistryStopped
	}
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return j.subscribe(opts)
}

// Cancel fires the job's context. The producer observes this via
// Handle.Context().Done() and is expected to wind down promptly.
// Idempotent. Returns ErrNotFound if the ID is unknown.
func (r *Registry) Cancel(id string) error {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	j.cancel()
	return nil
}

// janitorLoop sweeps terminal jobs past CompletedTTL and flips silently
// stalled running jobs to failed. Runs until Registry.Stop.
func (r *Registry) janitorLoop() {
	defer r.wg.Done()
	ticker := r.clk.NewTicker(r.cfg.JanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.Chan():
			r.sweep()
		}
	}
}

// sweep walks the registry once. Inactivity-gated fails run first so
// they participate in the same CompletedTTL eviction window as
// organically-terminated jobs.
func (r *Registry) sweep() {
	now := r.clk.Now()

	r.mu.Lock()
	toFail := make([]*job, 0)
	toEvict := make([]string, 0)
	for id, j := range r.jobs {
		phase, lastActivity, terminatedAt := j.phaseAndTimestamps()
		if phase == PhaseRunning && j.inactivityTTL > 0 && now.Sub(lastActivity) > j.inactivityTTL {
			toFail = append(toFail, j)
			continue
		}
		if phase.IsTerminal() && now.Sub(terminatedAt) > r.cfg.CompletedTTL {
			toEvict = append(toEvict, id)
		}
	}
	for _, id := range toEvict {
		delete(r.jobs, id)
	}
	r.mu.Unlock()

	for _, j := range toFail {
		slog.Warn("jobs: reaping inactive job",
			"subsystem", "jobs",
			"job_id", j.id,
			"kind", j.kind,
			"inactivity_ttl", j.inactivityTTL)
		j.failSilent()
	}
}

// randomHex returns 2n hex chars from crypto/rand. Panics on RNG
// failure — the registry cannot construct unique IDs without entropy,
// and returning an error per-Start would surface a latent boot-level
// problem at a wrong layer.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("jobs: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(buf)
}

// cloneMeta returns a shallow copy of m (nil-safe).
func cloneMeta(m Meta) Meta {
	if m == nil {
		return nil
	}
	out := make(Meta, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeCancel returns a context that cancels when either parent cancels.
// Equivalent to Go 1.26's context.AfterFunc composition but inlined to
// avoid a goroutine per job when only one parent can cancel (the common
// case: caller passes context.Background()).
func mergeCancel(parent1, parent2 context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent2)
	stop := context.AfterFunc(parent1, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

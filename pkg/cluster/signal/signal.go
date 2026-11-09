// Package signal carries node operations over coordinator-initiated requests.
package signal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Hold bounds a parked poll. Clients allow additional transport slack.
const Hold = 25 * time.Second
const maxPending = 64

// ErrStopped refuses operations that require a live coordinator exchange.
var ErrStopped = errors.New("node exchange is stopped")

// Kind identifies a node operation, never an arbitrary remotely supplied route.
type Kind string

const (
	// Catalog requests publication of this node's inventory.
	Catalog Kind = "catalog"
	// ProviderEnabled requests a coordinator-owned node enable choice.
	ProviderEnabled Kind = "provider_enabled"
	// Goodbye reports graceful departure.
	Goodbye Kind = "goodbye"
	// Renew requests a certificate for the existing node key.
	Renew Kind = "renew"
)

// Operation remains pending until acknowledged or its caller cancels.
type Operation struct {
	ID       string `json:"id"`
	Kind     Kind   `json:"kind"`
	Provider string `json:"provider,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
	CSR      string `json:"csr,omitempty"`
}

// StatusError preserves the coordinator's refusal status and code.
type StatusError struct {
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("coordinator operation: status %d: %s", e.Status, e.Detail)
}

// Result acknowledges one operation on its original node incarnation.
type Result struct {
	ID          string       `json:"id"`
	Error       *StatusError `json:"error,omitempty"`
	Certificate string       `json:"certificate,omitempty"`
}

// Poll identifies operations the coordinator is already processing.
type Poll struct {
	Incarnation string   `json:"incarnation,omitempty"`
	Seen        []string `json:"seen,omitempty"`
}

// Batch is a replayable snapshot of pending operations.
type Batch struct {
	Incarnation string      `json:"incarnation"`
	Operations  []Operation `json:"operations"`
}

// Acknowledgment completes operations only on their original incarnation.
type Acknowledgment struct {
	Incarnation string   `json:"incarnation"`
	Results     []Result `json:"results"`
}

// Config contains the runtime role accessor.
type Config struct{ IsWorker func() bool }
type pending struct {
	op     Operation
	done   chan struct{}
	result Result
}

// Signaler owns node operations and their lifecycle. It never dials.
type Signaler struct {
	cfg         Config
	mu          sync.Mutex
	ctx         context.Context
	parent      context.Context
	cancel      context.CancelFunc
	incarnation string
	changed     chan struct{}
	pending     map[string]*pending
}

// New constructs a dormant exchange.
func New(cfg Config) *Signaler {
	return &Signaler{cfg: cfg, incarnation: uuid.NewString(), changed: make(chan struct{}), pending: make(map[string]*pending)}
}

// Start opens admission; starting an already running exchange is a no-op.
func (s *Signaler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		s.parent = ctx
		s.ctx, s.cancel = context.WithCancel(ctx)
	}
}

// Stop cancels callers and parked polls without spawning drain goroutines.
func (s *Signaler) Stop(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
		s.ctx = nil
		s.incarnation = uuid.NewString()
		s.pending = make(map[string]*pending)
		s.wakeLocked()
	}
}
func (s *Signaler) wakeLocked() { close(s.changed); s.changed = make(chan struct{}) }

// Wait returns unseen operations, or an empty batch after normal hold expiry.
func (s *Signaler) Wait(ctx context.Context, poll Poll) (Batch, error) {
	waitCtx, cancel := context.WithTimeout(ctx, Hold)
	defer cancel()
	for {
		s.mu.Lock()
		batch := Batch{Incarnation: s.incarnation, Operations: []Operation{}}
		if s.ctx == nil {
			s.mu.Unlock()
			return batch, errors.New("node exchange is stopped")
		}
		life := s.ctx
		seen := make(map[string]bool, len(poll.Seen))
		if poll.Incarnation == s.incarnation {
			for _, id := range poll.Seen {
				seen[id] = true
			}
		}
		for id, p := range s.pending {
			if !seen[id] {
				batch.Operations = append(batch.Operations, p.op)
			}
		}
		changed := s.changed
		s.mu.Unlock()
		if len(batch.Operations) > 0 {
			return batch, nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return batch, ctx.Err()
			}
			return batch, nil
		case <-life.Done():
			return batch, life.Err()
		case <-changed:
		}
	}
}

// Acknowledge is idempotent and refuses results for older incarnations.
func (s *Signaler) Acknowledge(ack Acknowledgment) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ack.Incarnation != s.incarnation {
		return false
	}
	for _, r := range ack.Results {
		if p := s.pending[r.ID]; p != nil {
			p.result = r
			delete(s.pending, r.ID)
			close(p.done)
		}
	}
	s.wakeLocked()
	return true
}
func (s *Signaler) submit(ctx context.Context, op Operation) (Result, error) {
	s.mu.Lock()
	if s.ctx == nil || s.cfg.IsWorker == nil || !s.cfg.IsWorker() {
		s.mu.Unlock()
		return Result{}, ErrStopped
	}
	life := s.ctx
	if life.Err() != nil {
		s.mu.Unlock()
		return Result{}, life.Err()
	}
	if len(s.pending) >= maxPending {
		s.mu.Unlock()
		return Result{}, &StatusError{Status: 503, Code: "node_exchange_busy", Detail: "node exchange is at capacity"}
	}
	op.ID = uuid.NewString()
	p := &pending{op: op, done: make(chan struct{})}
	s.pending[op.ID] = p
	s.wakeLocked()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, op.ID); s.wakeLocked(); s.mu.Unlock() }()
	select {
	case <-p.done:
		if p.result.Error != nil {
			return p.result, p.result.Error
		}
		return p.result, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-life.Done():
		return Result{}, life.Err()
	}
}

// NotifyCacheRefresh waits for publication, rather than just event delivery.
func (s *Signaler) NotifyCacheRefresh(ctx context.Context) error {
	s.mu.Lock()
	active := s.ctx != nil && s.cfg.IsWorker != nil && s.cfg.IsWorker()
	s.mu.Unlock()
	if !active {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 33*time.Second)
	defer cancel()
	_, err := s.submit(ctx, Operation{Kind: Catalog})
	return err
}

// NotifyCacheRefreshAsync records a best-effort notice without a new goroutine.
func (s *Signaler) NotifyCacheRefreshAsync(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil || s.cfg.IsWorker == nil || !s.cfg.IsWorker() || len(s.pending) >= maxPending {
		return
	}
	op := Operation{ID: uuid.NewString(), Kind: Catalog}
	s.pending[op.ID] = &pending{op: op, done: make(chan struct{})}
	s.wakeLocked()
}

// RecordProviderEnabled waits until the coordinator has delivered the config.
func (s *Signaler) RecordProviderEnabled(ctx context.Context, provider string, enabled bool) error {
	_, err := s.submit(ctx, Operation{Kind: ProviderEnabled, Provider: provider, Enabled: enabled})
	return err
}

// SendGoodbye gives an existing poll a bounded opportunity to observe departure.
func (s *Signaler) SendGoodbye(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _ = s.submit(ctx, Operation{Kind: Goodbye})
}

// RenewCertificate waits for a signed certificate without opening a connection.
func (s *Signaler) RenewCertificate(ctx context.Context, csr string) (string, error) {
	r, err := s.submit(ctx, Operation{Kind: Renew, CSR: csr})
	return r.Certificate, err
}

// Reset discards a prior cluster's operations and wakes their callers.
func (s *Signaler) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.ctx, s.cancel = context.WithCancel(s.parent)
	}
	s.incarnation = uuid.NewString()
	s.pending = make(map[string]*pending)
	s.wakeLocked()
}

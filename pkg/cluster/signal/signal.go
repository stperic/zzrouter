// Package signal owns worker→coordinator cross-node calls. Workers send
// a cache-refresh ping after any config mutation (providers changed,
// models changed), a goodbye POST during graceful shutdown so the coord
// marks the endpoint DOWN without waiting on the liveness hysteresis,
// and their own provider enable choices, which only the coordinator's
// provider tree may hold.
//
// Kept separate from pkg/model/cache so the cache stays a data layer
// (catalog + indexes + lifecycle) and dispatch plumbing lives with
// the rest of the cluster-networking code.
package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// StatusError is a coordinator refusal, kept whole so the caller can relay
// its status and closed code rather than flatten it.
type StatusError struct {
	Status int
	Code   string
	Detail string
}

func (e *StatusError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("coord returned status %d", e.Status)
	}
	return fmt.Sprintf("coord returned status %d: %s", e.Status, e.Detail)
}

// errCoordNotWired means the caller can't reach the coordinator yet —
// pre-pairing, pre-Start, or mid-demote. NotifyCacheRefresh uses it to
// short-circuit the retry loop; SendGoodbye silently swallows it.
var errCoordNotWired = errors.New("coordinator dispatch not wired")

// Config bundles Signaler dependencies.
type Config struct {
	// IsWorker gates both Notify and Goodbye — only workers signal the
	// coord. Wrapped as a function so role transitions flip observed
	// behavior without a Signaler rebuild.
	IsWorker func() bool
	// MTLSClient returns the worker's mTLS dispatch client (coordinator
	// OU). Required; nil returns short-circuit the calls.
	MTLSClient func() *http.Client
	// CoordinatorURL returns the paired coordinator's cluster-port URL
	// (https://host:cluster-port, read from clusterDir at pairing).
	CoordinatorURL func() string
	// WorkerPublicURL returns this worker's public URL as the coord has
	// it registered. Sent in the goodbye payload so the coord can mark
	// exactly this worker DOWN.
	WorkerPublicURL func() string
}

// Signaler emits worker→coord notifications. Lifecycle-gated via
// Start/Stop so in-flight notifications drain before server shutdown.
type Signaler struct {
	cfg Config

	lifecycleMu sync.Mutex
	lifeCtx     context.Context
	lifeCancel  context.CancelFunc
	notifyWG    sync.WaitGroup
}

// New constructs a Signaler from Config.
func New(cfg Config) *Signaler {
	return &Signaler{cfg: cfg}
}

// Start installs the lifecycle ctx. Idempotent — a second Start without
// an intervening Stop is a no-op.
func (s *Signaler) Start(ctx context.Context) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.lifeCancel != nil {
		return
	}
	s.lifeCtx, s.lifeCancel = context.WithCancel(ctx)
}

// Stop cancels the lifecycle ctx and waits for in-flight notifications
// to exit. Bounded by ctx. Safe to call repeatedly.
func (s *Signaler) Stop(ctx context.Context) {
	s.lifecycleMu.Lock()
	cancel := s.lifeCancel
	s.lifeCtx = nil
	s.lifeCancel = nil
	s.lifecycleMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()

	done := make(chan struct{})
	go func() {
		s.notifyWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("signal stop deadline exceeded", "err", ctx.Err())
	}
}

// NotifyCacheRefresh waits for coordinator catalog publication with
// bounded retries. No-op on coordinator nodes or pre-Start. Tracked
// via the wait group so Stop blocks until in-flight notifications exit.
//
// The body carries {"node_url": workerURL} so the coord can narrow
// its refresh to just this sender instead of broadcasting to every
// endpoint. Empty body is still accepted on the coord side for
// compatibility with pre-narrowing workers.
func (s *Signaler) NotifyCacheRefresh(ctx context.Context) error {
	if s.cfg.IsWorker == nil || !s.cfg.IsWorker() {
		return nil
	}
	s.lifecycleMu.Lock()
	lifeCtx := s.lifeCtx
	if lifeCtx == nil {
		s.lifecycleMu.Unlock()
		return nil
	}
	s.notifyWG.Add(1)
	s.lifecycleMu.Unlock()
	defer s.notifyWG.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(lifeCtx, cancel) //nolint:contextcheck // bounds the caller's ctx by the signaler's lifetime so Stop ends in-flight notifications
	defer stop()
	if lifeCtx.Err() != nil {
		cancel()
	}

	body := []byte("{}")
	if s.cfg.WorkerPublicURL != nil {
		if u := s.cfg.WorkerPublicURL(); u != "" {
			if b, err := json.Marshal(map[string]string{"node_url": u}); err == nil {
				body = b
			}
		}
	}

	const (
		maxRetries  = 3
		perAttempt  = 10 * time.Second
		initBackoff = 1 * time.Second
	)
	backoff := initBackoff
	var err error
	for attempt := range maxRetries {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		callCtx, cancel := context.WithTimeout(ctx, perAttempt)
		err = s.doCoordPost(callCtx, "/zzrouter/v1/internal/models/refresh", body)
		cancel()
		if err == nil {
			slog.Info("[Worker] Master cache refresh notification sent successfully")
			return nil
		}
		if errors.Is(err, errCoordNotWired) {
			slog.Debug("[Worker] Skipping master notify", "reason", err)
			return err
		}
		slog.Warn("[Worker] Master notify attempt failed", "attempt", attempt, "err", err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	slog.Warn("[Worker] Failed to notify master after attempts", "max_retries", maxRetries)
	return err
}

// SendGoodbye POSTs a synchronous goodbye to the coord so it can mark
// this worker DOWN without waiting on the 2-miss liveness hysteresis.
// Returns silently on any error; a failed goodbye just defers to the
// normal poll-based DOWN detection.
func (s *Signaler) SendGoodbye(ctx context.Context) {
	if s.cfg.IsWorker == nil || !s.cfg.IsWorker() {
		return
	}
	var workerURL string
	if s.cfg.WorkerPublicURL != nil {
		workerURL = s.cfg.WorkerPublicURL()
	}
	if workerURL == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"node_url": workerURL})
	if err != nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.doCoordPost(callCtx, "/zzrouter/v1/internal/goodbye", body); err != nil {
		slog.Info("[Worker] Goodbye notify failed", "err", err)
		return
	}
	slog.Info("[Worker] Goodbye notify acknowledged")
}

// RecordProviderEnabled asks the coordinator to record this node's enable
// choice; nil means the sync carrying it has already reached this node.
func (s *Signaler) RecordProviderEnabled(ctx context.Context, provider string, enabled bool) error {
	body, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return err
	}
	return s.doCoord(ctx, http.MethodPut, "/zzrouter/v1/internal/providers/"+url.PathEscape(provider)+"/enabled", body)
}

func (s *Signaler) doCoordPost(ctx context.Context, path string, body []byte) error {
	return s.doCoord(ctx, http.MethodPost, path, body)
}

func (s *Signaler) doCoord(ctx context.Context, method, path string, body []byte) error {
	if s.cfg.MTLSClient == nil {
		return errCoordNotWired
	}
	client := s.cfg.MTLSClient()
	if client == nil {
		return errCoordNotWired
	}
	var coord string
	if s.cfg.CoordinatorURL != nil {
		coord = s.cfg.CoordinatorURL()
	}
	if coord == "" {
		return errCoordNotWired
	}
	req, err := http.NewRequestWithContext(ctx, method, coord+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&problem)
		return &StatusError{Status: resp.StatusCode, Code: problem.Code, Detail: problem.Detail}
	}
	return nil
}

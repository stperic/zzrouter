// Package signal owns worker→coordinator cross-node notifications.
// Workers send two kinds of signal: a cache-refresh ping after any
// config mutation (providers changed, models changed) and a goodbye
// POST during graceful shutdown so the coord marks the endpoint DOWN
// without waiting on the liveness hysteresis.
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
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

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
// Start/Stop so in-flight notify goroutines drain before server shutdown.
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
	s.lifeCtx, s.lifeCancel = context.WithCancel(ctx) //nolint:gosec // cancel stored on s.lifeCancel and invoked by Stop
}

// Stop cancels the lifecycle ctx and waits for in-flight notify
// goroutines to exit. Bounded by ctx. Safe to call repeatedly.
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

// NotifyCacheRefresh fires one refresh ping to the coordinator with
// bounded retries. No-op on coordinator nodes or pre-Start. Tracked
// via the wait group so Stop blocks until in-flight goroutines exit.
//
// The body carries {"node_url": workerURL} so the coord can narrow
// its refresh to just this sender instead of broadcasting to every
// endpoint. Empty body is still accepted on the coord side for
// compatibility with pre-narrowing workers.
func (s *Signaler) NotifyCacheRefresh() {
	if s.cfg.IsWorker == nil || !s.cfg.IsWorker() {
		return
	}
	s.lifecycleMu.Lock()
	ctx := s.lifeCtx
	if ctx == nil {
		s.lifecycleMu.Unlock()
		return
	}
	s.notifyWG.Add(1)
	s.lifecycleMu.Unlock()

	body := []byte("{}")
	if s.cfg.WorkerPublicURL != nil {
		if u := s.cfg.WorkerPublicURL(); u != "" {
			if b, err := json.Marshal(map[string]string{"node_url": u}); err == nil {
				body = b
			}
		}
	}

	go func() {
		defer s.notifyWG.Done()
		defer utils.RecoverAndLog("signal.NotifyCacheRefresh")

		const (
			maxRetries  = 3
			perAttempt  = 10 * time.Second
			initBackoff = 1 * time.Second
		)
		backoff := initBackoff
		for attempt := range maxRetries {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff *= 2
			}
			callCtx, cancel := context.WithTimeout(ctx, perAttempt)
			err := s.doCoordPost(callCtx, "/zzrouter/v1/internal/models/refresh", body)
			cancel()
			if err == nil {
				slog.Info("[Worker] Master cache refresh notification sent successfully")
				return
			}
			if errors.Is(err, errCoordNotWired) {
				slog.Debug("[Worker] Skipping master notify", "reason", err)
				return
			}
			slog.Warn("[Worker] Master notify attempt failed", "attempt", attempt, "err", err)
			if ctx.Err() != nil {
				return
			}
		}
		slog.Warn("[Worker] Failed to notify master after attempts", "max_retries", maxRetries)
	}()
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

func (s *Signaler) doCoordPost(ctx context.Context, path string, body []byte) error {
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
	req, err := http.NewRequestWithContext(ctx, "POST", coord+path, bytes.NewReader(body))
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
		return fmt.Errorf("coord returned status %d", resp.StatusCode)
	}
	return nil
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/routing"
)

// Start enables observer registration after construction or promotion. A new
// group keeps a concurrent bounded Stop's wait separate from new observers.
func (s *DeploymentsService) Start() {
	s.observerMu.Lock()
	defer s.observerMu.Unlock()
	if s.observerCtx == nil || s.observerCtx.Err() != nil {
		s.observerCtx, s.observerCancel = context.WithCancel(context.Background())
		s.observerWG = &sync.WaitGroup{}
	}
}

// Stop cancels registration and bounds draining even when given Background.
func (s *DeploymentsService) Stop(ctx context.Context) {
	if s == nil {
		return
	}
	ctx, stopWait := ensureTimeout(ctx, constants.ClusterActionTimeout)
	defer stopWait()
	s.observerMu.Lock()
	cancel, wg := s.observerCancel, s.observerWG
	if cancel != nil {
		cancel()
	}
	s.observerMu.Unlock()
	if wg == nil {
		return
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("deployment observer drain interrupted", "error", ctx.Err())
	}
}

func (s *DeploymentsService) cancelJob(ctx context.Context, n DeploymentNode) {
	if n.JobID == "" {
		return
	}
	_, err := s.router.Route(ctx, &routing.Request{Method: "POST", Node: n.Node, Path: "/zzrouter/v1/internal/jobs/" + url.PathEscape(n.JobID) + "/cancel"})
	if err != nil {
		slog.Warn("failed to cancel placement job", "job", n.JobID, "error", err)
	}
}

//nolint:contextcheck // Accepted work outlives the request; observers intersect dynamic shutdown and service contexts.
func (s *DeploymentsService) acceptNode(ctx context.Context, id string, n DeploymentNode) {
	if !n.Status.IsTerminal() && n.JobID == "" {
		n.Status, n.Error = constants.StatusFailed, "worker acceptance has no job ID; outcome unconfirmed and may still be running"
	}
	if err := s.tracker.UpdateNode(id, n.Node, "", func(target *DeploymentNode) { *target = n }); err != nil {
		slog.Warn("cannot bind deployment acceptance", "deployment", id, "error", err)
		return
	}
	d := s.tracker.GetDeployment(id)
	for _, target := range d.Nodes {
		if target.Node != n.Node {
			continue
		}
		if d.Status == constants.StatusCancelled || target.Status == constants.StatusCancelled {
			s.cancelJob(ctx, target)
			return
		}
		if target.Status.IsTerminal() {
			return
		}
	}
	s.observerMu.Lock()
	if s.jobs == nil || s.observerCtx == nil || s.observerCtx.Err() != nil {
		s.observerMu.Unlock()
		s.trackingFailed(id, n, fmt.Errorf("deployment observer unavailable"))
		return
	}
	parent := context.Background()
	if s.shutdown != nil {
		parent = s.shutdown()
	}
	observerCtx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.observerCtx, cancel)
	wg := s.observerWG
	wg.Add(1)
	s.observerMu.Unlock()
	go func() {
		defer wg.Done()
		defer cancel()
		defer stop()
		err := s.jobs.Observe(observerCtx, n.JobID, n.Node, func(ev jobs.Event) error {
			kind := jobs.KindDownload
			if n.Source != "internet" {
				kind = jobs.KindSync
			}
			if ev.Kind != kind {
				return fmt.Errorf("unexpected placement job kind %s", ev.Kind)
			}
			// The producer sidecar preserves fractional progress, speed and
			// physical cancellation independently from jobs' generic phase.
			var transfer struct {
				Progress *float64         `json:"transfer_progress"`
				Speed    int64            `json:"transfer_speed"`
				Status   constants.Status `json:"transfer_status"`
			}
			raw, err := json.Marshal(ev.Meta)
			if err != nil {
				return fmt.Errorf("encode transfer metadata: %w", err)
			}
			if err := json.Unmarshal(raw, &transfer); err != nil {
				return fmt.Errorf("decode transfer metadata: %w", err)
			}
			return s.tracker.UpdateNode(id, n.Node, n.JobID, func(target *DeploymentNode) {
				if ev.Bytes != nil {
					target.BytesDownloaded, target.BytesTotal = ev.Bytes.Done, ev.Bytes.Total
				}
				target.Progress = float64(ev.Percent)

				if transfer.Progress != nil {
					target.Progress = *transfer.Progress
				}
				target.Speed = transfer.Speed
				if target.StartedAt.IsZero() || ev.At.Before(target.StartedAt) {
					target.StartedAt = ev.At
				}
				if ev.Phase == jobs.PhaseDone {
					target.Status, target.Progress, target.CompletedAt = constants.StatusCompleted, 100, ev.At
				}
				if ev.Phase == jobs.PhaseFailed {
					target.Status, target.Error, target.CompletedAt = constants.StatusFailed, ev.Err, ev.At
					if transfer.Status == constants.StatusCancelled {
						target.Status = constants.StatusCancelled
					}
				}
			})
		})
		if err != nil {
			s.trackingFailed(id, n, err)
		}
	}()
}

func (s *DeploymentsService) trackingFailed(id string, n DeploymentNode, err error) {
	_ = s.tracker.UpdateNode(id, n.Node, n.JobID, func(target *DeploymentNode) {
		target.Status, target.Error = constants.StatusFailed, "placement tracking failed: worker outcome unconfirmed and may still be running: "+err.Error()
	})
}

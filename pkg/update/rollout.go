package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils/clock"
)

// ErrNodeUnavailable leaves an offline node pending until it reconnects.
var ErrNodeUnavailable = errors.New("update node is unavailable")

const rolloutInterval = 5 * time.Second
const rolloutApplyTimeout = 30 * time.Minute
const rolloutHistoryLimit = 20

// RolloutBackend executes version-only commands through the node transport.
type RolloutBackend interface {
	Status(context.Context, string) (UpdateStatus, error)
	Apply(context.Context, string, string) (string, error)
}

// NodeUpdate is one serial rollout target.
type NodeUpdate struct {
	Node                string     `json:"node"`
	State               string     `json:"state"`
	FromVersion         string     `json:"from_version,omitempty"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	OperationID         string     `json:"operation_id,omitempty"`
	PreviousOperationID string     `json:"previous_operation_id,omitempty"`
	Error               string     `json:"error,omitempty"`
}

// Rollout is a persisted update job with the coordinator ordered last.
type Rollout struct {
	JobID   string       `json:"job_id"`
	Version string       `json:"version"`
	State   string       `json:"state"`
	Nodes   []NodeUpdate `json:"nodes"`
}

// Rollouts owns one serial loop and a bounded record of update jobs.
type Rollouts struct {
	mu               sync.Mutex
	path, local      string
	backend          RolloutBackend
	jobs             *jobs.Registry
	clock            clock.Clock
	records          []Rollout
	handle           jobs.Handle
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	started, stopped bool
}

// NewRollouts loads the existing record without executing commands.
func NewRollouts(path, local string, backend RolloutBackend, registry *jobs.Registry, clk clock.Clock) (*Rollouts, error) {
	if path == "" || local == "" || backend == nil || registry == nil || clk == nil {
		return nil, fmt.Errorf("update rollout requires path, node, backend, jobs and clock")
	}
	r := &Rollouts{path: path, local: local, backend: backend, jobs: registry, clock: clk}
	if err := readJSON(path, &r.records); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load update rollouts: %w", err)
	}
	if len(r.records) > rolloutHistoryLimit {
		return nil, fmt.Errorf("update rollout history exceeds limit")
	}
	for i, record := range r.records {
		if err := ValidateVersion(record.Version); err != nil {
			return nil, err
		}
		if record.JobID == "" || len(record.Nodes) == 0 || len(record.Nodes) > 64 || (i > 0 && record.State == "running") {
			return nil, fmt.Errorf("invalid persisted update rollout")
		}
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r, nil
}

// Start resumes the active job and binds observation to node shutdown.
func (r *Rollouts) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return nil
	}
	if len(r.records) > 0 && r.records[0].State == "running" && r.handle == nil {
		h, err := r.jobs.Resume(ctx, jobs.KindUpdate, "", r.records[0].JobID, jobs.Meta{"version": r.records[0].Version, "nodes": r.records[0].Nodes})
		if err != nil {
			return err
		}
		r.handle = h
	}
	context.AfterFunc(ctx, r.cancel)
	r.started = true
	r.wg.Add(1)
	go r.loop()
	return nil
}

// Stop stops observation without cancelling the persisted rollout.
func (r *Rollouts) Stop() { r.mu.Lock(); r.stopped = true; r.cancel(); r.mu.Unlock(); r.wg.Wait() }

// Submit persists before any node command, refusing concurrent rollouts.
func (r *Rollouts) Submit(target string, nodes []string) (Rollout, error) {
	if err := ValidateVersion(target); err != nil {
		return Rollout{}, err
	}
	if len(nodes) == 0 || len(nodes) > 64 {
		return Rollout{}, fmt.Errorf("select between 1 and 64 nodes")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return Rollout{}, context.Canceled
	}
	if len(r.records) > 0 && r.records[0].State == "running" {
		return Rollout{}, ErrApplyInFlight
	}
	ordered := slices.Clone(nodes)
	slices.Sort(ordered)
	for i, n := range ordered {
		if n == "" || (i > 0 && n == ordered[i-1]) {
			return Rollout{}, fmt.Errorf("node names must be unique and nonempty")
		}
	}
	if i := slices.Index(ordered, r.local); i >= 0 {
		ordered = append(slices.Delete(ordered, i, i+1), r.local)
	}
	h, err := r.jobs.StartDetached(jobs.KindUpdate, "", jobs.Meta{"version": target})
	if err != nil {
		return Rollout{}, err
	}
	record := Rollout{JobID: h.ID(), Version: target, State: "running"}
	for _, n := range ordered {
		record.Nodes = append(record.Nodes, NodeUpdate{Node: n, State: "pending"})
	}
	previous := r.records
	r.records = append([]Rollout{record}, r.records...)
	if len(r.records) > rolloutHistoryLimit {
		r.records = r.records[:rolloutHistoryLimit]
	}
	if err := r.save(); err != nil {
		r.records = previous
		h.Fail(err)
		return Rollout{}, err
	}
	r.handle = h
	h.Meta(jobs.Meta{"nodes": cloneRollout(record).Nodes})
	return cloneRollout(record), nil
}

// Cancel persists the decision before acknowledging cancellation. Accepted node work may finish.
func (r *Rollouts) Cancel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.records, func(v Rollout) bool { return v.JobID == id })
	if i < 0 {
		return jobs.ErrNotFound
	}
	if r.records[i].State != "running" {
		return nil
	}
	v, err := r.changeLocked(id, func(v *Rollout) {
		v.State = "cancelled"
		for j := range v.Nodes {
			if v.Nodes[j].State == "pending" {
				v.Nodes[j].State = "skipped"
			}
		}
	})
	if err == nil && r.handle != nil {
		r.handle.Meta(jobs.Meta{"nodes": v.Nodes})
		r.handle.Fail(context.Canceled)
	}
	return err
}

// History returns immutable per-node snapshots, newest first.
func (r *Rollouts) History() []Rollout {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Rollout, len(r.records))
	for i, v := range r.records {
		out[i] = cloneRollout(v)
	}
	return out
}

func cloneRollout(v Rollout) Rollout {
	v.Nodes = slices.Clone(v.Nodes)
	for i := range v.Nodes {
		if v.Nodes[i].StartedAt != nil {
			t := *v.Nodes[i].StartedAt
			v.Nodes[i].StartedAt = &t
		}
	}
	return v
}

func (r *Rollouts) save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0750); err != nil {
		return err
	}
	return writeJSONAtomic(r.path, r.records, 0600)
}

func (r *Rollouts) change(id string, fn func(*Rollout)) (Rollout, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changeLocked(id, fn)
}

func (r *Rollouts) changeLocked(id string, fn func(*Rollout)) (Rollout, error) {
	i := slices.IndexFunc(r.records, func(v Rollout) bool { return v.JobID == id && v.State == "running" })
	if i < 0 {
		return Rollout{}, context.Canceled
	}
	previous := cloneRollout(r.records[i])
	fn(&r.records[i])
	if err := r.save(); err != nil {
		r.records[i] = previous
		return Rollout{}, err
	}
	return cloneRollout(r.records[i]), nil
}

func (r *Rollouts) loop() {
	defer r.wg.Done()
	ticker := r.clock.NewTicker(rolloutInterval)
	defer ticker.Stop()
	for {
		r.step(r.ctx)
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.Chan():
		}
	}
}

func (r *Rollouts) step(ctx context.Context) {
	r.mu.Lock()
	if len(r.records) == 0 || r.records[0].State != "running" {
		r.mu.Unlock()
		return
	}
	record, h := cloneRollout(r.records[0]), r.handle
	r.mu.Unlock()
	if h == nil || ctx.Err() != nil {
		return
	}
	if h.Context().Err() != nil {
		_ = r.Cancel(record.JobID)
		return
	}
	h.Meta(jobs.Meta{"nodes": record.Nodes, "version": record.Version})
	h.Progress(0, "observing nodes", jobs.Bytes{})
	i := slices.IndexFunc(record.Nodes, func(n NodeUpdate) bool { return n.State != "succeeded" })
	if i < 0 {
		if v, err := r.change(record.JobID, func(v *Rollout) { v.State = "succeeded" }); err == nil {
			h.Meta(jobs.Meta{"nodes": v.Nodes})
			h.Done()
		}
		return
	}
	r.observeNode(ctx, record, i, h)
}

func (r *Rollouts) observeNode(ctx context.Context, record Rollout, i int, h jobs.Handle) {
	node := record.Nodes[i]
	observation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, err := r.backend.Status(observation, node.Node)
	if ctx.Err() != nil || h.Context().Err() != nil {
		return
	}
	if errors.Is(err, ErrNodeUnavailable) {
		if node.StartedAt != nil && r.clock.Since(*node.StartedAt) > rolloutApplyTimeout {
			r.fail(record.JobID, i, "node did not return healthy after update", h)
		}
		return
	}
	if err != nil {
		r.fail(record.JobID, i, err.Error(), h)
		return
	}
	if status.ConfirmationError != "" {
		r.fail(record.JobID, i, status.ConfirmationError, h)
		return
	}
	run := status.Operation
	currentRun := run != nil && run.ToVersion == record.Version && run.JobID != "" && (run.JobID == node.OperationID || (node.OperationID == "" && run.JobID != node.PreviousOperationID))
	if node.State == "applying" && currentRun && run.Finished() && !run.Success {
		r.fail(record.JobID, i, run.Error, h)
		return
	}
	if MatchesVersion(status.CurrentVersion, record.Version) && status.PendingConfirm == nil {
		if _, err := r.change(record.JobID, func(v *Rollout) { v.Nodes[i].State = "succeeded" }); err != nil {
			h.Progress(0, "retrying rollout persistence: "+err.Error(), jobs.Bytes{})
		}
		return
	}
	if node.State == "pending" {
		changed, err := r.prepareNode(record.JobID, i, status)
		if err != nil {
			h.Progress(0, "retrying rollout persistence: "+err.Error(), jobs.Bytes{})
			return
		}
		node = changed.Nodes[i]
	} else if currentRun {
		if status.RestartRequired {
			r.fail(record.JobID, i, status.RestartRequiredReason, h)
			return
		}
		if node.StartedAt != nil && r.clock.Since(*node.StartedAt) > rolloutApplyTimeout {
			r.fail(record.JobID, i, "target version was not confirmed before timeout", h)
		}
		return
	} else if run != nil && !run.Finished() && run.JobID != node.PreviousOperationID {
		r.fail(record.JobID, i, "another update operation owns this node", h)
		return
	}
	r.dispatch(observation, record.JobID, i, node.Node, record.Version, h)
}

func (r *Rollouts) prepareNode(id string, i int, status UpdateStatus) (Rollout, error) {
	started := r.clock.Now().UTC()
	return r.change(id, func(v *Rollout) {
		v.Nodes[i].State = "applying"
		v.Nodes[i].StartedAt = &started
		if status.Operation != nil {
			v.Nodes[i].PreviousOperationID = status.Operation.JobID
		}
		if status.CurrentVersion != nil {
			v.Nodes[i].FromVersion = status.CurrentVersion.String()
		}
	})
}

// dispatch serializes acceptance with durable cancellation; lost replies reconcile by ID.
func (r *Rollouts) dispatch(ctx context.Context, id string, i int, node, target string, h jobs.Handle) {
	r.mu.Lock()
	if len(r.records) == 0 || r.records[0].JobID != id || r.records[0].State != "running" || ctx.Err() != nil || h.Context().Err() != nil {
		r.mu.Unlock()
		return
	}
	operationID, err := r.backend.Apply(ctx, node, target)
	if err == nil {
		_, err = r.changeLocked(id, func(v *Rollout) { v.Nodes[i].OperationID = operationID })
		r.mu.Unlock()
		if err != nil {
			h.Progress(0, "retrying rollout persistence: "+err.Error(), jobs.Bytes{})
		}
		return
	}
	r.mu.Unlock()
	if !errors.Is(err, ErrNodeUnavailable) && ctx.Err() == nil {
		r.fail(id, i, err.Error(), h)
	}
}

func (r *Rollouts) fail(id string, i int, reason string, h jobs.Handle) {
	if reason == "" {
		reason = "node update failed"
	}
	v, err := r.change(id, func(v *Rollout) {
		v.State = "failed"
		v.Nodes[i].State = "failed"
		v.Nodes[i].Error = reason
		for j := i + 1; j < len(v.Nodes); j++ {
			v.Nodes[j].State = "skipped"
		}
	})
	if err != nil {
		h.Progress(0, "retrying rollout persistence: "+err.Error(), jobs.Bytes{})
		return
	}
	h.Meta(jobs.Meta{"nodes": v.Nodes})
	h.Fail(errors.New(reason))
}

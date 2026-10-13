package modelregistry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// DeploymentNode is the per-node progress slice of a Deployment.
type DeploymentNode struct {
	Node            string           `json:"node"`
	Status          constants.Status `json:"status"`
	Source          string           `json:"source"` // "internet" or node name
	DownloadID      string           `json:"download_id,omitempty"`
	JobID           string           `json:"job_id,omitempty"` // pkg/jobs handle; subscribe via /zzrouter/v1/jobs/:id/stream?node=<node>
	BytesDownloaded int64            `json:"bytes_downloaded"`
	BytesTotal      int64            `json:"bytes_total"`
	Progress        float64          `json:"progress"` // 0-100
	Speed           int64            `json:"speed"`    // bytes per second
	Error           string           `json:"error,omitempty"`
	StartedAt       time.Time        `json:"started_at"`
	CompletedAt     time.Time        `json:"completed_at"`
}

// Deployment represents a tracked multi-node model-placement job. It carries
// the per-node progress and the aggregate status that drives the public API.
type Deployment struct {
	RestartJobID string           `json:"restart_job_id,omitempty"`
	ID           string           `json:"id"`
	Model        string           `json:"model"`
	Format       string           `json:"format"`
	Registry     string           `json:"registry"`
	File         string           `json:"file,omitempty"` // Specific file for GGUF
	Status       constants.Status `json:"status"`
	Message      string           `json:"message"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`

	// Nodes carries the per-node placement slice.
	Nodes []DeploymentNode `json:"nodes"`

	// Summary counters maintained by UpdateSummary.
	NodesTotal      int `json:"nodes_total"`
	NodesComplete   int `json:"nodes_complete"`
	NodesFailed     int `json:"nodes_failed"`
	NodesSkipped    int `json:"nodes_skipped"`
	NodesPending    int `json:"nodes_pending"`
	NodesInProgress int `json:"nodes_in_progress"`
}

// SetRestartJobID records the job waiting for placement and restart readiness.
func (t *DeploymentTracker) SetRestartJobID(id, jobID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}
	d.RestartJobID = jobID
	return nil
}

// UpdateSummary recalculates the node summary counts from Nodes.
func (d *Deployment) UpdateSummary() {
	d.NodesTotal = len(d.Nodes)
	d.NodesComplete = 0
	d.NodesFailed = 0
	d.NodesSkipped = 0
	d.NodesPending = 0
	d.NodesInProgress = 0

	for _, n := range d.Nodes {
		switch n.Status {
		case constants.StatusCompleted:
			d.NodesComplete++
		case constants.StatusFailed, constants.StatusCancelled:
			d.NodesFailed++
		case constants.StatusSkipped:
			d.NodesSkipped++
		case constants.StatusPending:
			d.NodesPending++
		default:
			d.NodesInProgress++
		}
	}

	if d.Status != constants.StatusCancelled && len(d.Nodes) > 0 {
		switch {
		case d.NodesPending+d.NodesInProgress > 0:
			d.Status, d.Message = constants.StatusDownloading, fmt.Sprintf("Downloading on %d/%d nodes", d.NodesPending+d.NodesInProgress, d.NodesTotal)
		case d.NodesFailed == d.NodesTotal:
			d.Status, d.Message = constants.StatusFailed, "All nodes failed"
		case d.NodesFailed > 0:
			d.Status, d.Message = constants.StatusFailed, fmt.Sprintf("%d/%d nodes failed", d.NodesFailed, d.NodesTotal)
		case d.NodesSkipped == d.NodesTotal:
			d.Status, d.Message = constants.StatusCompleted, fmt.Sprintf("All %d nodes skipped", d.NodesTotal)
		default:
			d.Status, d.Message = constants.StatusCompleted, fmt.Sprintf("All %d nodes complete", d.NodesTotal)
		}
	}
	d.UpdatedAt = utils.Now()
}

// IsComplete returns true if the deployment has reached a terminal state.
func (d *Deployment) IsComplete() bool {
	return d.Status.IsTerminal()
}

// terminalRetention is how long a finished deployment stays queryable. Long
// enough that a client polling after a completion still sees the result,
// short enough that a long-lived coordinator doesn't accumulate every
// deployment it ever ran — cloud registrations reach terminal immediately,
// so the map grows at the rate deployments are requested.
const terminalRetention = 30 * time.Minute

// DeploymentTracker tracks active deployments in memory. A finished
// deployment stays queryable for terminalRetention and is then evicted on
// the next write, so the public API can show recent results without the map
// growing without bound.
type DeploymentTracker struct {
	mu          sync.RWMutex
	deployments map[string]*Deployment
	changed     chan struct{}
}

// evictExpiredLocked drops finished deployments past their retention. It
// runs on create rather than on a timer: the map only grows when something
// is created, so that is the moment the sweep is owed, and a tracker nobody
// writes to needs no goroutine watching it. Caller must hold t.mu.
func (t *DeploymentTracker) evictExpiredLocked() {
	cutoff := utils.Now().Add(-terminalRetention)
	for id, d := range t.deployments {
		if d.Status.IsTerminal() && d.UpdatedAt.Before(cutoff) {
			delete(t.deployments, id)
		}
	}
}

// NewDeploymentTracker creates an empty tracker.
func NewDeploymentTracker() *DeploymentTracker {
	return &DeploymentTracker{
		deployments: make(map[string]*Deployment),
		changed:     make(chan struct{}),
	}
}

// generateShortID generates a short random ID (8 hex chars). The crypto/rand
// failure fallback prevents a non-nil error from reaching callers — an
// essentially cosmetic uniqueness is fine for job IDs.
func generateShortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", utils.Now().UnixNano()&0xFFFFFFFF)
	}
	return hex.EncodeToString(b)
}

// CreateDeployment registers a new multi-node deployment with the given targets.
func (t *DeploymentTracker) CreateDeployment(model, format, repo, file string, targets []string) *Deployment {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.evictExpiredLocked()

	id := fmt.Sprintf("deploy_%s", generateShortID())
	now := utils.Now()

	d := &Deployment{
		ID:        id,
		Model:     model,
		Format:    format,
		Registry:  repo,
		File:      file,
		Status:    constants.StatusPending,
		Message:   "Deployment created, waiting to start",
		CreatedAt: now,
		UpdatedAt: now,
		Nodes:     make([]DeploymentNode, len(targets)),
	}

	for i, target := range targets {
		d.Nodes[i] = DeploymentNode{
			Node:   target,
			Status: constants.StatusPending,
			Source: "internet",
		}
	}

	d.UpdateSummary()
	d.Status, d.Message = constants.StatusPending, "Deployment created, waiting to start"
	t.deployments[id] = d

	return cloneDeployment(d)
}

// GetDeployment returns a deep-copy snapshot of the deployment, or nil.
func (t *DeploymentTracker) GetDeployment(id string) *Deployment {
	t.mu.RLock()
	defer t.mu.RUnlock()

	d, ok := t.deployments[id]
	if !ok {
		return nil
	}
	// Return a copy to prevent races with later mutations.
	return cloneDeployment(d)
}

// ListDeployments returns deep-copy snapshots of every tracked deployment.
// When activeOnly is true, terminal deployments are filtered out.
func (t *DeploymentTracker) ListDeployments(activeOnly bool) []*Deployment {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var result []*Deployment
	for _, d := range t.deployments {
		if activeOnly && d.IsComplete() {
			continue
		}
		result = append(result, cloneDeployment(d))
	}

	return result
}

// UpdateNode commits a node outcome and aggregation together. A nonempty
// jobID guards an observer update against another attempt. Terminal outcomes
// are sealed; late acceptance may only bind identity for cancellation.
func (t *DeploymentTracker) UpdateNode(id, node, jobID string, update func(*DeploymentNode)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.deployments[id]
	if d == nil {
		return fmt.Errorf("deployment not found: %s", id)
	}
	for i := range d.Nodes {
		n := &d.Nodes[i]
		if n.Node != node {
			continue
		}
		if jobID != "" && n.JobID != jobID {
			return fmt.Errorf("deployment job identity mismatch on %s", node)
		}
		next := *n
		update(&next)
		if next.Node != n.Node || (n.JobID != "" && (next.JobID != n.JobID || next.Source != n.Source || next.DownloadID != n.DownloadID)) {
			return fmt.Errorf("deployment ownership cannot change on %s", node)
		}
		if n.Status.IsTerminal() {
			if n.JobID != "" || next.JobID == "" {
				return nil
			}
			n.JobID, n.DownloadID, n.Source = next.JobID, next.DownloadID, next.Source
		} else {
			if next.StartedAt.IsZero() && next.Status != constants.StatusPending {
				next.StartedAt = utils.Now()
			}
			if next.Status.IsTerminal() && next.CompletedAt.IsZero() {
				next.CompletedAt = utils.Now()
			}
			*n = next
		}
		d.UpdateSummary()
		t.notifyLocked()
		return nil
	}
	return fmt.Errorf("node not found in deployment: %s", node)
}

// CancelDeployment records intent before any network cancellation. Pending
// acceptances can subsequently bind their immutable job for cancellation.
func (t *DeploymentTracker) CancelDeployment(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.deployments[id]
	if d == nil {
		return fmt.Errorf("deployment not found: %s", id)
	}
	d.Status, d.Message = constants.StatusCancelled, "Deployment cancelled by user"
	for i := range d.Nodes {
		if !d.Nodes[i].Status.IsTerminal() {
			d.Nodes[i].Status, d.Nodes[i].Error = constants.StatusCancelled, "cancelled by user"
			d.Nodes[i].CompletedAt = utils.Now()
		}
	}
	d.UpdateSummary()
	t.notifyLocked()
	return nil
}

// WaitNode uses the retained placement outcome, independent of worker-job TTL.
// Reading state and capturing notification under the same lock avoids a lost
// completion between the read and wait.
func (t *DeploymentTracker) WaitNode(ctx context.Context, id, node string) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t.mu.RLock()
		d := t.deployments[id]
		changed := t.changed
		var target *DeploymentNode
		if d != nil {
			for _, n := range d.Nodes {
				if n.Node == node || (node == "" && len(d.Nodes) == 1) {
					target = &n
					break
				}
			}
		}
		cancelled := d != nil && d.Status == constants.StatusCancelled
		t.mu.RUnlock()
		if target == nil {
			return fmt.Errorf("deployment %s has no retained target %q", id, node)
		}
		if cancelled || target.Status.IsTerminal() {
			if !cancelled && target.Status == constants.StatusCompleted {
				return nil
			}
			return fmt.Errorf("deployment on %s ended %s: %s", target.Node, target.Status, target.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (t *DeploymentTracker) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func cloneDeployment(d *Deployment) *Deployment {
	dc := *d
	dc.Nodes = append([]DeploymentNode(nil), d.Nodes...)
	return &dc
}

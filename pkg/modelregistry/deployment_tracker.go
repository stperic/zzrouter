package modelregistry

import (
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
		case constants.StatusFailed:
			d.NodesFailed++
		case constants.StatusSkipped:
			d.NodesSkipped++
		case constants.StatusPending:
			d.NodesPending++
		case constants.StatusDownloading:
			d.NodesInProgress++
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
	t.deployments[id] = d

	return d
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
	dc := *d
	dc.Nodes = make([]DeploymentNode, len(d.Nodes))
	copy(dc.Nodes, d.Nodes)
	return &dc
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
		dc := *d
		dc.Nodes = make([]DeploymentNode, len(d.Nodes))
		copy(dc.Nodes, d.Nodes)
		result = append(result, &dc)
	}

	return result
}

// UpdateDeploymentStatus sets the overall deployment status.
func (t *DeploymentTracker) UpdateDeploymentStatus(id string, status constants.Status, message string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}

	d.Status = status
	d.Message = message
	d.UpdatedAt = utils.Now()

	return nil
}

// UpdateNodeStatus sets the status of a specific node within a deployment.
// StartedAt and CompletedAt are derived from status transitions so callers
// don't need to pass timestamps.
func (t *DeploymentTracker) UpdateNodeStatus(id, node string, status constants.Status, errMsg string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}

	for i, n := range d.Nodes {
		if n.Node == node {
			d.Nodes[i].Status = status
			d.Nodes[i].Error = errMsg

			if status == constants.StatusDownloading {
				if d.Nodes[i].StartedAt.IsZero() {
					d.Nodes[i].StartedAt = utils.Now()
				}
			}

			if status == constants.StatusCompleted || status == constants.StatusFailed {
				d.Nodes[i].CompletedAt = utils.Now()
			}

			d.UpdateSummary()
			return nil
		}
	}

	return fmt.Errorf("node not found in deployment: %s", node)
}

// UpdateNodeProgress updates byte/speed counters for a specific node.
func (t *DeploymentTracker) UpdateNodeProgress(id, node string, bytesDownloaded, bytesTotal, speed int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}

	for i, n := range d.Nodes {
		if n.Node == node {
			d.Nodes[i].BytesDownloaded = bytesDownloaded
			d.Nodes[i].BytesTotal = bytesTotal
			d.Nodes[i].Speed = speed

			if bytesTotal > 0 {
				d.Nodes[i].Progress = float64(bytesDownloaded) / float64(bytesTotal) * 100
			}

			d.UpdatedAt = utils.Now()
			return nil
		}
	}

	return fmt.Errorf("node not found in deployment: %s", node)
}

// UpdateNodeDownloadID persists the per-node download ID returned by the
// internal deploy call so cancel flows can locate the remote download later.
func (t *DeploymentTracker) UpdateNodeDownloadID(id, node, downloadID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}

	for i, n := range d.Nodes {
		if n.Node == node {
			d.Nodes[i].DownloadID = downloadID
			d.UpdatedAt = utils.Now()
			return nil
		}
	}

	return fmt.Errorf("node not found in deployment: %s", node)
}

// UpdateNodeJobID persists the pkg/jobs handle ID reported by the worker's
// internal deploy path. Subscribers watch progress via
// /zzrouter/v1/jobs/:id/stream?node=<node>.
func (t *DeploymentTracker) UpdateNodeJobID(id, node, jobID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}

	for i, n := range d.Nodes {
		if n.Node == node {
			d.Nodes[i].JobID = jobID
			d.UpdatedAt = utils.Now()
			return nil
		}
	}

	return fmt.Errorf("node not found in deployment: %s", node)
}

// UpdateNodeSource persists the per-node download source — either
// "internet" for registry pulls or the name of a peer node when the
// worker synced via /sync/deploy. Surfaces in the deployments snapshot
// so the TUI can label rows with the origin of each transfer.
func (t *DeploymentTracker) UpdateNodeSource(id, node, source string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.deployments[id]
	if !ok {
		return fmt.Errorf("deployment not found: %s", id)
	}

	for i, n := range d.Nodes {
		if n.Node == node {
			d.Nodes[i].Source = source
			d.UpdatedAt = utils.Now()
			return nil
		}
	}

	return fmt.Errorf("node not found in deployment: %s", node)
}

// CancelDeployment marks a deployment as cancelled.
func (t *DeploymentTracker) CancelDeployment(id string) error {
	return t.UpdateDeploymentStatus(id, constants.StatusCancelled, "Deployment cancelled by user")
}

// CleanCompletedDeployments evicts terminal deployments older than maxAge
// and returns the number removed.
func (t *DeploymentTracker) CleanCompletedDeployments(maxAge time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := utils.Now().Add(-maxAge)
	cleaned := 0

	for id, d := range t.deployments {
		if d.IsComplete() && d.UpdatedAt.Before(cutoff) {
			delete(t.deployments, id)
			cleaned++
		}
	}

	return cleaned
}

// GetDeploymentByModel returns an active deployment for the given model if any.
func (t *DeploymentTracker) GetDeploymentByModel(model string) *Deployment {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, d := range t.deployments {
		if d.Model == model && !d.IsComplete() {
			dc := *d
			dc.Nodes = make([]DeploymentNode, len(d.Nodes))
			copy(dc.Nodes, d.Nodes)
			return &dc
		}
	}

	return nil
}

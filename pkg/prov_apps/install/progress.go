package install

import (
	"errors"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Install lifecycle statuses.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// InstallProgress tracks the current state of an install operation.
// Safe for concurrent read/write via internal mutex.
type InstallProgress struct {
	mu sync.RWMutex

	Provider   string    `json:"provider"`
	Action     string    `json:"action"` // "install", "upgrade", "uninstall"
	Status     string    `json:"status"` // "running", "completed", "failed"
	Step       int       `json:"step"`
	TotalSteps int       `json:"total_steps"`
	StepDesc   string    `json:"step_description"`
	BytesTotal int64     `json:"bytes_total,omitempty"`
	BytesDone  int64     `json:"bytes_done,omitempty"`
	Percent    int       `json:"percent"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	JobID      string    `json:"job_id,omitempty"`

	// handle optionally mirrors state changes onto a pkg/jobs stream.
	// Attached by the caller (InstallCoordinator) via SetJobHandle after
	// Start. Nil-safe: every Set* method no-ops the emit when unset.
	handle jobs.Handle `json:"-"`

	// Last values actually published to handle, for the download
	// coalescing in SetDownloadProgress. State above is always current;
	// these only gate the fan-out.
	lastEmitPct int
	lastEmitAt  time.Time
}

// downloadProgressMinInterval is how often a download still publishes
// while its percentage has not moved. Byte-level progress is reported
// once per read of the HTTP body, so a single provider install put
// ~49 000 frames on its job stream carrying 77 distinct percent values,
// out of ~65 000 / 18 MB for the whole install. A subscriber needs to
// know the download is alive and roughly where it is; it does not need
// every read. Kept short enough that a stalled download is still
// visibly ticking.
const downloadProgressMinInterval = time.Second

// SetJobHandle attaches a jobs.Handle so every subsequent Set* call
// also emits a progress/done/failed event onto the SSE stream. Safe to
// call multiple times; nil clears the attachment. Sets JobID from the
// handle so status snapshots carry it for clients switching from REST
// polling to SSE subscribe.
func (p *InstallProgress) SetJobHandle(h jobs.Handle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handle = h
	if h != nil {
		p.JobID = h.ID()
	} else {
		p.JobID = ""
	}
}

// Snapshot returns a read-consistent copy of the progress state.
func (p *InstallProgress) Snapshot() InstallProgress {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return InstallProgress{
		Provider:   p.Provider,
		Action:     p.Action,
		Status:     p.Status,
		Step:       p.Step,
		TotalSteps: p.TotalSteps,
		StepDesc:   p.StepDesc,
		BytesTotal: p.BytesTotal,
		BytesDone:  p.BytesDone,
		Percent:    p.Percent,
		Error:      p.Error,
		StartedAt:  p.StartedAt,
		UpdatedAt:  p.UpdatedAt,
		JobID:      p.JobID,
	}
}

// SetStep updates the current step info.
func (p *InstallProgress) SetStep(step, total int, desc string) {
	p.mu.Lock()
	p.Step = step
	p.TotalSteps = total
	p.StepDesc = desc
	if total > 0 {
		p.Percent = step * 100 / total
	}
	p.BytesTotal = 0
	p.BytesDone = 0
	// A new step means a new download, so do not let the previous one's
	// last published percent suppress this one's first update.
	p.lastEmitPct = -1
	p.lastEmitAt = time.Time{}
	p.UpdatedAt = utils.Now()
	h := p.handle
	pct := p.Percent
	p.mu.Unlock()
	if h != nil {
		// Carry step number + total in Meta so SSE subscribers can
		// correlate without re-parsing desc. The wizard TUI uses this
		// to drive its per-step status row.
		h.Meta(jobs.Meta{"step": step, "total_steps": total})
		h.Progress(pct, desc, jobs.Bytes{})
	}
}

// SetDownloadProgress updates byte-level download progress within the current step.
func (p *InstallProgress) SetDownloadProgress(done, total int64, pct int) {
	p.mu.Lock()
	p.BytesDone = done
	p.BytesTotal = total
	p.Percent = pct
	now := utils.Now()
	p.UpdatedAt = now
	h := p.handle
	desc := p.StepDesc
	// Coalesce the fan-out only. Snapshot() keeps reporting every byte,
	// so REST pollers and the final state are unaffected; what is bounded
	// is how many frames a subscriber has to read.
	emit := pct != p.lastEmitPct || now.Sub(p.lastEmitAt) >= downloadProgressMinInterval
	if emit {
		p.lastEmitPct = pct
		p.lastEmitAt = now
	}
	p.mu.Unlock()
	if h != nil && emit {
		h.Progress(pct, desc, jobs.Bytes{Done: done, Total: total})
	}
}

// SetCompleted marks the install as done. Clears any stale Error text
// so snapshots don't advertise a prior failure that's no longer relevant.
func (p *InstallProgress) SetCompleted() {
	p.mu.Lock()
	p.Status = StatusCompleted
	p.Percent = 100
	p.Error = ""
	p.UpdatedAt = utils.Now()
	h := p.handle
	p.mu.Unlock()
	if h != nil {
		h.Done()
	}
}

// SetFailed marks the install as failed with an error.
func (p *InstallProgress) SetFailed(err string) {
	p.mu.Lock()
	p.Status = StatusFailed
	p.Error = err
	p.UpdatedAt = utils.Now()
	h := p.handle
	p.mu.Unlock()
	if h != nil {
		if err == "" {
			h.Fail(errors.New("install failed"))
		} else {
			h.Fail(errors.New(err))
		}
	}
}

// ProgressTracker stores install progress per provider.
type ProgressTracker struct {
	mu       sync.RWMutex
	progress map[string]*InstallProgress
}

// NewProgressTracker creates a new tracker.
func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		progress: make(map[string]*InstallProgress),
	}
}

// Start begins tracking a new install operation, returning the progress handle.
func (t *ProgressTracker) Start(provider, action string, totalSteps int) *InstallProgress {
	p := &InstallProgress{
		Provider:   provider,
		Action:     action,
		Status:     StatusRunning,
		TotalSteps: totalSteps,
		StartedAt:  utils.Now(),
		UpdatedAt:  utils.Now(),
	}
	t.mu.Lock()
	t.progress[provider] = p
	t.mu.Unlock()
	return p
}

// Get returns the current progress for a provider, or nil if none.
func (t *ProgressTracker) Get(provider string) *InstallProgress {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.progress[provider]
}

// Remove deletes tracking data for a provider.
func (t *ProgressTracker) Remove(provider string) {
	t.mu.Lock()
	delete(t.progress, provider)
	t.mu.Unlock()
}

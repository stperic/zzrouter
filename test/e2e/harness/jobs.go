package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// defaultJobTerminalTimeout covers the slowest provider install today
// (vLLM cold pip on a CUDA worker can run ~12 min). 30 min leaves
// margin for the next layer of slowness without hiding genuine hangs.
const defaultJobTerminalTimeout = 30 * time.Minute

// JobOutcome is the terminal state of a tracked job plus the SSE
// trail. Status mirrors pkg/jobs.Phase: "done" (success) or "failed".
// pkg/jobs has no PhaseCancelled today; cancellation surfaces as
// phase=failed with an Error string.
type JobOutcome struct {
	ID            string
	Status        string // "done" | "failed"
	Error         string
	Events        []JobEvent
	DroppedEvents int // count of events_dropped frames seen
	Duration      time.Duration
	RawSSE        []byte // diagnostic bytes for failure dumps
}

// Succeeded is the canonical pass check for tests that don't care
// about the exact phase string.
func (o JobOutcome) Succeeded() bool { return o.Status == "done" }

// JobEvent is a forward-compatible DTO mirroring pkg/jobs.Event. Known
// fields are typed; unknown fields are preserved in Extra so server-side
// schema additions don't silently lose data.
type JobEvent struct {
	JobID   string `json:"job_id,omitempty"`
	Node    string `json:"node,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Epoch   string `json:"epoch,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	At      string `json:"at,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"err,omitempty"`
	// Dropped is set on events_dropped marker frames.
	Dropped *DroppedMarker `json:"dropped,omitempty"`
	// Extra preserves unknown fields for forward compat.
	Extra map[string]json.RawMessage `json:"-"`
}

// DroppedMarker mirrors pkg/jobs.DroppedMarker.
type DroppedMarker struct {
	Since   uint64 `json:"since"`
	Current uint64 `json:"current"`
}

// jobEnvelope is the standard SuccessResponse shape returned by
// GET /jobs/:id (response_helpers.go::SuccessResponse).
type jobEnvelope struct {
	Success bool     `json:"success"`
	Message string   `json:"message"`
	Data    JobEvent `json:"data"`
}

// ErrJobUnknownOrEvicted means /jobs/:id/stream returned 404. The
// underlying registry can't distinguish "never existed" from "aged
// past CompletedTTL" — both map to ErrNotFound. Tests that need to
// disambiguate should probe GET /jobs/:id beforehand.
var ErrJobUnknownOrEvicted = errors.New("job unknown or evicted")

// ErrJobStreamError signals a stream-level failure event from the
// server (encode error, transport hiccup) — distinct from a job that
// terminated with phase=failed.
var ErrJobStreamError = errors.New("job stream error")

// Jobs is the harness's job-stream consumer.
type Jobs struct {
	c       *Client
	timeout time.Duration
}

// NewJobs builds a Jobs helper bound to a Client. Pass an admin-tier
// client for /zzrouter/v1/jobs/:id/stream; cluster auth is the
// internal flavor (see WaitInternal).
func NewJobs(c *Client, jobTerminalTimeout time.Duration) *Jobs {
	if jobTerminalTimeout <= 0 {
		jobTerminalTimeout = defaultJobTerminalTimeout
	}
	return &Jobs{c: c, timeout: jobTerminalTimeout}
}

// Wait opens GET /zzrouter/v1/jobs/:id/stream and reads SSE frames
// until a terminal phase. Returns ErrJobUnknownOrEvicted on 404.
// Stream-without-terminal followed by EOF triggers a single
// GET /jobs/:id fallback (catches the eviction-during-stream race).
func (j *Jobs) Wait(ctx context.Context, jobID string) (JobOutcome, error) {
	return j.wait(ctx, "/zzrouter/v1/jobs/", jobID)
}

// WaitInternal is the cluster-internal flavor.
func (j *Jobs) WaitInternal(ctx context.Context, jobID string) (JobOutcome, error) {
	return j.wait(ctx, "/zzrouter/v1/internal/jobs/", jobID)
}

// Run launches a job via the supplied callback (which must POST and
// return the job_id) and waits for terminal state.
func (j *Jobs) Run(ctx context.Context, launch func() (string, error)) (JobOutcome, error) {
	id, err := launch()
	if err != nil {
		return JobOutcome{}, fmt.Errorf("launch: %w", err)
	}
	return j.Wait(ctx, id)
}

func (j *Jobs) wait(parent context.Context, prefix, jobID string) (JobOutcome, error) {
	if jobID == "" {
		return JobOutcome{}, errors.New("empty job id")
	}
	ctx, cancel := context.WithTimeout(parent, j.timeout)
	defer cancel()

	start := utils.Now()
	streamURL := j.c.node.baseURL + prefix + url.PathEscape(jobID) + "/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return JobOutcome{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	j.c.applyAuth(req, "")

	resp, err := j.c.http.Do(req)
	if err != nil {
		return JobOutcome{}, fmt.Errorf("stream %s: %w", jobID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return JobOutcome{}, fmt.Errorf("%w: %s", ErrJobUnknownOrEvicted, jobID)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return JobOutcome{}, fmt.Errorf("stream %s: status %d", jobID, resp.StatusCode)
	}

	out := JobOutcome{ID: jobID}
	var raw bytes.Buffer
	var streamErr error
	terminal := false

	err = TailSSE(ctx, resp, func(f SSEFrame) bool {
		raw.Write(f.Raw)

		switch f.Event {
		case "error":
			// Stream-level transport / encode failure. Surface as ErrJobStreamError;
			// distinct from a job that legitimately terminated with phase=failed.
			streamErr = fmt.Errorf("%w: %s", ErrJobStreamError, f.Data)
			return true
		case "stream_closed":
			// Clean non-terminal close — fall through to fallback GET.
			return true
		case "events_dropped":
			out.DroppedEvents++
			// Decode anyway so Dropped marker lands in Events for diagnostics.
		}

		if f.Data == "" {
			return false
		}
		var ev JobEvent
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
			// Malformed data on a typed event — record but keep tailing.
			return false
		}
		out.Events = append(out.Events, ev)

		// Discriminate terminal via PHASE, not event name. The server
		// emits `event: done` for every terminal transition regardless
		// of phase; the actual outcome lives in the JSON payload.
		if f.Event == "done" {
			out.Status = ev.Phase
			out.Error = ev.Error
			terminal = true
			return true
		}
		return false
	})
	out.RawSSE = raw.Bytes()
	out.Duration = time.Since(start)

	if streamErr != nil {
		return out, streamErr
	}
	if err != nil {
		return out, err
	}
	if !terminal {
		return j.fallbackGet(ctx, prefix, jobID, out, start)
	}
	return out, nil
}

func (j *Jobs) fallbackGet(ctx context.Context, prefix, jobID string, partial JobOutcome, start time.Time) (JobOutcome, error) {
	getURL := j.c.node.baseURL + prefix + url.PathEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return partial, err
	}
	j.c.applyAuth(req, "")

	resp, err := j.c.http.Do(req)
	if err != nil {
		return partial, fmt.Errorf("fallback get %s: %w", jobID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return partial, fmt.Errorf("%w: %s", ErrJobUnknownOrEvicted, jobID)
	}
	if resp.StatusCode != http.StatusOK {
		return partial, fmt.Errorf("fallback get %s: status %d", jobID, resp.StatusCode)
	}

	var env jobEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return partial, fmt.Errorf("decode fallback envelope: %w", err)
	}
	partial.Events = append(partial.Events, env.Data)
	partial.Duration = time.Since(start)

	switch env.Data.Phase {
	case "done", "failed":
		partial.Status = env.Data.Phase
		partial.Error = env.Data.Error
		return partial, nil
	default:
		return partial, fmt.Errorf("job %s closed without terminal phase (got %q)", jobID, env.Data.Phase)
	}
}

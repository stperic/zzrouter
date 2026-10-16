package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/jobs"
)

// JobEvent is the wire envelope decoded from each SSE data frame on
// /zzrouter/v1/jobs/:id/stream. Mirrors pkg/jobs.Event (keep in sync if
// the server's shape grows fields).
type JobEvent struct {
	JobID   string         `json:"job_id"`
	Node    string         `json:"node"`
	Kind    string         `json:"kind"`
	Epoch   string         `json:"epoch"`
	Seq     uint64         `json:"seq"`
	At      time.Time      `json:"at"`
	Phase   string         `json:"phase"`
	Percent int            `json:"percent,omitempty"`
	Step    string         `json:"step,omitempty"`
	Bytes   *JobEventBytes `json:"bytes,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
	Err     string         `json:"err,omitempty"`
	Dropped *JobEventDrop  `json:"dropped,omitempty"`

	Warning string `json:"warning,omitempty"`

	// Event is the SSE event name (progress | done | error |
	// stream_closed | events_dropped). Populated by SubscribeJob from
	// the frame's `event:` line; not present in the wire JSON.
	Event string `json:"-"`
}

// JobEventBytes mirrors pkg/jobs.Bytes.
type JobEventBytes struct {
	Done  int64 `json:"done,omitempty"`
	Total int64 `json:"total,omitempty"`
}

// JobEventDrop mirrors pkg/jobs.DroppedMarker.
type JobEventDrop struct {
	Since   uint64 `json:"since"`
	Current uint64 `json:"current"`
}

// IsTerminal reports whether this event closes the subscription.
func (e JobEvent) IsTerminal() bool {
	return e.Phase == JobPhaseDone || e.Phase == JobPhaseFailed
}

// SSE event names emitted by the server (see jobs_controller.serveStream).
const (
	JobEventProgress      = "progress"
	JobEventDone          = "done"
	JobEventError         = "error"
	JobEventStreamClosed  = "stream_closed"
	JobEventEventsDropped = "events_dropped"
)

// Terminal phases a job reports. The phase is what settles the outcome:
// the event name can be misreported, but a job that says "failed" failed.
const (
	JobPhaseDone   = "done"
	JobPhaseFailed = "failed"
)

// ErrJobEpochMismatch is returned by SubscribeJob when the registry's
// epoch changed between subscribes (e.g. node restart). Clients should
// drop any ?from/?epoch cursor and resubscribe fresh.
var ErrJobEpochMismatch = errors.New("job stream: epoch mismatch (resubscribe)")

// SubscribeJob opens an SSE stream at /zzrouter/v1/jobs/:id/stream and
// invokes onEvent for every frame (progress | done | error |
// stream_closed | events_dropped). Returns the final terminal event
// (done or failed) on clean completion, nil if the server closed with
// stream_closed without a terminal, or an error on network/protocol
// failure. ctx cancellation aborts the read and returns ctx.Err().
//
// node may be empty (local / coord-owned job) or a nodename/alias/URL
// to trigger the coord's cross-node proxy.
func (c *Client) SubscribeJob(ctx context.Context, jobID, node string, onEvent func(JobEvent)) (*JobEvent, error) {
	if jobID == "" {
		return nil, errors.New("SubscribeJob: empty jobID")
	}

	params := url.Values{}
	if node != "" {
		params.Set("node", node)
	}
	path := apipath.JobStream(jobID)
	if enc := params.Encode(); enc != "" {
		path += "?" + enc
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("SubscribeJob: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.newStreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("SubscribeJob: dial: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusConflict:
		return nil, ErrJobEpochMismatch
	case http.StatusNotFound:
		return nil, fmt.Errorf("SubscribeJob: job %q not found", jobID)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("SubscribeJob: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var terminal *JobEvent
	err = jobs.ReadSSE(resp.Body, func(name string, raw []byte) (bool, error) {
		var ev JobEvent
		if json.Unmarshal(raw, &ev) == nil {
			ev.Event = name
			if onEvent != nil {
				onEvent(ev)
			}
			if ev.IsTerminal() && ev.Dropped == nil {
				terminal = &ev
			}
		}
		return terminal != nil, nil
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("SubscribeJob: read: %w", err)
	}
	return terminal, nil
}

// CancelJob fires DELETE /zzrouter/v1/jobs/:id. Per server contract,
// cancel is non-blocking: the stream is authoritative for the final
// outcome. accepted is false for a 404 (job already terminal / unknown).
func (c *Client) CancelJob(ctx context.Context, jobID string) (accepted bool, err error) {
	if jobID == "" {
		return false, errors.New("CancelJob: empty jobID")
	}
	path := apipath.Job(jobID)
	resp, err := c.DoRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return false, fmt.Errorf("CancelJob: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("CancelJob: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// ListJobs returns the job snapshots held by one node. An empty node
// queries whichever node this client points at; a non-empty node asks the
// coordinator to proxy to that peer.
//
// Unlike GetJob, the server's list endpoint is node-local and does not fan
// out across the cluster — it reports its own registry. ListAllJobs layers
// the fan-out on top.
func (c *Client) ListJobs(ctx context.Context, node string) ([]JobEvent, error) {
	path := apipath.Jobs
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	resp, err := c.DoRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("ListJobs: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ListJobs: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// GET /jobs answers in the collection envelope every list endpoint on
	// the management surface uses: the array at `data`, the owning node
	// under `metadata.node`. The shape is the same whether the jobs are
	// local or came from ?node=<peer>.
	var env struct {
		Data     []JobEvent `json:"data"`
		Metadata struct {
			Node string `json:"node"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("ListJobs: decode: %w", err)
	}
	// The per-event Node field is authoritative when present; fall back to
	// the envelope so a row that does not name its owner still gets one.
	for i := range env.Data {
		if env.Data[i].Node == "" {
			env.Data[i].Node = env.Metadata.Node
		}
	}
	return env.Data, nil
}

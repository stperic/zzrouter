package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// InferenceLogEntry represents a single inference log entry from the server.
type InferenceLogEntry struct {
	ID              string    `json:"id"`
	Timestamp       time.Time `json:"timestamp"`
	Model           string    `json:"model"`
	App             string    `json:"provider"`
	RequestType     string    `json:"request_type"`
	Stream          bool      `json:"stream"`
	RoutingDecision string    `json:"routing_decision"`
	Node            string    `json:"node"`
	Status          string    `json:"status"`
	SystemPrompt    string    `json:"system_prompt,omitempty"`
	Messages        []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages,omitempty"`
	Response        string   `json:"response,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxTokens       *int     `json:"max_tokens,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TokensIn        int64    `json:"tokens_in"`
	TokensOut       int64    `json:"tokens_out"`
	TokensCached    int64    `json:"tokens_cached,omitempty"`
	TokensReasoning int64    `json:"tokens_reasoning,omitempty"`
	TokensPerSec    float64  `json:"tokens_per_sec"`
	TTFTMs          float64  `json:"ttft_ms"`
	LatencyMs       float64  `json:"latency_ms"`
	Cost            float64  `json:"cost,omitempty"`
	ErrorType       string   `json:"error_type,omitempty"`
	ErrorMessage    string   `json:"error_message,omitempty"`

	// PayloadAvailable reports that the full request bodies are still
	// retained server-side and fetchable via GetInferenceLogPayload.
	PayloadAvailable bool `json:"payload_available,omitempty"`
}

// InferenceLogPayload carries the request bodies retained for one entry.
type InferenceLogPayload struct {
	Request  json.RawMessage `json:"request,omitempty"`
	Upstream json.RawMessage `json:"upstream_request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
	Elided   []ElidedBlob    `json:"elided,omitempty"`
	Oversize bool            `json:"oversize,omitempty"`
}

// ElidedBlob describes attached media the server replaced with a
// placeholder rather than retaining as base64.
type ElidedBlob struct {
	Path  string `json:"path"`
	Media string `json:"media,omitempty"`
	Bytes int    `json:"bytes"`
}

// InferenceLogsResponse is the response from GET /inference-logs.
type InferenceLogsResponse struct {
	Data    []InferenceLogEntry `json:"data"`
	Total   int                 `json:"total"`
	HasMore bool                `json:"has_more"`
}

// GetInferenceLogs retrieves inference log entries from the server.
func (c *Client) GetInferenceLogs(model, status, since string, limit int) ([]InferenceLogEntry, error) {
	params := url.Values{}
	if model != "" {
		params.Set("model", model)
	}
	if status != "" {
		params.Set("status", status)
	}
	if since != "" {
		params.Set("since", since)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}

	path := apipath.InferenceLogs
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var result InferenceLogsResponse
	if err := c.doJSON("GET", path, nil, &result, "get inference logs"); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// GetInferenceLog retrieves a single inference log entry by ID.
func (c *Client) GetInferenceLog(id string) (*InferenceLogEntry, error) {
	var envelope struct {
		Data InferenceLogEntry `json:"data"`
	}
	if err := c.doJSON("GET", apipath.InferenceLog(id), nil, &envelope, "get inference log"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// GetInferenceLogPayload retrieves the retained request bodies for an
// entry. Bodies are kept for a shorter window than the entries
// themselves, so this 404s once a payload has aged out.
func (c *Client) GetInferenceLogPayload(id string) (*InferenceLogPayload, error) {
	var envelope struct {
		Data InferenceLogPayload `json:"data"`
	}
	path := apipath.InferenceLogPayload(id)
	if err := c.doJSON("GET", path, nil, &envelope, "get inference log payload"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// jobEventEnvelope is the jobs-stream SSE payload shape. Only the fields
// this client needs are decoded; the full Event struct lives in pkg/jobs.
type jobEventEnvelope struct {
	JobID string         `json:"job_id"`
	Kind  string         `json:"kind"`
	Phase string         `json:"phase"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// StreamInferenceLogs opens an SSE connection that tails inference logs in
// real time by subscribing to the singleton inference_log jobs firehose at
// /zzrouter/v1/jobs/:id/stream. Blocks until ctx is cancelled or the
// stream ends. The handler runs on each entry that passes the model
// filter (empty = no filter).
func (c *Client) StreamInferenceLogs(ctx context.Context, model string, handler func(InferenceLogEntry)) error {
	jobID, err := c.findInferenceLogJobID(ctx)
	if err != nil {
		return err
	}

	reqURL := c.baseURL + apipath.JobStream(jobID)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create stream request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.newStreamingClient().Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to jobs stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jobs stream returned status %d", resp.StatusCode)
	}

	modelFilter := strings.ToLower(model)
	return parseInferenceLogSSE(ctx, resp.Body, modelFilter, handler)
}

// findInferenceLogJobID locates the singleton inference_log firehose job
// by listing jobs filtered by kind. The server opens exactly one such job
// at startup (InferenceLogBridge.SetJobHandle), so the first entry is
// authoritative.
func (c *Client) findInferenceLogJobID(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+apipath.Jobs+"?kind=inference_log", nil)
	if err != nil {
		return "", fmt.Errorf("build jobs list request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("list inference_log jobs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list inference_log jobs: status %d", resp.StatusCode)
	}

	// Collection envelope: the array sits at `data`.
	var env struct {
		Data []struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", fmt.Errorf("decode jobs list: %w", err)
	}
	if len(env.Data) == 0 {
		return "", fmt.Errorf("no inference_log job registered on server")
	}
	return env.Data[0].JobID, nil
}

// parseInferenceLogSSE reads SSE frames, unwraps Meta["entry"] out of each
// progress/done envelope, applies the model filter, and invokes handler.
// Returns nil on clean terminal (done / stream_closed), the SSE error on
// server-reported failure, or the read error on transport failure.
func parseInferenceLogSSE(ctx context.Context, body interface {
	Read(p []byte) (int, error)
}, modelFilter string, handler func(InferenceLogEntry)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventName string
	var dataBuf strings.Builder

	dispatch := func() {
		defer func() {
			eventName = ""
			dataBuf.Reset()
		}()
		if eventName != "progress" && eventName != "done" {
			return
		}
		var env jobEventEnvelope
		if err := json.Unmarshal([]byte(dataBuf.String()), &env); err != nil {
			return // malformed frame — skip, don't kill the stream
		}
		raw, ok := env.Meta["entry"]
		if !ok {
			return
		}
		// Re-marshal the any back to JSON so we can decode it into the
		// typed struct. Direct type assertion won't work because
		// json.Unmarshal decoded it as map[string]interface{}.
		buf, err := json.Marshal(raw)
		if err != nil {
			return
		}
		var entry InferenceLogEntry
		if err := json.Unmarshal(buf, &entry); err != nil {
			return
		}
		if modelFilter != "" && !strings.EqualFold(entry.Model, modelFilter) {
			return
		}
		handler(entry)
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil //nolint:nilerr // the caller cancelled its own tail; that is the stop signal, not a failure
		}
		line := scanner.Text()
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, ":"):
			// SSE comment / keep-alive; ignore
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if eventName == "done" || eventName == "stream_closed" {
			dispatch()
			return nil
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("stream read error: %w", err)
	}
	return nil
}

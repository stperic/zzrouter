package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/apipath"
)

// DefaultTailLines is the initial tail size used by RunSession and is
// the recommended default for callers constructing their own
// FetchTail calls.
const DefaultTailLines = 250

// TailResult is the decoded payload from a non-follow logs fetch.
type TailResult struct {
	// Lines is the slice of log lines in file order (oldest first,
	// newest last). Length is bounded by the requested limit and the
	// actual file size.
	Lines []string

	// TotalLines is the server's report of the total lines in the
	// file. Useful as an upper bound when skipping replayed SSE
	// events.
	TotalLines int

	// Truncated reports whether the server truncated its tail scan
	// (the log file was larger than the server's threshold for a
	// full read and only the last N lines were returned).
	Truncated bool

	// LogFile is the absolute path reported by the server. Purely
	// informational.
	LogFile string

	// LastLine is Lines[len(Lines)-1] hoisted for convenience, or
	// empty if Lines is empty. RunSession uses this as the content
	// sentinel for SSE dedup.
	LastLine string
}

// FetchTail fetches the last `lines` entries of a run's log file. This
// is the non-follow form of GET /zzrouter/v1/runs/:id/logs.
//
// The endpoint responds with a JSON envelope; the server code can be
// consulted at internal/server/logs_handlers.go for the exact shape.
// We decode defensively so fields added later do not break callers.
func FetchTail(ctx context.Context, c *utilsclient.Client, runID string, lines int) (TailResult, error) {
	if runID == "" {
		return TailResult{}, fmt.Errorf("logs: FetchTail: empty run id")
	}
	if lines <= 0 {
		lines = DefaultTailLines
	}

	params := url.Values{}
	params.Set("lines", fmt.Sprintf("%d", lines))
	path := apipath.RunLogs(runID) + "?" + params.Encode()

	resp, err := c.DoRequest(ctx, "GET", path, nil)
	if err != nil {
		return TailResult{}, fmt.Errorf("logs: FetchTail: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return TailResult{}, fmt.Errorf("logs: FetchTail: status %d: %s", resp.StatusCode, string(body))
	}

	// Server envelope. The server wraps responses via respondSuccess
	// which produces {"success": bool, "message": string, "data": {...}}.
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			LogFile    string   `json:"log_file"`
			TotalLines int      `json:"total_lines"`
			Lines      []string `json:"lines"`
			Count      int      `json:"count"`
			Truncated  bool     `json:"truncated"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return TailResult{}, fmt.Errorf("logs: FetchTail: decode: %w", err)
	}

	tr := TailResult{
		Lines:      envelope.Data.Lines,
		TotalLines: envelope.Data.TotalLines,
		Truncated:  envelope.Data.Truncated,
		LogFile:    envelope.Data.LogFile,
	}
	if n := len(tr.Lines); n > 0 {
		tr.LastLine = tr.Lines[n-1]
	}
	return tr, nil
}

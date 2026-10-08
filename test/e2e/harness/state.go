package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

// stateSnapshotPath + stateRestorePath are the public-admin endpoints
// the harness drives. Plan §3.7 originally said /internal/state/* —
// reality: the public-engine /internal mount is in-process-only (see
// internalRequestOnlyMiddleware), so the contract surface lives on the
// public admin path. Both endpoints round-trip a gzipped tar.
const (
	stateSnapshotPath = "/zzrouter/v1/state/snapshot"
	stateRestorePath  = "/zzrouter/v1/state/restore"

	stateContentType = "application/gzip"
)

// State is the harness's snapshot/restore client for a single Node.
// Backend-agnostic: works inproc + remote alike.
//
// Snapshot returns the raw gzipped-tar body — callers persist it as
// .tar.gz, hand it to Restore on another node, or pass it straight
// into the diagnostics tarball as state-snapshot.tar.gz.
type State struct {
	c *Client
}

// NewState binds a State to an admin-tier Client. The endpoints are
// admin-key gated; passing a non-admin tier will get rejected.
func NewState(c *Client) *State {
	if c == nil {
		panic("harness: NewState nil client")
	}
	return &State{c: c}
}

// Snapshot fetches the gzipped-tar body. POST (not GET) because the
// server's snapshot handler triggers store-level flushes before tar
// emission — that's a state-mutating side effect, so POST is the
// correct verb. Returned bytes are owned by the caller.
func (s *State) Snapshot(ctx context.Context) ([]byte, error) {
	url := s.c.node.baseURL + stateSnapshotPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	s.c.applyAuth(req, "")

	resp, err := s.c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("snapshot status %d: %s", resp.StatusCode, body)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != stateContentType {
		return nil, fmt.Errorf("snapshot Content-Type %q != %q", mt, stateContentType)
	}
	return io.ReadAll(resp.Body)
}

// RestoreResult names which files the server actually wrote on
// restore. Matches the JSON envelope handleStateRestore returns.
type RestoreResult struct {
	Restored []string `json:"restored"`
}

// ErrEmptySnapshot guards against silently restoring nothing — a
// zero-byte body decodes as an empty tar with no entries, which the
// server happily accepts but tells us nothing useful.
var ErrEmptySnapshot = errors.New("empty snapshot body")

// Restore POSTs a gzipped-tar body produced by Snapshot. Returns the
// list of files written.
func (s *State) Restore(ctx context.Context, body []byte) (RestoreResult, error) {
	if len(body) == 0 {
		return RestoreResult{}, ErrEmptySnapshot
	}
	url := s.c.node.baseURL + stateRestorePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return RestoreResult{}, err
	}
	req.Header.Set("Content-Type", stateContentType)
	s.c.applyAuth(req, "")

	resp, err := s.c.http.Do(req)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return RestoreResult{}, fmt.Errorf("restore status %d: %s", resp.StatusCode, respBody)
	}

	out := RestoreResult{}
	r := Response{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: respBody}
	if err := r.JSON(&out); err != nil {
		return RestoreResult{}, fmt.Errorf("decode restore envelope: %w", err)
	}
	return out, nil
}

package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/utils"
)

// errNoEndpoint is returned by a call on a Target with no endpoint.
var errNoEndpoint = errors.New("provider has no endpoint")

// Target is where a protocol call goes: a provider's endpoint, and the
// upstream that says what it is sent. Every request a call makes is built
// through it, so a provider declaring a credential gets it on each one.
type Target struct {
	base string
	up   backend.Upstream
	// probe waits out a daemon that may still be starting before the first
	// call. A hosted API is up or it is not, and its own answer says why.
	probe bool
}

// NewTarget returns the target at endpoint (scheme://host[/prefix]; a
// trailing slash or /v1 is not part of the base) reached as up.
func NewTarget(endpoint string, up backend.Upstream) Target {
	return Target{base: config.NormalizeEndpoint(endpoint), up: up, probe: true}
}

// TargetOf returns the target a resolved provider is.
func TargetOf(r *backend.Resolved) Target {
	t := NewTarget(r.Endpoint, r.Upstream)
	t.probe = !r.Cloud
	return t
}

// Host returns the endpoint's host[:port].
func (t Target) Host() string {
	u, err := url.Parse(t.base)
	if err != nil {
		return ""
	}
	return u.Host
}

// call sends method to path on t, authenticated, with body JSON-encoded
// when non-nil, and returns the whole answer.
func (t Target) call(ctx context.Context, client *http.Client, method, path string, body any) (*utils.HTTPResponse, error) {
	if t.base == "" {
		return nil, errNoEndpoint
	}
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := (&backend.Resolved{Endpoint: t.base, Upstream: t.up}).NewRequest(ctx, method, path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	return &utils.HTTPResponse{Body: raw, StatusCode: resp.StatusCode}, nil
}

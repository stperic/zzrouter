package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeployRequest mirrors internal/server.DeployRequest (subset).
type DeployRequest struct {
	Model    string   `json:"model"`
	Nodes    []string `json:"nodes,omitempty"`
	Registry string   `json:"registry,omitempty"`
	File     string   `json:"file,omitempty"`
	Format   string   `json:"format,omitempty"`
	Force    bool     `json:"force,omitempty"`
}

// DeploymentNode mirrors pkg/modelregistry.DeploymentNode (subset).
type DeploymentNode struct {
	Node            string `json:"node"`
	Status          string `json:"status"`
	BytesDownloaded int64  `json:"bytes_downloaded,omitempty"`
	BytesTotal      int64  `json:"bytes_total,omitempty"`
	Progress        int    `json:"progress,omitempty"`
	JobID           string `json:"job_id,omitempty"`
}

// Deployment mirrors pkg/modelregistry.Deployment (subset).
type Deployment struct {
	ID            string           `json:"id"`
	Model         string           `json:"model"`
	Format        string           `json:"format,omitempty"`
	Registry      string           `json:"registry"`
	File          string           `json:"file,omitempty"`
	Status        string           `json:"status"`
	Message       string           `json:"message,omitempty"`
	Nodes         []DeploymentNode `json:"nodes"`
	NodesTotal    int              `json:"nodes_total"`
	NodesComplete int              `json:"nodes_complete"`
	NodesFailed   int              `json:"nodes_failed"`
}

type deployEnvelope struct {
	Data Deployment `json:"data"`
}

type deployListEnvelope struct {
	Data []Deployment `json:"data"`
}

// CreateDeployment POSTs /zzrouter/v1/deployments and returns the
// initial Deployment record (status often "downloading").
func CreateDeployment(ctx context.Context, c *Client, req DeployRequest) (*Deployment, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("CreateDeployment: model required")
	}
	resp, err := c.POST(ctx, "/zzrouter/v1/deployments", req)
	if err != nil {
		return nil, fmt.Errorf("POST /deployments: %w", err)
	}
	if resp.Status != 201 && resp.Status != 200 && resp.Status != 202 {
		return nil, fmt.Errorf("POST /deployments: status %d body=%s", resp.Status, resp.Body)
	}
	var env deployEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode deployment: %w", err)
	}
	return &env.Data, nil
}

// ListDeployments fetches GET /zzrouter/v1/deployments.
func ListDeployments(ctx context.Context, c *Client) ([]Deployment, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/deployments")
	if err != nil {
		return nil, fmt.Errorf("GET /deployments: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /deployments: status %d body=%s", resp.Status, resp.Body)
	}
	var env deployListEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode deployments list: %w", err)
	}
	return env.Data, nil
}

// GetDeployment fetches GET /zzrouter/v1/deployments/:id with live
// progress merged in.
func GetDeployment(ctx context.Context, c *Client, id string) (*Deployment, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/deployments/"+id)
	if err != nil {
		return nil, fmt.Errorf("GET /deployments/%s: %w", id, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("GET /deployments/%s: status %d body=%s", id, resp.Status, resp.Body)
	}
	var env deployEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode deployment: %w", err)
	}
	return &env.Data, nil
}

// CancelDeployment DELETEs /zzrouter/v1/deployments/:id. Idempotent —
// 404 = success.
func CancelDeployment(ctx context.Context, c *Client, id string) error {
	resp, err := c.DELETE(ctx, "/zzrouter/v1/deployments/"+id)
	if err != nil {
		return fmt.Errorf("DELETE /deployments/%s: %w", id, err)
	}
	if resp.Status == 200 || resp.Status == 204 || resp.Status == 404 {
		return nil
	}
	return fmt.Errorf("DELETE /deployments/%s: status %d body=%s", id, resp.Status, resp.Body)
}

// WaitDeployment polls GET /deployments/:id until status is terminal
// (completed/failed/cancelled) or ctx fires.
func WaitDeployment(ctx context.Context, c *Client, id string) (*Deployment, error) {
	var out *Deployment
	var lastErr error
	err := PollUntil(ctx, 500*time.Millisecond, func(ctx context.Context) (bool, error) {
		d, err := GetDeployment(ctx, c, id)
		if err != nil {
			lastErr = err
			return false, nil //nolint:nilerr // transient while the deployment is created; reported if we time out
		}
		if isTerminalDeployStatus(d.Status) {
			out = d
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		if lastErr != nil {
			return nil, fmt.Errorf("WaitDeployment: %w (last poll error: %v)", err, lastErr)
		}
		return nil, fmt.Errorf("WaitDeployment: %w", err)
	}
	return out, nil
}

// ErrDeploymentFailed is returned by WaitDeploymentReady when the
// deployment reaches a terminal failure status (failed/cancelled).
var ErrDeploymentFailed = fmt.Errorf("deployment terminal failure")

// WaitDeploymentReady polls until the deployment is ready to serve
// (status=ready or completed). Cloud deploys are metadata-only so they
// usually flip to ready immediately; local pulls go ready only after
// completed. Terminal failure (failed/cancelled) returns
// ErrDeploymentFailed so callers don't tick out the deadline.
func WaitDeploymentReady(ctx context.Context, c *Client, id string) (*Deployment, error) {
	var out *Deployment
	var lastErr error
	err := PollUntil(ctx, 500*time.Millisecond, func(ctx context.Context) (bool, error) {
		d, err := GetDeployment(ctx, c, id)
		if err != nil {
			lastErr = err
			return false, nil //nolint:nilerr // transient while the deployment is created; reported if we time out
		}
		switch strings.ToLower(d.Status) {
		case "ready", "completed":
			out = d
			return true, nil
		case "failed", "cancelled", "canceled":
			return false, fmt.Errorf("%w: id=%s status=%s", ErrDeploymentFailed, id, d.Status)
		}
		return false, nil
	})
	if err != nil {
		// A terminal failure is the answer, not a symptom; only a timeout wants the poll error.
		if lastErr != nil && !errors.Is(err, ErrDeploymentFailed) {
			return nil, fmt.Errorf("%w (last poll error: %v)", err, lastErr)
		}
		return nil, err
	}
	return out, nil
}

func isTerminalDeployStatus(s string) bool {
	switch strings.ToLower(s) {
	case "completed", "failed", "cancelled", "canceled":
		return true
	}
	return false
}

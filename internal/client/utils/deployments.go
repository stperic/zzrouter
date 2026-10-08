package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/constants"
)

// DeployRequest mirrors the server-side public deploy request shape.
type DeployRequest struct {
	Model    string   `json:"model"`
	Nodes    []string `json:"nodes,omitempty"`
	Registry string   `json:"registry,omitempty"`
	File     string   `json:"file,omitempty"`
	Format   string   `json:"format,omitempty"`
	Force    bool     `json:"force,omitempty"`
}

// DeploymentNode mirrors the server-side per-node progress entry inside a Deployment.
type DeploymentNode struct {
	Node            string           `json:"node"`
	Status          constants.Status `json:"status"`
	Source          string           `json:"source"`
	DownloadID      string           `json:"download_id,omitempty"`
	JobID           string           `json:"job_id,omitempty"`
	BytesDownloaded int64            `json:"bytes_downloaded"`
	BytesTotal      int64            `json:"bytes_total"`
	Progress        float64          `json:"progress"`
	Speed           int64            `json:"speed"`
	Error           string           `json:"error,omitempty"`
	StartedAt       string           `json:"started_at"`
	CompletedAt     string           `json:"completed_at"`
}

// Deployment mirrors the server-side tracked deployment entity returned by
// POST /zzrouter/v1/deployments and GET /zzrouter/v1/deployments[/:id].
type Deployment struct {
	ID              string           `json:"id"`
	Model           string           `json:"model"`
	Format          string           `json:"format"`
	Registry        string           `json:"registry"`
	File            string           `json:"file,omitempty"`
	Status          constants.Status `json:"status"`
	Message         string           `json:"message"`
	CreatedAt       string           `json:"created_at"`
	UpdatedAt       string           `json:"updated_at"`
	Nodes           []DeploymentNode `json:"nodes"`
	NodesTotal      int              `json:"nodes_total"`
	NodesComplete   int              `json:"nodes_complete"`
	NodesFailed     int              `json:"nodes_failed"`
	NodesSkipped    int              `json:"nodes_skipped"`
	NodesPending    int              `json:"nodes_pending"`
	NodesInProgress int              `json:"nodes_in_progress"`
}

// Deploy starts a deployment against the server. Empty nodes defers target
// selection to the server (valid for cloud/shared-storage; otherwise 400).
func (c *Client) Deploy(model, registry, file string, nodes []string, force bool) (*Deployment, error) {
	req := DeployRequest{
		Model:    model,
		Nodes:    nodes,
		Registry: registry,
		File:     file,
		Force:    force,
	}
	var envelope struct {
		Data Deployment `json:"data"`
	}
	if err := c.doJSON("POST", apipath.Deployments, req, &envelope, "deploy"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// ListDeployments returns every tracked deployment.
func (c *Client) ListDeployments() ([]*Deployment, error) {
	var envelope struct {
		Data    []*Deployment `json:"data"`
		Total   int           `json:"total"`
		HasMore bool          `json:"has_more"`
	}
	if err := c.doJSON("GET", apipath.Deployments, nil, &envelope, "list deployments"); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

// GetDeployment fetches a single deployment. Returns (nil, nil) on 404.
// Keeps its own plumbing because the 404-to-nil contract doesn't fit doJSON.
func (c *Client) GetDeployment(id string) (*Deployment, error) {
	resp, err := c.makeRequest("GET", apipath.Deployment(id), nil)
	if err != nil {
		return nil, fmt.Errorf("get deployment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get deployment failed with status %d: %s", resp.StatusCode, parseErrorResponse(body))
	}
	var envelope struct {
		Data Deployment `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("get deployment: decode response: %w", err)
	}
	return &envelope.Data, nil
}

// CancelDeployment cancels a single deployment and stops its downloads.
func (c *Client) CancelDeployment(id string) error {
	return c.doJSON("DELETE", apipath.Deployment(id), nil, nil, "cancel deployment")
}

// CancelDeploymentNode cancels a single node within a deployment, stopping
// just that node's download without affecting the other nodes in the job.
func (c *Client) CancelDeploymentNode(id, node string) error {
	if id == "" {
		return fmt.Errorf("deployment id is required")
	}
	if node == "" {
		return fmt.Errorf("node is required")
	}
	return c.doJSON("DELETE", apipath.DeploymentNode(id, node), nil, nil, "cancel deployment node")
}

// StopAllDeployments cancels active deployments. When node is empty it cancels
// every active deployment cluster-wide; otherwise only the slices targeting
// the named node across all active deployments.
func (c *Client) StopAllDeployments(node string) error {
	path := apipath.Deployments
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	return c.doJSON("DELETE", path, nil, nil, "stop deployments")
}

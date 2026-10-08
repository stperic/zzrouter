package client

import (
	"github.com/stperic/zzrouter/pkg/apipath"
)

// Strategy constants for model group routing (mirrors pkg/model/group.StrategyType).
const (
	StrategyPriority  = "priority"
	StrategyLeastLoad = "least-load"
	StrategyFastest   = "fastest"
)

// ModelGroupResponse is the client representation of a model group.
type ModelGroupResponse struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description,omitempty"`
	Strategy    string                     `json:"strategy"`
	HealthCheck *HealthCheckConfigResponse `json:"health_check,omitempty"`
	Replicas    []ReplicaResponse          `json:"replicas"`
	Params      map[string]any             `json:"params,omitempty"`
	AutoManaged bool                       `json:"auto_managed,omitempty"`
	AutoOwner   string                     `json:"auto_owner,omitempty"`
}

// HealthCheckConfigResponse is the client representation of health check settings.
type HealthCheckConfigResponse struct {
	Path     string `json:"path"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
}

// ReplicaResponse is the client representation of a replica within a group.
type ReplicaResponse struct {
	Name              string         `json:"name"`
	Model             string         `json:"model"`
	App               string         `json:"provider"`
	Node              string         `json:"node,omitempty"`
	Priority          int            `json:"priority"`
	Timeout           string         `json:"timeout,omitempty"`
	OnDemand          bool           `json:"on_demand,omitempty"`
	GPUMemoryRequired string         `json:"gpu_memory_required,omitempty"`
	MaxRetries        int            `json:"max_retries"`
	Tags              []string       `json:"tags,omitempty"`
	Params            map[string]any `json:"params,omitempty"`
	CooldownSeconds   float64        `json:"cooldown_seconds,omitempty"`
	CooldownReason    string         `json:"cooldown_reason,omitempty"`
}

// ModelGroupListResponse is the envelope for GET /model-groups.
type ModelGroupListResponse struct {
	Data     []ModelGroupResponse `json:"data"`
	Total    int                  `json:"total"`
	HasMore  bool                 `json:"has_more,omitempty"`
	Metadata struct {
		RoutePrefix string `json:"route_prefix"`
	} `json:"metadata"`
}

// ModelGroupDetailResponse is the envelope for GET /model-groups/:name.
type ModelGroupDetailResponse struct {
	Success bool               `json:"success"`
	Message string             `json:"message"`
	Data    ModelGroupResponse `json:"data"`
}

// ListModelGroups retrieves all model groups and the configured route prefix.
// GET /zzrouter/v1/model-groups
func (c *Client) ListModelGroups() ([]ModelGroupResponse, string, error) {
	var result ModelGroupListResponse
	if err := c.doJSON("GET", apipath.ModelGroups, nil, &result, "list model groups"); err != nil {
		return nil, "", err
	}
	return result.Data, result.Metadata.RoutePrefix, nil
}

// GetModelGroup retrieves a single model group by name.
// GET /zzrouter/v1/model-groups/:name
func (c *Client) GetModelGroup(name string) (*ModelGroupResponse, error) {
	var result ModelGroupDetailResponse
	if err := c.doJSON("GET", apipath.ModelGroup(name), nil, &result, "get model group"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// CreateOrUpdateModelGroupRequest is the input for creating/updating a model group.
type CreateOrUpdateModelGroupRequest struct {
	Description string                    `json:"description,omitempty"`
	Strategy    string                    `json:"strategy,omitempty"`
	HealthCheck *HealthCheckConfigRequest `json:"health_check,omitempty"`
	Replicas    []ReplicaRequest          `json:"replicas"`
	Params      map[string]any            `json:"params,omitempty"`
}

// HealthCheckConfigRequest is the input for health check settings.
type HealthCheckConfigRequest struct {
	Path     string `json:"path,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

// ReplicaRequest is the input for a replica within a group.
type ReplicaRequest struct {
	Name              string         `json:"name"`
	Model             string         `json:"model"`
	App               string         `json:"provider"`
	Node              string         `json:"node,omitempty"`
	Priority          int            `json:"priority"`
	Timeout           string         `json:"timeout,omitempty"`
	OnDemand          bool           `json:"on_demand,omitempty"`
	GPUMemoryRequired string         `json:"gpu_memory_required,omitempty"`
	MaxRetries        int            `json:"max_retries"`
	Tags              []string       `json:"tags,omitempty"`
	Params            map[string]any `json:"params,omitempty"`
}

// CreateOrUpdateModelGroup creates or updates a model group.
// PUT /zzrouter/v1/model-groups/:name
func (c *Client) CreateOrUpdateModelGroup(name string, req *CreateOrUpdateModelGroupRequest) (*ModelGroupResponse, error) {
	var result ModelGroupDetailResponse
	if err := c.doJSON("PUT", apipath.ModelGroup(name), req, &result, "save model group"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// DeleteModelGroup deletes a model group by name.
// DELETE /zzrouter/v1/model-groups/:name
func (c *Client) DeleteModelGroup(name string) error {
	return c.doJSON("DELETE", apipath.ModelGroup(name), nil, nil, "delete model group")
}

package client

import (
	"context"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ProviderWithFormats represents a provider with its supported formats
type ProviderWithFormats struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Formats []string `json:"formats"`
	Running bool     `json:"running"`
}

// NodeDiskInfo represents disk space information
type NodeDiskInfo struct {
	TotalGB     float64 `json:"total_gb"`
	AvailableGB float64 `json:"available_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// NodeMemoryInfo represents memory information
type NodeMemoryInfo struct {
	TotalGB     float64 `json:"total_gb"`
	AvailableGB float64 `json:"available_gb"`
	UsedPercent float64 `json:"used_percent"`
}

// NodeWithProviders represents a host with its available providers and resource info
type NodeWithProviders struct {
	Name         string                `json:"name"`
	GPU          *NodeGPUInfo          `json:"gpu,omitempty"`
	HealthStatus string                `json:"health_status,omitempty"`
	IPAddress    string                `json:"ip_address,omitempty"`
	ClusterRole  string                `json:"cluster_role,omitempty"`
	Providers    []ProviderWithFormats `json:"providers"`
	DiskInfo     *NodeDiskInfo         `json:"disk,omitempty"`
	MemoryInfo   *NodeMemoryInfo       `json:"memory,omitempty"`
}

// NodeGPUInfo preserves the inventory's separate device capacities.
type NodeGPUInfo struct {
	Count         int           `json:"count"`
	UnifiedMemory bool          `json:"unified_memory"`
	GPUs          []NodeGPUCard `json:"gpus"`
}

// NodeGPUCard keeps an absent free-memory observation distinct from zero.
type NodeGPUCard struct {
	TotalGB     float64  `json:"memory_total_gb"`
	AvailableGB *float64 `json:"memory_available_gb,omitempty"`
}

// NodeAssessment retains both positive fits and the reason evidence is unknown.
type NodeAssessment struct {
	Node                    string `json:"node"`
	Provider                string `json:"provider,omitempty"`
	UnknownCause            string `json:"unknown_cause,omitempty"`
	WeightsMiB              int64  `json:"weights_mib,omitempty"`
	ContextMode             string `json:"context_mode"`
	ContextTokens           int64  `json:"context_tokens,omitempty"`
	AffordableContextTokens int64  `json:"affordable_context_tokens,omitempty"`
	Placement               string `json:"placement,omitempty"`
	Status                  string `json:"status"`
	Reason                  string `json:"reason"`
	BudgetKind              string `json:"budget_kind,omitempty"`
	RequiredMiB             int64  `json:"required_mib,omitempty"`
	CapacityMiB             int64  `json:"capacity_mib,omitempty"`
}

// CompatibleNodes separates fit-only automatic targets from advisory evidence.
type CompatibleNodes struct {
	Data     []NodeWithProviders `json:"data"`
	Total    int                 `json:"total"`
	HasMore  bool                `json:"has_more"`
	Metadata struct {
		Assessments []NodeAssessment `json:"assessments"`
	} `json:"metadata"`
}

// GetCompatibleNodes preserves fit-only data and all capacity assessments.
func (c *Client) GetCompatibleNodes(ctx context.Context, model, repo, provider string) (*CompatibleNodes, error) {
	q := url.Values{"model": {model}, "registry": {repo}, "provider": {provider}}
	var out CompatibleNodes
	if err := c.doContextJSON(ctx, "GET", apipath.NodesCompatible+"?"+q.Encode(), nil, &out, "get compatible nodes"); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetNodeInventory lists explicit download targets without an admission filter.
func (c *Client) GetNodeInventory(ctx context.Context) ([]NodeWithProviders, error) {
	var out struct {
		Data []NodeWithProviders `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.Nodes, nil, &out, "get node inventory"); err != nil {
		return nil, err
	}
	return out.Data, nil
}

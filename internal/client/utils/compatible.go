package client

import (
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
	Name        string                `json:"name"`
	IPAddress   string                `json:"ip_address,omitempty"`
	ClusterRole string                `json:"cluster_role,omitempty"`
	Providers   []ProviderWithFormats `json:"providers"`
	DiskInfo    *NodeDiskInfo         `json:"disk,omitempty"`
	MemoryInfo  *NodeMemoryInfo       `json:"memory,omitempty"`
}

// GetCompatibleNodes retrieves nodes with compatible providers for a specific format and source
// GET /zzrouter/nodes/compatible?format=<format>&source=<source>
func (c *Client) GetCompatibleNodes(model, repo, provider string) ([]NodeWithProviders, error) {
	// Build query parameters
	params := url.Values{}
	if model != "" {
		params.Set("model_name", model)
	}
	if repo != "" {
		params.Set("registry", repo)
	}
	if provider != "" {
		params.Set("provider", provider)
	}

	path := apipath.NodesCompatible
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var response struct {
		Data   []NodeWithProviders `json:"data"`
		Format string              `json:"format"`
		Source string              `json:"source"`
	}
	if err := c.doJSON("GET", path, nil, &response, "get compatible hosts"); err != nil {
		return nil, err
	}
	return response.Data, nil
}

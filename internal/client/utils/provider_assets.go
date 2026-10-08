package client

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ProviderAsset describes one of a provider's asset files.
type ProviderAsset struct {
	Name         string   `json:"name"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
	Shipped      bool     `json:"shipped"`
	ReferencedBy []string `json:"referenced_by"`
}

// ListProviderAssets lists a provider's assets.
func (c *Client) ListProviderAssets(provider string) ([]ProviderAsset, error) {
	var out struct {
		Assets []ProviderAsset `json:"assets"`
	}
	if err := c.doJSON(http.MethodGet, apipath.ProviderAssets(provider), nil, &out, "list provider assets"); err != nil {
		return nil, err
	}
	if out.Assets == nil {
		// An empty listing is [] to a script reading -o json, not null.
		out.Assets = []ProviderAsset{}
	}
	return out.Assets, nil
}

// GetProviderAsset returns one asset's bytes.
func (c *Client) GetProviderAsset(provider, name string) ([]byte, error) {
	var data []byte
	err := c.doJSON(http.MethodGet, apipath.ProviderAsset(provider, name), nil, &data, "get provider asset")
	return data, err
}

// ProviderAssetWrite is an asset write's answer: the asset as stored, and
// what it means for the models already running.
type ProviderAssetWrite struct {
	ProviderAsset
	Runs *RunsReport `json:"runs,omitempty"`
}

// PutProviderAsset creates or replaces an asset with data, sent as is.
// With restart, the runs the new content leaves stale are restarted.
func (c *Client) PutProviderAsset(provider, name string, data []byte, restart bool) (ProviderAssetWrite, error) {
	const operation = "put provider asset"
	var out ProviderAssetWrite
	path := withQuery(apipath.ProviderAsset(provider, name), writeQuery(restart))
	req, err := c.newRequest(context.Background(), http.MethodPut, path,
		bytes.NewReader(data), "application/octet-stream", nil)
	if err != nil {
		return out, fmt.Errorf("%s: %w", operation, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("%s: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return out, readResponse(resp, &out, operation)
}

// DeleteProviderAsset removes an asset.
func (c *Client) DeleteProviderAsset(provider, name string) error {
	return c.doJSON(http.MethodDelete, apipath.ProviderAsset(provider, name), nil, nil, "delete provider asset")
}

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ProviderModelDownloadRequest selects one provider-owned destination on a node.
type ProviderModelDownloadRequest struct {
	Node     string `json:"node"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Name     string `json:"name,omitempty"`
}

// ProviderModelDownload identifies an accepted download and its owning node.
type ProviderModelDownload struct {
	Provider string `json:"provider"`
	Node     string `json:"node"`
	Category string `json:"category"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	Revision string `json:"revision"`
	Status   string `json:"status"`
	JobID    string `json:"job_id"`
}

// ProviderModelResolveRequest inspects workflow metadata and optionally downloads.
type ProviderModelResolveRequest struct {
	Node     string          `json:"node"`
	Workflow json.RawMessage `json:"workflow"`
	Download bool            `json:"download"`
}

// ListProviderModels lists the named node's files, optionally in one category.
func (c *Client) ListProviderModels(ctx context.Context, provider, node, category string) (map[string]any, error) {
	return providerModelsRequest[map[string]any](ctx, c, http.MethodGet, provider, "", url.Values{"node": {node}, "category": {category}}, nil)
}

// DownloadProviderModel starts a download through the coordinator.
func (c *Client) DownloadProviderModel(ctx context.Context, provider string, req ProviderModelDownloadRequest) (ProviderModelDownload, error) {
	return providerModelsRequest[ProviderModelDownload](ctx, c, http.MethodPost, provider, "", nil, req)
}

// DeleteProviderModel removes one regular file from the named node.
func (c *Client) DeleteProviderModel(ctx context.Context, provider, node, category, name string) (map[string]any, error) {
	return providerModelsRequest[map[string]any](ctx, c, http.MethodDelete, provider, "", url.Values{"node": {node}, "category": {category}, "name": {name}}, nil)
}

// ResolveProviderModels returns every workflow requirement, including failed starts.
func (c *Client) ResolveProviderModels(ctx context.Context, provider string, req ProviderModelResolveRequest) (map[string]any, error) {
	return providerModelsRequest[map[string]any](ctx, c, http.MethodPost, provider, "/resolve", nil, req)
}

func providerModelsRequest[T any](ctx context.Context, c *Client, method, provider, suffix string, query url.Values, body any) (T, error) {
	var out struct {
		Data T `json:"data"`
	}
	send := func(method, path string, body any) (*http.Response, error) {
		return c.makeContextRequest(ctx, method, path, body, nil)
	}
	err := c.doWith(send, method, withQuery(apipath.Provider(provider)+"/models"+suffix, query), body, &out, "provider models")
	return out.Data, err
}

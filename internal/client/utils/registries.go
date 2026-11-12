package client

import (
	"context"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// GetHuggingFaceVariants fetches grouped weights and declared companion features.
func (c *Client) GetHuggingFaceVariants(ctx context.Context, modelID, provider string) (*metadata.HuggingFaceVariantsResponse, error) {
	q := url.Values{"id": {modelID}}
	if provider != "" {
		q.Set("provider", provider)
	}
	var envelope struct {
		Data metadata.HuggingFaceVariantsResponse `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.RegistriesHuggingFaceVariants+"?"+q.Encode(), nil, &envelope, "get variants"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// GetOllamaTags fetches the server-owned Ollama catalog.
func (c *Client) GetOllamaTags(ctx context.Context, modelID string) (*metadata.OllamaTags, error) {
	var envelope struct {
		Data metadata.OllamaTags `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.RegistriesOllamaTags+"?id="+url.QueryEscape(modelID), nil, &envelope, "get Ollama tags"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

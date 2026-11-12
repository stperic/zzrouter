package client

import (
	"context"
	"net/url"
	"strconv"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// SearchModels uses the public search contract for every client presentation.
func (c *Client) SearchModels(ctx context.Context, query metadata.SearchQuery) (*metadata.SearchPage, error) {
	if query.Provider == "" {
		query.Provider = "all"
	}
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Sort == "" {
		query.Sort = "trendingScore"
	}
	if query.Order == "" {
		query.Order = "desc"
	}
	if query.Logic == "" {
		query.Logic = "AND"
	}
	q := url.Values{"type": {"models"}, "q": {query.Query}, "provider": {query.Provider}, "limit": {strconv.Itoa(query.Limit)}, "offset": {strconv.Itoa(query.Offset)}, "sort": {query.Sort}, "order": {query.Order}, "tags": {query.Tags}, "logic": {query.Logic}, "author": {query.Author}, "cursor": {query.Cursor}, "full": {strconv.FormatBool(query.Full)}, "enrich": {strconv.FormatBool(query.Enrich)}}
	var envelope struct {
		Data metadata.SearchPage `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.Search+"?"+q.Encode(), nil, &envelope, "search"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}

// GetSearchProviders fetches typed server discovery capabilities.
func (c *Client) GetSearchProviders(ctx context.Context) ([]metadata.SearchProvider, error) {
	var envelope struct {
		Data struct {
			Results []metadata.SearchProvider `json:"results"`
		} `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.Search+"?type=providers", nil, &envelope, "get search providers"); err != nil {
		return nil, err
	}
	return envelope.Data.Results, nil
}

// GetModelCard preserves native provider details and typed discovery metadata.
func (c *Client) GetModelCard(ctx context.Context, provider, modelID string) (*metadata.DiscoveryResult, error) {
	var envelope struct {
		Data metadata.DiscoveryResult `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.ModelCard(provider, modelID), nil, &envelope, "get model card"); err != nil {
		return nil, err
	}
	return &envelope.Data, nil
}
